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
	"os"
	"strings"
	"sync"

	"velocitylabs.cc/sdk/internal/debugtrace"
)

type streamOpts struct {
	ReasoningField  string
	ReasoningFields []string
	ProcessDelta    func(raw map[string]json.RawMessage, ev *StreamEvent)
	FinalEvent      func() (StreamEvent, bool)
}

type sseLine struct {
	line string
	err  error
}

type openAIStream struct {
	ctx          context.Context
	opts         streamOpts
	resp         *http.Response
	lineCh       chan sseLine
	closeOnce    sync.Once
	quit         chan struct{}
	readerDone   chan struct{}
	finalEmitted bool
	streamID     string
	singleJSON   bool
	jsonDone     bool
}

func streamChatCompletions(
	ctx context.Context,
	httpClient *http.Client,
	apiURL string,
	apiKey string,
	body []byte,
	extraHeaders map[string]string,
	opts streamOpts,
) (ChatStream, error) {
	endpoint := ProviderEndpoint(apiURL, "chat/completions")
	streamID := debugtrace.NextID()
	debugtrace.Log("provider.request", map[string]any{
		"protocol":     "chat_completions",
		"stream_id":    streamID,
		"body_bytes":   len(body),
		"body_sha256":  debugtrace.Digest(string(body)),
		"header_count": len(extraHeaders),
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		debugtrace.Log("provider.request_error", map[string]any{
			"protocol":  "chat_completions",
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
			"protocol":  "chat_completions",
			"stream_id": streamID,
			"error":     err.Error(),
		})
		return nil, err
	}
	debugtrace.Log("provider.response", map[string]any{
		"protocol":     "chat_completions",
		"stream_id":    streamID,
		"status":       resp.StatusCode,
		"content_type": resp.Header.Get("Content-Type"),
		"request_id":   resp.Header.Get("X-Request-ID"),
	})

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		debugtrace.Log("provider.http_error", map[string]any{
			"protocol":    "chat_completions",
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

	singleJSON := !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	s := &openAIStream{
		ctx:        ctx,
		opts:       opts,
		resp:       resp,
		lineCh:     make(chan sseLine, 64),
		quit:       make(chan struct{}),
		readerDone: make(chan struct{}),
		streamID:   streamID,
		singleJSON: singleJSON,
	}

	if singleJSON {
		close(s.readerDone)
		return s, nil
	}

	go s.readLoop()
	return s, nil
}

func (s *openAIStream) readLoop() {
	defer close(s.readerDone)
	defer close(s.lineCh)

	reader := bufio.NewReaderSize(s.resp.Body, 64*1024)
	for {
		lineRaw, err := reader.ReadString('\n')
		if err != nil {
			select {
			case s.lineCh <- sseLine{line: lineRaw, err: err}:
			case <-s.quit:
			}
			return
		}
		select {
		case s.lineCh <- sseLine{line: lineRaw}:
		case <-s.quit:
			return
		}
	}
}

func (s *openAIStream) shutdown() {
	s.closeOnce.Do(func() {
		close(s.quit)
		if s.resp != nil && s.resp.Body != nil {
			s.resp.Body.Close()
		}
	})
}

func (s *openAIStream) Close() error {
	s.shutdown()
	<-s.readerDone
	return nil
}

func (s *openAIStream) Recv() (StreamEvent, error) {
	if s.singleJSON {
		if s.jsonDone {
			return s.handleFinalEvent()
		}
		s.jsonDone = true
		defer s.shutdown()
		raw, err := io.ReadAll(io.LimitReader(s.resp.Body, 32<<20))
		if err != nil {
			return StreamEvent{}, err
		}
		ev, parseErr := s.parseEvent(strings.TrimSpace(string(raw)))
		if parseErr != nil {
			return StreamEvent{}, parseErr
		}
		if ev == nil {
			return s.handleFinalEvent()
		}
		return *ev, nil
	}

	for {
		payload, err := s.readSSEPayload()
		if err != nil {
			if errors.Is(err, io.EOF) {
				debugtrace.Log("provider.stream_end", map[string]any{
					"protocol":  "chat_completions",
					"stream_id": s.streamID,
					"reason":    "eof",
				})
				return s.handleFinalEvent()
			}
			debugtrace.Log("provider.stream_error", map[string]any{
				"protocol":  "chat_completions",
				"stream_id": s.streamID,
				"error":     err.Error(),
			})
			return StreamEvent{}, err
		}

		payload = strings.TrimSpace(payload)
		debugtrace.Log("provider.sse_payload", map[string]any{
			"protocol":       "chat_completions",
			"stream_id":      s.streamID,
			"payload_bytes":  len(payload),
			"payload_sha256": debugtrace.Digest(payload),
		})
		debugtrace.Raw("provider.sse_payload", map[string]any{
			"protocol":  "chat_completions",
			"stream_id": s.streamID,
		}, payload)
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			debugtrace.Log("provider.stream_end", map[string]any{
				"protocol":  "chat_completions",
				"stream_id": s.streamID,
				"reason":    "done_marker",
			})
			return s.handleFinalEvent()
		}

		ev, parseErr := s.parseEvent(payload)
		if parseErr != nil {
			return StreamEvent{}, parseErr
		}
		if ev == nil {
			continue
		}

		if s.opts.ProcessDelta != nil {
			var raw map[string]json.RawMessage
			_ = json.Unmarshal([]byte(payload), &raw)
			if choicesBytes, ok := raw["choices"]; ok {
				var choices []map[string]json.RawMessage
				if err := json.Unmarshal(choicesBytes, &choices); err == nil && len(choices) > 0 {
					s.opts.ProcessDelta(choices[0], ev)
				}
			}
		}

		return *ev, nil
	}
}

func (s *openAIStream) readSSEPayload() (string, error) {
	dataLines := make([]string, 0, 1)

	for {
		select {
		case <-s.ctx.Done():
			s.shutdown()
			return "", s.ctx.Err()
		case sl, ok := <-s.lineCh:
			if !ok {
				return "", io.EOF
			}
			if sl.err != nil {
				if errors.Is(sl.err, io.EOF) {
					if strings.HasPrefix(sl.line, "data:") {
						data := strings.TrimPrefix(strings.TrimRight(sl.line, "\r\n"), "data:")
						dataLines = append(dataLines, strings.TrimPrefix(data, " "))
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

func (s *openAIStream) handleFinalEvent() (StreamEvent, error) {
	if s.opts.FinalEvent != nil && !s.finalEmitted {
		if ev, ok := s.opts.FinalEvent(); ok {
			s.finalEmitted = true
			return ev, nil
		}
	}
	return StreamEvent{}, io.EOF
}

func parseStreamErrorField(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var object struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &object) == nil {
		if object.Message != "" {
			return fmt.Errorf("provider: %s", object.Message)
		}
		if object.Error != "" {
			return fmt.Errorf("provider: %s", object.Error)
		}
	}
	var message string
	if json.Unmarshal(raw, &message) == nil && message != "" {
		return fmt.Errorf("provider: %s", message)
	}
	return fmt.Errorf("provider returned an error")
}

func parseContentValue(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var collect func(any) string
	collect = func(v any) string {
		switch typed := v.(type) {
		case string:
			return typed
		case []any:
			var b strings.Builder
			for _, item := range typed {
				b.WriteString(collect(item))
			}
			return b.String()
		case map[string]any:
			for _, key := range []string{"text", "content", "value", "delta", "summary"} {
				if child, ok := typed[key]; ok {
					if t := collect(child); t != "" {
						return t
					}
				}
			}
		}
		return ""
	}
	return collect(value)
}

func parseReasoningDetailsValue(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var details []map[string]any
	if json.Unmarshal(raw, &details) == nil {
		return details
	}
	var detail map[string]any
	if json.Unmarshal(raw, &detail) == nil {
		return []map[string]any{detail}
	}
	return nil
}

func (s *openAIStream) parseEvent(payload string) (*StreamEvent, error) {
	var raw struct {
		Choices []struct {
			Delta        map[string]json.RawMessage `json:"delta"`
			Message      map[string]json.RawMessage `json:"message"`
			FinishReason string                     `json:"finish_reason"`
		} `json:"choices"`
		Usage tokenUsagePayload `json:"usage"`
		Error json.RawMessage   `json:"error"`
	}

	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		debugtrace.Log("provider.parse_error", map[string]any{
			"protocol":      "chat_completions",
			"stream_id":     s.streamID,
			"payload_bytes": len(payload),
			"error":         err.Error(),
		})
		return nil, fmt.Errorf("decode provider stream event: %w", err)
	}
	if err := parseStreamErrorField(raw.Error); err != nil {
		return nil, err
	}

	usage := raw.Usage.normalized()
	if len(raw.Choices) == 0 {
		if usage != nil {
			return &StreamEvent{TotalTokens: usage.TotalTokens, Usage: usage}, nil
		}
		return nil, nil
	}

	choice := raw.Choices[0]
	delta := choice.Delta
	if len(delta) == 0 {
		delta = choice.Message
	}

	var ev StreamEvent
	ev.FinishReason = choice.FinishReason
	if usage != nil {
		ev.TotalTokens = usage.TotalTokens
		ev.Usage = usage
	}
	if roleBytes, ok := delta["role"]; ok {
		_ = json.Unmarshal(roleBytes, &ev.Role)
	}
	if contentBytes, ok := delta["content"]; ok {
		ev.Content = parseContentValue(contentBytes)
	}

	for _, field := range s.reasoningFields() {
		if reasoningBytes, ok := delta[field]; ok {
			ev.Reasoning = parseReasoning(reasoningBytes)
			if ev.Reasoning != "" {
				break
			}
		}
	}

	if detailsBytes, ok := delta["reasoning_details"]; ok {
		if details := parseReasoningDetailsValue(detailsBytes); len(details) > 0 {
			ev.ReasoningDetails = details
		}
	}

	if toolCallsBytes, ok := delta["tool_calls"]; ok && len(toolCallsBytes) > 0 && string(toolCallsBytes) != "null" {
		var toolCalls []struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(toolCallsBytes, &toolCalls); err != nil {
			return nil, fmt.Errorf("decode provider tool call: %w", err)
		}
		for _, tc := range toolCalls {
			debugtrace.Log("provider.tool_delta", map[string]any{
				"protocol":        "chat_completions",
				"stream_id":       s.streamID,
				"index":           tc.Index,
				"id":              tc.ID,
				"name":            tc.Function.Name,
				"argument_bytes":  len(tc.Function.Arguments),
				"argument_sha256": debugtrace.Digest(tc.Function.Arguments),
			})
			ev.ToolCallDeltas = append(ev.ToolCallDeltas, ToolCallDelta{
				Index:             tc.Index,
				ID:                tc.ID,
				Type:              tc.Type,
				FunctionName:      tc.Function.Name,
				FunctionArguments: tc.Function.Arguments,
			})
		}
	}

	return &ev, nil
}

func (s *openAIStream) reasoningFields() []string {
	if len(s.opts.ReasoningFields) > 0 {
		return uniqueNonEmpty(s.opts.ReasoningFields)
	}
	return ReasoningReadFields(s.opts.ReasoningField)
}

type RateLimitError struct {
	StatusCode int
	RetryAfter int
	Message    string
}

func (e *RateLimitError) Error() string {
	return e.Message
}

func IsRateLimitError(err error) (*RateLimitError, bool) {
	if err == nil {
		return nil, false
	}
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}

type ServerError struct {
	StatusCode int
	Message    string
}

func (e *ServerError) Error() string {
	return e.Message
}

func IsServerError(err error) (*ServerError, bool) {
	if err == nil {
		return nil, false
	}
	var se *ServerError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

func formatStreamError(status string, requestBody, responseBody []byte) error {
	bodySummary := strings.TrimSpace(string(responseBody))
	if bodySummary == "" {
		bodySummary = "<empty response body>"
	}

	msg := fmt.Sprintf("stream API error (%s): %s", status, bodySummary)

	if os.Getenv("VELOCITY_STREAM_DEBUG") != "" {
		if requestPath, err := dumpPayload("velocity-request-*.json", requestBody); err == nil {
			msg += fmt.Sprintf("\nrequest: %s", requestPath)
		}
		if responsePath, err := dumpPayload("velocity-response-*.log", responseBody); err == nil {
			msg += fmt.Sprintf("\nresponse: %s", responsePath)
		}
	}

	return fmt.Errorf("%s", msg)
}

func dumpPayload(pattern string, payload []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	_ = f.Close()
	return path, nil
}
