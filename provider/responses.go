package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"velocitylabs.cc/sdk/internal/debugtrace"
)

const defaultResponsesAPIURL = "https://api.openai.com/v1/responses"

type responsesProvider struct {
	httpClient  *http.Client
	apiURL      string
	apiKey      string
	tokenSource TokenSource
	extraBody   map[string]any
}

func NewResponsesProvider(apiURL, apiKey string, extraBody map[string]any) *responsesProvider {
	url := strings.TrimSpace(apiURL)
	if url == "" {
		url = defaultResponsesAPIURL
	}
	httpClient := defaultHTTPClient()
	preconnectHTTPClient(httpClient, url, apiKey)

	return &responsesProvider{
		httpClient: httpClient,
		apiURL:     url,
		apiKey:     apiKey,
		extraBody:  CopyMapAny(extraBody),
	}
}

func (p *responsesProvider) SetTokenSource(ts TokenSource) {
	p.tokenSource = ts
}

func (p *responsesProvider) resolveToken(ctx context.Context) (string, error) {
	if p.tokenSource != nil {
		return p.tokenSource(ctx)
	}
	return p.apiKey, nil
}

func (p *responsesProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
		SupportsPromptCache:      true,
	}
}

func (p *responsesProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	token, err := p.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	return FetchModelsFromEndpoint(ctx, token, p.apiURL, nil)
}

type responsesRequest struct {
	Model      string             `json:"model"`
	Input      []any              `json:"input"`
	Tools      []responsesToolDef `json:"tools,omitempty"`
	ToolChoice string             `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream"`
	Truncation string             `json:"truncation,omitempty"`
}

