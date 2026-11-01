package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultClaudeCommand is the Claude Code binary the Claude Agent provider runs
// when no command is configured.
const DefaultClaudeCommand = "claude"

// ClaudeAgentOptions configures a provider that runs the unmodified Claude Code
// binary in print mode and treats it as the model backend. Authentication is
// whatever that binary is signed into, so the host never sees Claude
// credentials.
type ClaudeAgentOptions struct {
	// Command is the Claude Code executable to run. Empty selects
	// DefaultClaudeCommand from PATH.
	Command string
}

type claudeAgentProvider struct {
	command string
}

// NewClaudeAgentProvider returns a provider backed by the Claude Code CLI.
func NewClaudeAgentProvider(options ClaudeAgentOptions) Provider {
	command := strings.TrimSpace(options.Command)
	if command == "" {
		command = DefaultClaudeCommand
	}
	return &claudeAgentProvider{command: command}
}

func (p *claudeAgentProvider) Capabilities() Capabilities {
	return Capabilities{SupportsReasoning: true, SupportsToolCalls: true}
}

func (p *claudeAgentProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	systemPrompt, prompt := claudeAgentPrompt(req.Messages)
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("claude agent: prompt is empty")
	}

	bridge, hasBridge := AgentToolBridgeFromContext(ctx)
	var definitions []ToolDef
	if hasBridge && bridge.Definitions != nil {
		definitions = bridge.Definitions(ctx)
	}

	runCtx, cancel := context.WithCancel(ctx)
	stream := &claudeAgentStream{events: make(chan StreamEvent, 64), cancel: cancel}

	if len(definitions) > 0 && bridge.Execute != nil {
		tools, err := startClaudeAgentTools(runCtx, definitions, bridge.Execute)
		if err != nil {
			cancel()
			return nil, err
		}
		stream.tools = tools
	}

	args, systemFile, err := claudeAgentArgs(req, systemPrompt, stream.tools, definitions)
	if err != nil {
		stream.Close()
		return nil, err
	}
	stream.systemFile = systemFile

	cmd := exec.Command(p.command, args...)
	cmd.Dir, err = os.Getwd()
	if err != nil {
		stream.Close()
		return nil, err
	}
	cmd.Env = claudeAgentEnv()
	stderr := &boundedWriter{limit: 8 << 10}
	cmd.Stderr = stderr
	stream.stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		stream.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stream.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stream.Close()
		return nil, fmt.Errorf("claude agent: start %s: %w", p.command, err)
	}
	stream.cmd = cmd

	go func() {
		_, _ = io.WriteString(stdin, prompt)
		_ = stdin.Close()
	}()
	go stream.pump(runCtx, stdout)
	return stream, nil
}

// claudeAgentArgs builds the print-mode invocation. Built-in Claude Code tools,
// settings, hooks and foreign MCP servers are all switched off, so the only
// tools the model can reach are the host's, served over the local MCP bridge.
func claudeAgentArgs(req ChatRequest, systemPrompt string, tools *claudeAgentTools, definitions []ToolDef) ([]string, string, error) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--no-session-persistence",
		"--setting-sources", "",
		"--tools", "",
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		args = append(args, "--model", model)
	}

	systemFile := ""
	if strings.TrimSpace(systemPrompt) != "" {
		dir, err := os.MkdirTemp("", "velocity-claude-")
		if err != nil {
			return nil, "", fmt.Errorf("claude agent: %w", err)
		}
		systemFile = filepath.Join(dir, "system-prompt.txt")
		if err := os.WriteFile(systemFile, []byte(systemPrompt), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, "", fmt.Errorf("claude agent: %w", err)
		}
		args = append(args, "--system-prompt-file", systemFile)
	}

	if tools != nil {
		config, err := tools.Config()
		if err != nil {
			return nil, systemFile, err
		}
		args = append(args, "--strict-mcp-config", "--mcp-config", config)
		allowed := make([]string, 0, len(definitions))
		for _, def := range definitions {
			allowed = append(allowed, tools.ToolName(def.Function.Name))
		}
		if len(allowed) > 0 {
			args = append(args, "--allowedTools", strings.Join(allowed, ","))
		}
	}
	return args, systemFile, nil
}

