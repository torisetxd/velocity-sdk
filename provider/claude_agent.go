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

	var transcript *claudeTranscript
	if path := claudeTranscriptFromContext(ctx); path != "" {
		system, prior, tail := claudeAgentSplit(req.Messages)
		if strings.TrimSpace(tail) == "" {
			return nil, fmt.Errorf("claude agent: prompt is empty")
		}
		systemPrompt, prompt = system, tail
		var err error
		transcript, err = openClaudeTranscript(path, prior)
		if err != nil {
			return nil, err
		}
		if !transcript.resumed {
			if err := transcript.seed(prior); err != nil {
				return nil, err
			}
		}
		// The prompt itself never comes back on the stream, so record it here and
		// let the run append the assistant side.
		if err := transcript.appendPrompt(prompt); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("claude agent: prompt is empty")
	}

	bridge, hasBridge := AgentToolBridgeFromContext(ctx)
	var definitions []ToolDef
	if hasBridge && bridge.Definitions != nil {
		definitions = bridge.Definitions(ctx)
	}

	runCtx, cancel := context.WithCancel(ctx)
	stream := &claudeAgentStream{events: make(chan StreamEvent, 64), cancel: cancel, transcript: transcript}

	if len(definitions) > 0 && bridge.Execute != nil {
		tools, err := startClaudeAgentTools(runCtx, definitions, bridge.Execute)
		if err != nil {
			cancel()
			return nil, err
		}
		stream.tools = tools
	}

	args, systemFile, err := claudeAgentArgs(req, systemPrompt, stream.tools, definitions, transcript)
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
func claudeAgentArgs(req ChatRequest, systemPrompt string, tools *claudeAgentTools, definitions []ToolDef, transcript *claudeTranscript) ([]string, string, error) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--setting-sources", "",
		"--tools", "",
	}
	if transcript == nil {
		args = append(args, "--no-session-persistence")
	} else if transcript.resumed {
		args = append(args, "--resume", transcript.path)
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := claudeAgentEffort(req.ExtraBody); effort != "" {
		args = append(args, "--effort", effort)
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

// claudeAgentEffort reads the reasoning effort from the request body. Claude
// Code takes it as a session flag, and every run here is a fresh session, so a
// host can switch effort between turns without disturbing anything cached.
func claudeAgentEffort(extraBody map[string]any) string {
	effort, _ := extraBody["effort"].(string)
	return strings.TrimSpace(effort)
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

// claudeAgentSplit separates the system prompt, every turn before the trailing
// user message, and that trailing message. Without a stored Claude Code session
// the earlier turns are replayed as text; with one they are already on disk.
func claudeAgentSplit(messages []ChatMessage) (string, []ChatMessage, string) {
	system := make([]string, 0, 1)
	turns := make([]ChatMessage, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer":
			if text := strings.TrimSpace(claudeAgentContent(message.Content)); text != "" {
				system = append(system, text)
			}
		default:
			turns = append(turns, message)
		}
	}

	prior, tail := turns, ""
	if count := len(turns); count > 0 && strings.EqualFold(strings.TrimSpace(turns[count-1].Role), "user") {
		tail = strings.TrimSpace(claudeAgentContent(turns[count-1].Content))
		prior = turns[:count-1]
	}
	return strings.Join(system, "\n\n"), prior, tail
}

// claudeAgentPrompt renders the whole conversation as one print-mode prompt for
// runs that keep no Claude Code session on disk.
func claudeAgentPrompt(messages []ChatMessage) (string, string) {
	system, prior, tail := claudeAgentSplit(messages)

	blocks := make([]string, 0, len(prior))
	for _, message := range prior {
		role := strings.ToUpper(strings.TrimSpace(message.Role))
		text := strings.TrimSpace(claudeAgentContent(message.Content))
		if role == "" || text == "" {
			continue
		}
		blocks = append(blocks, role+":\n"+text)
	}

	transcript := strings.Join(blocks, "\n\n")
	switch {
	case transcript == "":
		return system, tail
	case tail == "":
		return system, "Conversation so far:\n\n" + transcript
	default:
		return system, "Conversation so far:\n\n" + transcript + "\n\nUSER:\n" + tail
	}
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
	transcript *claudeTranscript
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

	decoder := &claudeAgentDecoder{stream: s, ctx: ctx, transcript: s.transcript}
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
	if s.transcript != nil {
		if err := s.transcript.flush(); err != nil && decoder.sawResult {
			s.setError(err)
		}
	}
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
