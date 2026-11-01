package provider

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// claudeAgentServerName is the MCP server name the bridged tools are published
// under, so a call reaches Claude Code as mcp__host__<tool>.
const claudeAgentServerName = "host"

// AgentToolBridge connects a provider that runs an external agent harness to the
// host's tool implementations. Definitions is called once per run to decide
// which tools the harness may call; Execute runs a single call and returns its
// result, which is sent back to the model as tool output.
type AgentToolBridge struct {
	Definitions func(ctx context.Context) []ToolDef
	Execute     func(ctx context.Context, call ToolCall) any
}

type agentToolBridgeKey struct{}

// WithAgentToolBridge attaches a tool bridge to a run context.
func WithAgentToolBridge(ctx context.Context, bridge AgentToolBridge) context.Context {
	return context.WithValue(ctx, agentToolBridgeKey{}, bridge)
}

// AgentToolBridgeFromContext returns the tool bridge attached to ctx.
func AgentToolBridgeFromContext(ctx context.Context) (AgentToolBridge, bool) {
	bridge, ok := ctx.Value(agentToolBridgeKey{}).(AgentToolBridge)
	return bridge, ok
}

// claudeAgentTools serves the host's tools to the Claude Code CLI over a
// loopback MCP endpoint for the lifetime of a single run.
type claudeAgentTools struct {
	ctx      context.Context
	defs     []ToolDef
	byName   map[string]ToolDef
	execute  func(context.Context, ToolCall) any
	server   *http.Server
	listener net.Listener
	token    string
	url      string
}

func startClaudeAgentTools(ctx context.Context, defs []ToolDef, execute func(context.Context, ToolCall) any) (*claudeAgentTools, error) {
	byName := make(map[string]ToolDef, len(defs))
	unique := make([]ToolDef, 0, len(defs))
	for _, def := range defs {
		name := strings.TrimSpace(def.Function.Name)
		if name == "" {
			continue
		}
		if _, exists := byName[name]; exists {
			continue
		}
		byName[name] = def
		unique = append(unique, def)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claude agent: start tool bridge: %w", err)
	}
	token, err := claudeAgentToken()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("claude agent: %w", err)
	}

	tools := &claudeAgentTools{
		ctx:      ctx,
		defs:     unique,
		byName:   byName,
		execute:  execute,
		listener: listener,
		token:    token,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", tools.handle)
	tools.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	tools.url = "http://" + listener.Addr().String() + "/mcp"
	go func() { _ = tools.server.Serve(listener) }()
	return tools, nil
}

func claudeAgentToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// ToolName is the name Claude Code sees for a host tool.
func (t *claudeAgentTools) ToolName(name string) string {
	return "mcp__" + claudeAgentServerName + "__" + name
}

// Config renders the --mcp-config payload that points the CLI at this bridge.
func (t *claudeAgentTools) Config() (string, error) {
	payload := map[string]any{
		"mcpServers": map[string]any{
			claudeAgentServerName: map[string]any{
				"type":    "http",
				"url":     t.url,
				"headers": map[string]any{"Authorization": "Bearer " + t.token},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("claude agent: encode tool bridge config: %w", err)
	}
	return string(encoded), nil
}

func (t *claudeAgentTools) Close() error {
	if t == nil || t.server == nil {
		return nil
	}
	t.server.Close()
	return nil
}

type claudeAgentRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (t *claudeAgentTools) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	authorization := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(authorization), []byte("Bearer "+t.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var request claudeAgentRPC
	if err := json.Unmarshal(body, &request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch request.Method {
	case "initialize":
		t.writeResult(w, request.ID, t.initializeResult(request.Params))
	case "tools/list":
		t.writeResult(w, request.ID, t.listResult())
	case "tools/call":
		t.writeResult(w, request.ID, t.callResult(request.Params))
	case "ping":
		t.writeResult(w, request.ID, map[string]any{})
	default:
		t.writeError(w, request.ID, -32601, "method not found: "+request.Method)
	}
}

func (t *claudeAgentTools) initializeResult(params json.RawMessage) map[string]any {
	version := "2025-06-18"
	var payload struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &payload); err == nil {
		switch payload.ProtocolVersion {
		case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
			version = payload.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": claudeAgentServerName, "version": "1.0.0"},
	}
}

func (t *claudeAgentTools) listResult() map[string]any {
	listed := make([]map[string]any, 0, len(t.defs))
	for _, def := range t.defs {
		entry := map[string]any{"name": def.Function.Name}
		if description := strings.TrimSpace(def.Function.Description); description != "" {
			entry["description"] = description
		}
		if def.Function.Parameters != nil {
			entry["inputSchema"] = def.Function.Parameters
		} else {
			entry["inputSchema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		listed = append(listed, entry)
	}
	return map[string]any{"tools": listed}
}

func (t *claudeAgentTools) callResult(params json.RawMessage) map[string]any {
	var payload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return claudeAgentToolText("tool error: invalid tool call payload", true)
	}
	def, ok := t.byName[payload.Name]
	if !ok {
		return claudeAgentToolText("tool error: unknown tool "+payload.Name, true)
	}
	arguments := strings.TrimSpace(string(payload.Arguments))
	if arguments == "" || arguments == "null" {
		arguments = "{}"
	}
	output := t.run(def.Function.Name, arguments)
	return claudeAgentToolText(output, strings.HasPrefix(strings.TrimSpace(output), "tool error:"))
}

func (t *claudeAgentTools) run(name, arguments string) (output string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			output = fmt.Sprintf("tool error: %s panicked: %v", name, recovered)
		}
	}()
	if err := t.ctx.Err(); err != nil {
		return "tool error: run cancelled before the tool executed"
	}
	result := t.execute(t.ctx, ToolCall{
		ID:   "claude-agent-" + name,
		Type: "function",
		Function: ToolFunction{
			Name:      name,
			Arguments: arguments,
		},
	})
	return claudeAgentToolOutput(result)
}

func claudeAgentToolOutput(result any) string {
	switch value := result.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func claudeAgentToolText(text string, isError bool) map[string]any {
	result := map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
	if isError {
		result["isError"] = true
	}
	return result
}

func (t *claudeAgentTools) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (t *claudeAgentTools) writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}