// claudeAgentEnv drops the variables Claude Code uses to detect that it is
// already running inside another Claude Code session.
func claudeAgentEnv() []string {
	blocked := map[string]bool{
		"CLAUDECODE":               true,
		"CLAUDE_CODE_ENTRYPOINT":   true,
		"CLAUDE_CODE_SSE_PORT":     true,
		"CLAUDE_CODE_SESSION_ID":   true,
		"CLAUDE_CODE_PARENT_AGENT": true,
	}
	environ := os.Environ()
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if blocked[name] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// claudeAgentPrompt splits the chat history into a system prompt and a single
// print-mode prompt. The trailing user turn becomes the prompt itself; earlier
// turns are replayed as a transcript because every run starts a fresh session.
func claudeAgentPrompt(messages []ChatMessage) (string, string) {
	system := make([]string, 0, 1)
	history := make([]string, 0, len(messages))
	pendingUser := ""

	flushUser := func() {
		if pendingUser != "" {
			history = append(history, "USER:\n"+pendingUser)
			pendingUser = ""
		}
	}

	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		text := strings.TrimSpace(claudeAgentContent(message.Content))
		switch role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			flushUser()
			pendingUser = text
		default:
			if text == "" {
				continue
			}
			flushUser()
			history = append(history, strings.ToUpper(role)+":\n"+text)
		}
	}

	if pendingUser != "" {
		if len(history) == 0 {
			return strings.Join(system, "\n\n"), pendingUser
		}
		history = append(history, "USER:\n"+pendingUser)
	}

	transcript := strings.Join(history, "\n\n")
	if transcript == "" {
		return strings.Join(system, "\n\n"), ""
	}
	return strings.Join(system, "\n\n"), "Conversation so far:\n\n" + transcript
}

func claudeAgentContent(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []ChatMessage:
		parts := make([]string, 0, len(value))
		for _, message := range value {
			if text := claudeAgentContent(message.Content); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case []any:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			if text := claudeAgentBlock(block); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func claudeAgentBlock(block any) string {
	if text, ok := block.(string); ok {
		return text
	}
	fields, ok := block.(map[string]any)
	if !ok {
		return ""
	}
	kind, _ := fields["type"].(string)
	switch kind {
	case "", "text", "input_text", "output_text":
		text, _ := fields["text"].(string)
		return text
	case "image", "image_url", "input_image":
		return "[image attached]"
	default:
		return "[" + kind + " omitted]"
	}
}

type claudeAgentStream struct {
	events     chan StreamEvent
	cmd        *exec.Cmd
	tools      *claudeAgentTools
	cancel     context.CancelFunc
	systemFile string
	stderr     *boundedWriter

	closeOnce sync.Once
	mu        sync.Mutex
	err       error
}

func (s *claudeAgentStream) Recv() (StreamEvent, error) {
	event, ok := <-s.events
	if !ok {
		return StreamEvent{}, s.failure()
	}
	return event, nil
}

func (s *claudeAgentStream) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			_ = s.cmd.Wait()
		}
		if s.tools != nil {
			_ = s.tools.Close()
		}
		if s.systemFile != "" {
			_ = os.RemoveAll(filepath.Dir(s.systemFile))
		}
	})
	return nil
}

func (s *claudeAgentStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return io.EOF
}

func (s *claudeAgentStream) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *claudeAgentStream) pump(ctx context.Context, stdout io.Reader) {
	defer close(s.events)
	defer s.Close()

	decoder := &claudeAgentDecoder{stream: s, ctx: ctx}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		decoder.decodeLine(scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			s.setError(fmt.Errorf("claude agent: read output: %w", err))
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	waitErr := s.cmd.Wait()
	if decoder.sawResult {
		return
	}
	if detail := strings.TrimSpace(s.stderr.String()); detail != "" {
		s.setError(fmt.Errorf("claude agent: %s", detail))
		return
	}
	if waitErr != nil {
		s.setError(fmt.Errorf("claude agent: %w", waitErr))
		return
	}
	s.setError(errors.New("claude agent: the Claude Code CLI produced no result"))
}

type boundedWriter struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if remaining := w.limit - len(w.data); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		w.data = append(w.data, p[:remaining]...)
	}
	return len(p), nil
}

func (w *boundedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.data)
}
