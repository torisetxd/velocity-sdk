package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"velocitylabs.cc/sdk/internal/debugtrace"
)

const (
	DefaultAnthropicURL = "https://api.anthropic.com"
	// DefaultAnthropicMaxTokens applies when neither the request nor the
	// configured extra body gives an output limit, since Messages requires one.
	DefaultAnthropicMaxTokens = 32000
	// AnthropicReasoningFormat tags reasoning_details this provider produces, so
	// signed thinking blocks are only replayed to it.
	AnthropicReasoningFormat = "anthropic-claude-v1"

	anthropicVersion     = "2023-06-01"
	anthropicIdleTimeout = 5 * time.Minute
)

// AnthropicConfig configures a provider for the native Messages API.
//
// APIKey is sent as x-api-key. With TokenSource set, its token is sent as a
// bearer token instead.
type AnthropicConfig struct {
	APIURL      string
	APIKey      string
	ExtraBody   map[string]any
	TokenSource TokenSource
}

type anthropicProvider struct {
	httpClient  *http.Client
	baseURL     string
	apiKey      string
	tokenSource TokenSource
	extraBody   map[string]any
}

// NewAnthropicProvider speaks the Messages API directly, so prompt caching,
// signed thinking blocks and cache usage survive the trip that an
// OpenAI-compatible shim would drop.
func NewAnthropicProvider(cfg AnthropicConfig) *anthropicProvider {
	base := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	base = strings.TrimSuffix(base, "/messages")
	base = strings.TrimSuffix(base, "/v1")
	if base == "" {
		base = DefaultAnthropicURL
	}
	return &anthropicProvider{
		httpClient:  defaultHTTPClient(),
		baseURL:     base,
		apiKey:      strings.TrimSpace(cfg.APIKey),
		tokenSource: cfg.TokenSource,
		extraBody:   CopyMapAny(cfg.ExtraBody),
	}
}

func (p *anthropicProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:           true,
		SupportsReasoningDetails:    true,
		SupportsImages:              true,
		SupportsToolCalls:           true,
		SupportsPromptCache:         true,
		PromptCacheEnabledByDefault: true,
	}
}

func (p *anthropicProvider) setHeaders(ctx context.Context, req *http.Request) error {
	req.Header.Set("anthropic-version", anthropicVersion)
	if p.tokenSource != nil {
		token, err := p.tokenSource(ctx)
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}
	if p.apiKey != "" {
		req.Header.Set("x-api-key", p.apiKey)
	}
	return nil
}