type responsesToolDef struct {
	Type        string      `json:"type"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
	Strict      bool        `json:"strict,omitempty"`
}

type inputItem struct {
	Role      string      `json:"role,omitempty"`
	Type      string      `json:"type,omitempty"`
	ID        string      `json:"id,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	CallID    string      `json:"call_id,omitempty"`
	Output    interface{} `json:"output,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
}

func BuildResponsesBody(req ChatRequest, opts OpenAIRequestOptions) ([]byte, error) {
	input := convertMessagesToResponseInput(req.Messages, opts.ResponsesOutputMetaKey)
	tools := convertToolsToResponsesFormat(req.Tools)

	payload := responsesRequest{
		Model:      req.Model,
		Input:      input,
		Tools:      tools,
		ToolChoice: strings.TrimSpace(req.ToolChoice),
		Stream:     true,
		Truncation: "disabled",
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	mergedExtra := MergeMaps(opts.ProviderExtraBody, opts.RequestExtraBody)
	if req.PromptCacheKey != "" {
		if mergedExtra == nil {
			mergedExtra = map[string]any{}
		}
		mergedExtra["prompt_cache_key"] = req.PromptCacheKey
	}
	if len(mergedExtra) == 0 {
		return b, nil
	}

	var asMap map[string]any
	if err := json.Unmarshal(b, &asMap); err != nil {
		return nil, fmt.Errorf("decode request payload: %w", err)
	}
	for key, value := range mergedExtra {
		asMap[key] = value
	}
	return json.Marshal(asMap)
}

func convertToolsToResponsesFormat(tools []ToolDef) []responsesToolDef {
	if len(tools) == 0 {
		return nil
	}

	result := make([]responsesToolDef, 0, len(tools))
	for _, tool := range tools {
		rt := responsesToolDef{
			Type:        "function",
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		}
		result = append(result, rt)
	}
	return result
}

func convertMessagesToResponseInput(messages []ChatMessage, outputMetaKey string) []any {
	items := make([]any, 0, len(messages))

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}

		switch role {
		case "system", "developer":
			items = append(items, inputItem{
				Type:    "message",
				Role:    role,
				Content: normalizeContentForInput(msg.Content),
			})
		case "user":
			items = append(items, inputItem{
				Type:    "message",
				Role:    "user",
				Content: normalizeContentForInput(msg.Content),
			})
		case "assistant":
			if output := responseOutputItems(msg.ProviderMeta, outputMetaKey); len(output) > 0 {
				items = append(items, output...)
				continue
			}
			if content := normalizeContentForInput(msg.Content); content != nil {
				items = append(items, inputItem{
					Type:    "message",
					Role:    "assistant",
					Content: content,
				})
			}
			for _, toolCall := range msg.ToolCalls {
				callID := strings.TrimSpace(toolCall.ID)
				if callID == "" {
					callID = "call_" + generateShortID()
				}
				items = append(items, inputItem{
					Type:      "function_call",
					CallID:    callID,
					Name:      toolCall.Function.Name,
					Arguments: toolCall.Function.Arguments,
				})
			}
		case "tool":
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				callID = "call_" + generateShortID()
			}
			items = append(items, inputItem{
				Type:   "function_call_output",
				CallID: callID,
				Output: normalizeToolOutput(msg.Content),
			})
		}
	}

	return items
}

func responseOutputItems(meta map[string]any, key string) []any {
	if len(meta) == 0 || strings.TrimSpace(key) == "" {
		return nil
	}
	raw, ok := meta[key]
	if !ok {
		return nil
	}
	var values []any
	switch output := raw.(type) {
	case []any:
		values = output
	case []map[string]any:
		values = make([]any, len(output))
		for i := range output {
			values[i] = output[i]
		}
	default:
		return nil
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(item["type"])) == "" {
			continue
		}
		items = append(items, CopyMapAny(item))
	}
	return items
}

func normalizeContentForInput(content any) interface{} {
	switch c := content.(type) {
	case nil:
		return nil
	case string:
		text := strings.TrimSpace(c)
		if text == "" {
			return nil
		}
		return text
	case []map[string]any:
		if len(c) == 0 {
			return nil
		}
		parts := make([]map[string]any, 0, len(c))
		for _, part := range c {
			normalized := normalizeContentPartForInput(part)
			if normalized != nil {
				parts = append(parts, normalized)
			}
		}
		return parts
	case []any:
		if len(c) == 0 {
			return nil
		}
		parts := make([]map[string]any, 0, len(c))
		for _, rawPart := range c {
			if partMap, ok := rawPart.(map[string]any); ok {
				normalized := normalizeContentPartForInput(partMap)
				if normalized != nil {
					parts = append(parts, normalized)
				}
			} else {
				text := strings.TrimSpace(fmt.Sprint(rawPart))
				if text != "" {
					parts = append(parts, map[string]any{
						"type": "input_text",
						"text": text,
					})
				}
			}
		}
		return parts
	default:
		text := strings.TrimSpace(fmt.Sprint(c))
		if text == "" {
			return nil
		}
		return text
	}
}

func normalizeContentPartForInput(part map[string]any) map[string]any {
	if len(part) == 0 {
		return nil
	}

	partType, _ := part["type"].(string)
	text, _ := part["text"].(string)

	if partType == "text" || partType == "input_text" || partType == "output_text" {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return map[string]any{
			"type": "input_text",
			"text": text,
		}
	}

	if partType == "function_call" {
		return CopyMapAny(part)
	}

	if partType == "image_url" {
		imageURL := part["image_url"]
		switch v := imageURL.(type) {
		case string:
			return map[string]any{
				"type":      "input_image",
				"image_url": v,
			}
		case map[string]any:
			url, _ := v["url"].(string)
			detail, _ := v["detail"].(string)
			if url != "" {
				result := map[string]any{
					"type":      "input_image",
					"image_url": url,
				}
				if detail != "" {
					result["detail"] = detail
				}
				return result
			}
		}
		return nil
	}

	if partType == "input_image" {
		return CopyMapAny(part)
	}

	if partType == "input_file" {
		return CopyMapAny(part)
	}

	text = strings.TrimSpace(fmt.Sprint(part))
	if text != "" && text != "map[]" {
		return map[string]any{
			"type": "input_text",
			"text": text,
		}
	}

	return nil
}

func normalizeToolOutput(content any) any {
	switch c := content.(type) {
	case nil:
		return EmptyToolResponse
	case string:
		return NormalizeToolResponse(c)
	case []map[string]any:
		if len(c) == 0 {
			return EmptyToolResponse
		}
		parts := make([]map[string]any, 0, len(c))
		for _, part := range c {
			if normalized := normalizeContentPartForInput(part); normalized != nil {
				parts = append(parts, normalized)
			}
		}
		if len(parts) > 0 {
			return parts
		}
		return EmptyToolResponse
	default:
		b, err := json.Marshal(c)
		if err == nil {
			return string(b)
		}
		return fmt.Sprint(c)
	}
}

var idCounter int

func generateShortID() string {
	idCounter++
	return fmt.Sprintf("%d", idCounter)
}

type responsesStream struct {
	ctx               context.Context
	resp              *http.Response
	lineCh            chan sseLine
	closeOnce         sync.Once
	readerDone        chan struct{}
	stopCh            chan struct{}
	outputMetaKey     string
	outputItems       []map[string]any
	reasoningStreamed bool
	streamID          string
	requireCompletion bool
	completed         bool
	accessToken       string

	toolCallArgs map[string]string
	toolCallInfo map[string]struct {
		Index int
		Name  string
		ID    string
	}
}

func streamResponses(
	ctx context.Context,
	httpClient *http.Client,
	apiURL string,
	apiKey string,
	body []byte,
	extraHeaders map[string]string,
	outputMetaKey string,
) (ChatStream, error) {
	endpoint := ProviderEndpoint(apiURL, "responses")
	streamID := debugtrace.NextID()
	debugtrace.Log("provider.request", map[string]any{
		"protocol":     "responses",
		"stream_id":    streamID,
		"body_bytes":   len(body),
		"body_sha256":  debugtrace.Digest(string(body)),
		"header_count": len(extraHeaders),
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		debugtrace.Log("provider.request_error", map[string]any{
			"protocol":  "responses",
			"stream_id": streamID,
			"error":     err.Error(),
		})
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for key, value := range extraHeaders {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			httpReq.Header.Set(key, value)
		}
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		debugtrace.Log("provider.request_error", map[string]any{
			"protocol":  "responses",
			"stream_id": streamID,
			"error":     err.Error(),
		})
		return nil, err
	}
	debugtrace.Log("provider.response", map[string]any{
		"protocol":     "responses",
		"stream_id":    streamID,
		"status":       resp.StatusCode,
		"content_type": resp.Header.Get("Content-Type"),
		"request_id":   resp.Header.Get("X-Request-ID"),
	})

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		debugtrace.Log("provider.http_error", map[string]any{
			"protocol":    "responses",
			"stream_id":   streamID,
			"status":      resp.StatusCode,
			"body_bytes":  len(rawBody),
			"body_sha256": debugtrace.Digest(string(rawBody)),
		})

		if resp.StatusCode == 429 {
			retryAfter := 0
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				fmt.Sscanf(ra, "%d", &retryAfter)
			}
			return nil, &RateLimitError{
				StatusCode: 429,
				RetryAfter: retryAfter,
				Message:    fmt.Sprintf("rate limit exceeded (429): %s", strings.TrimSpace(string(rawBody))),
			}
		}

		if resp.StatusCode >= 500 {
			return nil, &ServerError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("server error (%d): %s", resp.StatusCode, strings.TrimSpace(string(rawBody))),
			}
		}

		return nil, formatStreamError(resp.Status, body, rawBody)
	}

	s := &responsesStream{
		ctx:           ctx,
		resp:          resp,
		lineCh:        make(chan sseLine, 64),
		readerDone:    make(chan struct{}),
		stopCh:        make(chan struct{}),
		outputMetaKey: outputMetaKey,
		streamID:      streamID,
		accessToken:   apiKey,
		toolCallArgs:  make(map[string]string),
		toolCallInfo: make(map[string]struct {
			Index int
			Name  string
			ID    string
		}),
	}

	go s.readLoop()
	return s, nil
}

func (s *responsesStream) readLoop() {
	defer close(s.readerDone)
	defer close(s.lineCh)
	reader := bufio.NewReaderSize(s.resp.Body, 64*1024)
	for {
		lineRaw, err := reader.ReadString('\n')
		if err != nil {
			select {
			case s.lineCh <- sseLine{line: lineRaw, err: err}:
			case <-s.stopCh:
			}
			return
		}
		select {
		case s.lineCh <- sseLine{line: lineRaw}:
		case <-s.stopCh:
			return
		}
	}
}

func (s *responsesStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.stopCh)
		if s.resp != nil && s.resp.Body != nil {
			_ = s.resp.Body.Close()
		}
	})
	<-s.readerDone
	return nil
}

func (s *responsesStream) Recv() (StreamEvent, error) {
	for {
		payload, err := s.readSSEPayload()
		if err != nil {
			if errors.Is(err, io.EOF) {
				debugtrace.Log("provider.stream_end", map[string]any{
					"protocol":  "responses",
					"stream_id": s.streamID,
					"reason":    "eof",
				})
				return s.handleFinalEvent()
			}
			debugtrace.Log("provider.stream_error", map[string]any{
				"protocol":  "responses",
				"stream_id": s.streamID,
				"error":     err.Error(),
			})
			return StreamEvent{}, err
		}

		payload = strings.TrimSpace(payload)
		if s.requireCompletion && s.accessToken != "" {
			payload = strings.ReplaceAll(payload, s.accessToken, "[REDACTED]")
		}
		debugtrace.Log("provider.sse_payload", map[string]any{
			"protocol":       "responses",
			"stream_id":      s.streamID,
			"payload_bytes":  len(payload),
			"payload_sha256": debugtrace.Digest(payload),
		})
		if !s.requireCompletion {
			debugtrace.Raw("provider.sse_payload", map[string]any{
				"protocol":  "responses",
				"stream_id": s.streamID,
			}, payload)
		}
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			debugtrace.Log("provider.stream_end", map[string]any{
				"protocol":  "responses",
				"stream_id": s.streamID,
				"reason":    "done_marker",
			})
			return s.handleFinalEvent()
		}

		if streamErr, isErrorEvent := parseResponsesStreamError(payload); isErrorEvent {
			debugtrace.Log("provider.responses_stream_error", map[string]any{
				"protocol":  "responses",
				"stream_id": s.streamID,
				"error":     streamErr.Error(),
			})
			return StreamEvent{}, streamErr
		}

		ev := s.parseEvent(payload)
		if ev == nil {
			continue
		}

		return *ev, nil
	}
}

func (s *responsesStream) readSSEPayload() (string, error) {
	dataLines := make([]string, 0, 1)

	for {
		select {
		case <-s.ctx.Done():
			s.closeOnce.Do(func() {
				close(s.stopCh)
				if s.resp != nil && s.resp.Body != nil {
					_ = s.resp.Body.Close()
				}
			})
			return "", s.ctx.Err()
		case <-s.stopCh:
			return "", io.EOF
		case sl, ok := <-s.lineCh:
			if !ok {
				return "", io.EOF
			}
			if sl.err != nil {
				if errors.Is(sl.err, io.EOF) {
					if strings.HasPrefix(sl.line, "data:") {
						dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(sl.line, "\r\n"), "data:"), " "))
					}
					if len(dataLines) > 0 {
						return strings.Join(dataLines, "\n"), nil
					}
					return "", io.EOF
				}
				return "", sl.err
			}

			line := strings.TrimRight(sl.line, "\r\n")

			switch {
			case line == "":
				if len(dataLines) > 0 {
					return strings.Join(dataLines, "\n"), nil
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimPrefix(line, "data:")
				data = strings.TrimPrefix(data, " ")
				dataLines = append(dataLines, data)
			}
		}
	}
}

func (s *responsesStream) handleFinalEvent() (StreamEvent, error) {
	if s.requireCompletion && !s.completed {
		statusCode := http.StatusInternalServerError
		if s.resp != nil {
			statusCode = s.resp.StatusCode
		}
		return StreamEvent{}, &ServerError{
			StatusCode: statusCode,
			Message:    fmt.Sprintf("Responses inference interrupted without response.completed (HTTP %d)", statusCode),
		}
	}
	return StreamEvent{}, io.EOF
}

func (s *responsesStream) parseEvent(payload string) *StreamEvent {
	var raw struct {
		Type string `json:"type"`
	}

	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		debugtrace.Log("provider.parse_error", map[string]any{
			"protocol":      "responses",
			"stream_id":     s.streamID,
			"payload_bytes": len(payload),
			"error":         err.Error(),
		})
		return nil
	}
	debugtrace.Log("provider.responses_event", map[string]any{
		"protocol":  "responses",
		"stream_id": s.streamID,
		"type":      raw.Type,
	})

	switch raw.Type {
	case "response.output_text.delta":
		return s.parseOutputTextDelta(payload)
	case "response.output_text.done":
		return nil

	case "response.function_call_arguments.delta":
		return s.parseFunctionCallArgsDelta(payload)
	case "response.function_call_arguments.done":
		return s.parseFunctionCallArgsDone(payload)

	case "response.output_item.added":
		return s.parseOutputItemAdded(payload)

	case "response.refusal.delta":
		return s.parseRefusalDelta(payload)

	case "response.reasoning.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return s.parseReasoningDelta(payload)

	case "response.completed":
		return s.parseCompletedEvent(payload)

	case "response.output_item.done":
		return s.parseOutputItemDone(payload)

	default:
		return nil
	}
}

func (s *responsesStream) parseOutputTextDelta(payload string) *StreamEvent {
	var raw struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}
	return &StreamEvent{Content: raw.Delta}
}

func (s *responsesStream) parseRefusalDelta(payload string) *StreamEvent {
	var raw struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}
	return &StreamEvent{Content: raw.Delta}
}

func (s *responsesStream) parseReasoningDelta(payload string) *StreamEvent {
	var raw struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}
	if raw.Delta != "" {
		s.reasoningStreamed = true
	}
	return &StreamEvent{Reasoning: raw.Delta}
}

func (s *responsesStream) parseOutputItemAdded(payload string) *StreamEvent {
	var raw struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}

	if raw.Item.Type == "function_call" {
		s.toolCallInfo[raw.Item.ID] = struct {
			Index int
			Name  string
			ID    string
		}{
			Index: raw.OutputIndex,
			Name:  raw.Item.Name,
			ID:    raw.Item.CallID,
		}
	}

	return nil
}

func (s *responsesStream) parseOutputItemDone(payload string) *StreamEvent {
	var raw struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil || len(raw.Item) == 0 {
		return nil
	}
	if s.outputMetaKey != "" {
		s.outputItems = append(s.outputItems, CopyMapAny(raw.Item))
	}

	event := &StreamEvent{}
	if itemType, _ := raw.Item["type"].(string); itemType == "reasoning" {
		if s.reasoningStreamed {
			return s.outputMetadataEvent(event)
		}
		if summary, ok := raw.Item["summary"]; ok {
			if encoded, err := json.Marshal(summary); err == nil {
				event.Reasoning = parseReasoning(encoded)
			}
		}
		if event.Reasoning == "" {
			if content, ok := raw.Item["content"]; ok {
				if encoded, err := json.Marshal(content); err == nil {
					event.Reasoning = parseReasoning(encoded)
				}
			}
		}
	}

	return s.outputMetadataEvent(event)
}

func (s *responsesStream) outputMetadataEvent(event *StreamEvent) *StreamEvent {
	if event == nil {
		event = &StreamEvent{}
	}
	if s.outputMetaKey != "" && len(s.outputItems) > 0 {
		event.ProviderMeta = map[string]any{s.outputMetaKey: CopyReasoningDetails(s.outputItems)}
	}
	if event.Reasoning != "" || len(event.ProviderMeta) > 0 {
		return event
	}
	return nil
}

func (s *responsesStream) parseFunctionCallArgsDelta(payload string) *StreamEvent {
	var raw struct {
		ItemID string `json:"item_id"`
		Delta  string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}

	accumulated := raw.Delta
	if existing, ok := s.toolCallArgs[raw.ItemID]; ok {
		accumulated = existing + raw.Delta
	}
	s.toolCallArgs[raw.ItemID] = accumulated
	return nil
}

func (s *responsesStream) parseFunctionCallArgsDone(payload string) *StreamEvent {
	var raw struct {
		ItemID    string `json:"item_id"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}

	args := raw.Arguments
	if accumulated, hasAccumulated := s.toolCallArgs[raw.ItemID]; hasAccumulated && accumulated != "" {
		args = accumulated
		delete(s.toolCallArgs, raw.ItemID)
	}

	info, ok := s.toolCallInfo[raw.ItemID]
	if !ok {
		return nil
	}
	delete(s.toolCallInfo, raw.ItemID)

	return &StreamEvent{
		ToolCallDeltas: []ToolCallDelta{{
			Index:             info.Index,
			ID:                info.ID,
			Type:              "function",
			FunctionName:      info.Name,
			FunctionArguments: args,
		}},
	}
}

func (s *responsesStream) parseCompletedEvent(payload string) *StreamEvent {
	var raw struct {
		Response struct {
			Output []map[string]any  `json:"output"`
			Usage  tokenUsagePayload `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil
	}

	usage := raw.Response.Usage.normalized()
	s.completed = true
	event := &StreamEvent{Usage: usage}
	if usage != nil {
		event.TotalTokens = usage.TotalTokens
	}
	if s.outputMetaKey != "" {
		output := raw.Response.Output
		if len(output) == 0 {
			output = s.outputItems
		}
		if len(output) > 0 {
			event.ProviderMeta = map[string]any{s.outputMetaKey: CopyReasoningDetails(output)}
		}
	}
	if event.Usage != nil || len(event.ProviderMeta) > 0 {
		return event
	}
	return nil
}

func (p *responsesProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := BuildResponsesBody(req, OpenAIRequestOptions{
		ForceStream:       true,
		IncludeUsage:      true,
		ProviderExtraBody: p.extraBody,
		RequestExtraBody:  req.ExtraBody,
	})
	if err != nil {
		return nil, err
	}

	return streamResponses(
		ctx,
		p.httpClient,
		p.apiURL,
		token,
		body,
		nil,
		"",
	)
}