func (p *anthropicProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	afterID := ""
	for page := 0; page < 20; page++ {
		target := p.baseURL + "/v1/models?limit=1000"
		if afterID != "" {
			target += "&after_id=" + url.QueryEscape(afterID)
		}
		var listing struct {
			Data []struct {
				ID             string `json:"id"`
				DisplayName    string `json:"display_name"`
				MaxInputTokens int    `json:"max_input_tokens"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := p.getJSON(ctx, target, &listing); err != nil {
			return nil, err
		}
		for _, m := range listing.Data {
			models = append(models, ModelInfo{ID: m.ID, Name: m.DisplayName, ContextLength: m.MaxInputTokens})
		}
		if !listing.HasMore || listing.LastID == "" {
			break
		}
		afterID = listing.LastID
	}
	return models, nil
}

func (p *anthropicProvider) getJSON(ctx context.Context, target string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := p.setHeaders(ctx, req); err != nil {
		return err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return classifyAnthropicHTTPError(resp, nil, raw)
	}
	return json.Unmarshal(raw, out)
}

func (p *anthropicProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	payload, err := BuildAnthropicBody(req, p.extraBody)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	streamID := debugtrace.NextID()
	debugtrace.Log("provider.request", map[string]any{
		"protocol":    "anthropic_messages",
		"stream_id":   streamID,
		"body_bytes":  len(body),
		"body_sha256": debugtrace.Digest(string(body)),
	})

	connCtx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(connCtx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if err := p.setHeaders(ctx, httpReq); err != nil {
		cancel()
		return nil, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		cancel()
		debugtrace.Log("provider.request_error", map[string]any{"protocol": "anthropic_messages", "stream_id": streamID, "error": err.Error()})
		return nil, err
	}
	debugtrace.Log("provider.response", map[string]any{
		"protocol":   "anthropic_messages",
		"stream_id":  streamID,
		"status":     resp.StatusCode,
		"request_id": resp.Header.Get("request-id"),
	})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer cancel()
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, classifyAnthropicHTTPError(resp, body, raw)
	}

	s := &anthropicStream{
		streamID:  streamID,
		body:      resp.Body,
		cancel:    cancel,
		toolIndex: map[int]int{},
		thinking:  map[int]*anthropicThinkingBlock{},
	}
	s.idleTimer = time.AfterFunc(anthropicIdleTimeout, func() {
		s.idleFired.Store(true)
		cancel()
	})
	s.reader = bufio.NewReaderSize(&idleReader{r: resp.Body, timer: s.idleTimer, d: anthropicIdleTimeout}, 64*1024)

	// Pull the first event so errors that arrive after the 200 surface here,
	// before the caller has seen anything, and it can still fall back.
	if err := s.advance(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func classifyAnthropicHTTPError(resp *http.Response, requestBody, rawBody []byte) error {
	message := strings.TrimSpace(string(rawBody))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &RateLimitError{StatusCode: resp.StatusCode, RetryAfter: retryAfter, Message: fmt.Sprintf("rate limit exceeded (429): %s", message)}
	case resp.StatusCode >= 500 || resp.StatusCode == 529:
		return &ServerError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("server error (%d): %s", resp.StatusCode, message)}
	}
	return formatStreamError(resp.Status, requestBody, rawBody)
}

// BuildAnthropicBody builds the Messages payload. Provider and request extra
// bodies are deep-merged over it, so max_tokens, thinking, output_config and
// the like are set from there.
func BuildAnthropicBody(req ChatRequest, providerExtra map[string]any) (map[string]any, error) {
	var system []map[string]any
	var messages []map[string]any
	explicitCache := false

	appendBlocks := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(messages); n > 0 && messages[n-1]["role"] == role {
			prev := messages[n-1]["content"].([]map[string]any)
			messages[n-1]["content"] = append(prev, blocks...)
			return
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			for _, b := range anthropicContent(msg.Content, &explicitCache) {
				if b["type"] == "text" {
					system = append(system, b)
				}
			}
		case "user":
			appendBlocks("user", anthropicContent(msg.Content, &explicitCache))
		case "assistant":
			blocks := anthropicThinking(msg)
			blocks = append(blocks, anthropicContent(msg.Content, &explicitCache)...)
			for _, call := range msg.ToolCalls {
				input := map[string]any{}
				if strings.TrimSpace(call.Function.Arguments) != "" {
					if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil || input == nil {
						input = map[string]any{}
					}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
			}
			appendBlocks("assistant", blocks)
		case "tool":
			result := map[string]any{"type": "tool_result", "tool_use_id": msg.ToolCallID}
			if content := anthropicContent(msg.Content, &explicitCache); len(content) > 0 {
				result["content"] = content
			} else {
				result["content"] = EmptyToolResponse
			}
			appendBlocks("user", []map[string]any{result})
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("anthropic request has no user or assistant messages")
	}

	body := map[string]any{"model": req.Model, "messages": messages, "stream": true}
	if len(system) > 0 {
		body["system"] = system
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Function.Name == "" {
				continue
			}
			schema := t.Function.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tool := map[string]any{"name": t.Function.Name, "input_schema": schema}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}
	switch strings.ToLower(strings.TrimSpace(req.ToolChoice)) {
	case "auto":
		body["tool_choice"] = map[string]any{"type": "auto"}
	case "none":
		body["tool_choice"] = map[string]any{"type": "none"}
	case "required", "any":
		body["tool_choice"] = map[string]any{"type": "any"}
	}
	if !explicitCache {
		body["cache_control"] = map[string]any{"type": "ephemeral"}
	}

	body = MergeMaps(body, MergeMaps(providerExtra, req.ExtraBody))
	body["model"] = req.Model
	body["messages"] = messages
	body["stream"] = true
	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = DefaultAnthropicMaxTokens
	}
	return body, nil
}

// anthropicThinking restores signed thinking blocks from reasoning_details, so
// tool loops with thinking keep their signatures across turns.
func anthropicThinking(msg ChatMessage) []map[string]any {
	raw, _ := msg.ReasoningMeta["reasoning_details"].([]any)
	var out []map[string]any
	for _, item := range raw {
		d, _ := item.(map[string]any)
		if d == nil {
			continue
		}
		if format, _ := d["format"].(string); format != "" && format != AnthropicReasoningFormat {
			continue
		}
		switch d["type"] {
		case "reasoning.text":
			sig, _ := d["signature"].(string)
			text, _ := d["text"].(string)
			if sig != "" {
				out = append(out, map[string]any{"type": "thinking", "thinking": text, "signature": sig})
			}
		case "reasoning.encrypted":
			if data, _ := d["data"].(string); data != "" {
				out = append(out, map[string]any{"type": "redacted_thinking", "data": data})
			}
		}
	}
	return out
}

func anthropicContent(content any, explicitCache *bool) []map[string]any {
	var parts []map[string]any
	switch c := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(c) == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": c}}
	case []any:
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				parts = append(parts, m)
			}
		}
	case []map[string]any:
		parts = c
	default:
		return []map[string]any{{"type": "text", "text": fmt.Sprint(c)}}
	}

	var out []map[string]any
	for _, part := range parts {
		var block map[string]any
		switch part["type"] {
		case "text", "input_text":
			text, _ := part["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			block = map[string]any{"type": "text", "text": text}
		case "image_url":
			imageURL := ""
			switch v := part["image_url"].(type) {
			case string:
				imageURL = v
			case map[string]any:
				imageURL, _ = v["url"].(string)
			}
			block = anthropicImage(imageURL)
		case "file":
			file, _ := part["file"].(map[string]any)
			data, _ := file["file_data"].(string)
			if mediaType, b64, ok := parseBase64DataURL(data); ok {
				block = map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": b64}}
			}
		case "image", "document", "tool_result", "tool_use", "thinking", "redacted_thinking", "search_result":
			block = CopyMapAny(part)
		}
		if block == nil {
			continue
		}
		if cc, ok := part["cache_control"]; ok {
			block["cache_control"] = cc
			*explicitCache = true
		}
		out = append(out, block)
	}
	return out
}

func anthropicImage(imageURL string) map[string]any {
	if mediaType, data, ok := parseBase64DataURL(imageURL); ok {
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}
	}
	if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": imageURL}}
	}
	return nil
}

func parseBase64DataURL(raw string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(raw, "data:")
	if !found {
		return "", "", false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found || !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	payload = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, payload)
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return "", "", false
	}
	return strings.TrimSuffix(meta, ";base64"), payload, true
}

type anthropicThinkingBlock struct {
	text      strings.Builder
	signature string
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStream struct {
	streamID  string
	body      io.Closer
	cancel    context.CancelFunc
	reader    *bufio.Reader
	idleTimer *time.Timer
	idleFired atomic.Bool

	pending   []StreamEvent
	toolIndex map[int]int
	nextTool  int
	thinking  map[int]*anthropicThinkingBlock
	usage     TokenUsage
	finish    string
	stopped   bool
	done      bool
}

func (s *anthropicStream) Close() error {
	s.idleTimer.Stop()
	s.cancel()
	return s.body.Close()
}

func (s *anthropicStream) Recv() (StreamEvent, error) {
	for len(s.pending) == 0 {
		if s.done {
			return StreamEvent{}, io.EOF
		}
		if err := s.advance(); err != nil {
			return StreamEvent{}, err
		}
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

// readData returns the next SSE data payload.
func (s *anthropicStream) readData() (string, error) {
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil && line == "" {
			if s.idleFired.Load() {
				return "", fmt.Errorf("anthropic stream idle for %s", anthropicIdleTimeout)
			}
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err != nil && len(data) > 0 {
			return strings.Join(data, "\n"), nil
		}
	}
}

// advance reads one SSE event and queues what it translates to.
func (s *anthropicStream) advance() error {
	payload, err := s.readData()
	if err != nil {
		if err == io.EOF {
			if !s.stopped {
				return io.ErrUnexpectedEOF
			}
			s.finishStream()
			return nil
		}
		return err
	}
	debugtrace.Raw("provider.sse_payload", map[string]any{"protocol": "anthropic_messages", "stream_id": s.streamID}, payload)

	var ev anthropicEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return fmt.Errorf("anthropic stream: invalid event: %w", err)
	}
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		s.usage.CachedTokens = u.CacheReadInputTokens
		s.usage.CacheWriteTokens = u.CacheCreationInputTokens
		s.usage.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		s.usage.OutputTokens = u.OutputTokens
		s.pending = append(s.pending, StreamEvent{Role: "assistant"})
	case "content_block_start":
		block := ev.ContentBlock
		switch block.Type {
		case "tool_use":
			idx := s.nextTool
			s.nextTool++
			s.toolIndex[ev.Index] = idx
			s.pending = append(s.pending, StreamEvent{ToolCallDeltas: []ToolCallDelta{{Index: idx, ID: block.ID, Type: "function", FunctionName: block.Name}}})
		case "thinking":
			tb := &anthropicThinkingBlock{signature: block.Signature}
			tb.text.WriteString(block.Thinking)
			s.thinking[ev.Index] = tb
			if block.Thinking != "" {
				s.pending = append(s.pending, StreamEvent{Reasoning: block.Thinking})
			}
		case "redacted_thinking":
			s.pending = append(s.pending, StreamEvent{ReasoningDetails: []map[string]any{{
				"type": "reasoning.encrypted", "data": block.Data, "format": AnthropicReasoningFormat, "index": ev.Index,
			}}})
		case "text":
			if block.Text != "" {
				s.pending = append(s.pending, StreamEvent{Content: block.Text})
			}
		}
	case "content_block_delta":
		d := ev.Delta
		switch d.Type {
		case "text_delta":
			s.pending = append(s.pending, StreamEvent{Content: d.Text})
		case "thinking_delta":
			if tb := s.thinking[ev.Index]; tb != nil {
				tb.text.WriteString(d.Thinking)
			}
			s.pending = append(s.pending, StreamEvent{Reasoning: d.Thinking})
		case "signature_delta":
			if tb := s.thinking[ev.Index]; tb != nil {
				tb.signature += d.Signature
			}
		case "input_json_delta":
			if idx, ok := s.toolIndex[ev.Index]; ok && d.PartialJSON != "" {
				s.pending = append(s.pending, StreamEvent{ToolCallDeltas: []ToolCallDelta{{Index: idx, FunctionArguments: d.PartialJSON}}})
			}
		}
	case "content_block_stop":
		if tb := s.thinking[ev.Index]; tb != nil {
			delete(s.thinking, ev.Index)
			s.pending = append(s.pending, StreamEvent{ReasoningDetails: []map[string]any{{
				"type": "reasoning.text", "text": tb.text.String(), "signature": tb.signature, "format": AnthropicReasoningFormat, "index": ev.Index,
			}}})
		}
	case "message_delta":
		s.finish = anthropicFinishReason(ev.Delta.StopReason)
		if ev.Usage.OutputTokens > 0 {
			s.usage.OutputTokens = ev.Usage.OutputTokens
		}
	case "message_stop":
		s.stopped = true
		s.finishStream()
	case "error":
		return anthropicStreamError(ev.Error.Type, ev.Error.Message)
	}
	return nil
}

func (s *anthropicStream) finishStream() {
	s.done = true
	usage := s.usage
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	s.pending = append(s.pending, StreamEvent{FinishReason: s.finish, Usage: &usage, TotalTokens: usage.TotalTokens})
}

func anthropicStreamError(kind, message string) error {
	msg := fmt.Sprintf("anthropic stream error (%s): %s", kind, message)
	switch kind {
	case "rate_limit_error":
		return &RateLimitError{StatusCode: http.StatusTooManyRequests, Message: msg}
	case "overloaded_error":
		return &ServerError{StatusCode: 529, Message: msg}
	case "api_error":
		return &ServerError{StatusCode: http.StatusInternalServerError, Message: msg}
	}
	return fmt.Errorf("%s", msg)
}

func anthropicFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return reason
	}
}
