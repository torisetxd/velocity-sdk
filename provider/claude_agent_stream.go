package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type claudeAgentUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *claudeAgentUsage) toTokenUsage(cost float64) *TokenUsage {
	if u == nil {
		return nil
	}
	return &TokenUsage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
		CachedTokens:     u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		Cost:             cost,
	}
}

type claudeAgentEnvelope struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Event     json.RawMessage `json:"event"`
	Message   json.RawMessage `json:"message"`
	UUID      string          `json:"uuid"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Version   string          `json:"claude_code_version"`
}

type claudeAgentStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
}

type claudeAgentMessage struct {
	Role    string `json:"role"`
	Error   string `json:"error"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"content"`
}

type claudeAgentDenial struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

type claudeAgentResult struct {
	IsError        bool                `json:"is_error"`
	Result         string              `json:"result"`
	Subtype        string              `json:"subtype"`
	APIError       string              `json:"api_error"`
	APIErrorStatus int                 `json:"api_error_status"`
	PermissionMode string              `json:"permission_mode"`
	NumTurns       int                 `json:"num_turns"`
	TotalCostUSD   float64             `json:"total_cost_usd"`
	Usage          *claudeAgentUsage   `json:"usage"`
	Denials        []claudeAgentDenial `json:"permission_denials"`
}

type claudeAgentRateLimit struct {
	Status   string `json:"status"`
	Type     string `json:"rateLimitType"`
	ResetsAt int64  `json:"resetsAt"`
	Overage  string `json:"overageStatus"`
}

type claudeAgentDecoder struct {
	stream     *claudeAgentStream
	transcript *claudeTranscript
	ctx        context.Context
	sawResult  bool
	sawDelta   bool
	emitted    bool
	rateLimit  *claudeAgentRateLimit
}

func (d *claudeAgentDecoder) emit(event StreamEvent) bool {
	select {
	case d.stream.events <- event:
		return true
	case <-d.ctx.Done():
		return false
	}
}

func (d *claudeAgentDecoder) decodeLine(raw []byte) {
	line := strings.TrimSpace(string(raw))
	if line == "" || !strings.HasPrefix(line, "{") {
		return
	}
	var envelope claudeAgentEnvelope
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		return
	}
	switch envelope.Type {
	case "stream_event":
		d.decodeStreamEvent(envelope.Event)
	case "system":
		d.decodeSession(envelope)
	case "user", "assistant":
		d.record(envelope)
		if envelope.Type == "assistant" {
			d.decodeAssistant(envelope.Message)
		}
	case "rate_limit_event":
		d.decodeRateLimit(envelope.Message, raw)
	case "result":
		d.decodeResult(raw)
	}
}

// decodeSession captures the identifiers the transcript records need.
func (d *claudeAgentDecoder) decodeSession(envelope claudeAgentEnvelope) {
	if d.transcript == nil || envelope.Subtype != "init" {
		return
	}
	d.transcript.cwd = envelope.Cwd
	d.transcript.version = envelope.Version
	if d.transcript.session == "" {
		d.transcript.session = envelope.SessionID
	}
}

// record appends the full user and assistant messages to the transcript, so the
// next run can resume this conversation with its thinking blocks intact.
func (d *claudeAgentDecoder) record(envelope claudeAgentEnvelope) {
	transcript := d.transcript
	if transcript == nil || transcript.session == "" || len(envelope.Message) == 0 {
		return
	}
	// The host already recorded the prompt; a plain text user echo would double it.
	if envelope.Type == "user" && !claudeMessageHasToolResult(envelope.Message) {
		return
	}
	if transcript.seen(envelope.UUID) {
		return
	}
	line, err := transcript.append(envelope.Type, envelope.Message, envelope.UUID, envelope.Timestamp)
	if err != nil {
		return
	}
	_ = transcript.write(line)
}

func (d *claudeAgentDecoder) decodeStreamEvent(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var event claudeAgentStreamEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return
	}
	switch event.Type {
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text == "" {
				return
			}
			d.sawDelta = true
			d.emitted = true
			d.emit(StreamEvent{Content: event.Delta.Text})
		case "thinking_delta":
			if event.Delta.Thinking == "" {
				return
			}
			d.sawDelta = true
			d.emit(StreamEvent{Reasoning: event.Delta.Thinking})
		}
	case "message_start":
		var payload struct {
			Message struct {
				Usage *claudeAgentUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &payload); err == nil && payload.Message.Usage != nil {
			if usage := payload.Message.Usage.toTokenUsage(0); usage != nil {
				d.emit(StreamEvent{Usage: usage, TotalTokens: usage.TotalTokens})
			}
		}
	}
}

func (d *claudeAgentDecoder) decodeAssistant(raw json.RawMessage) {
	if len(raw) == 0 || d.sawDelta {
		return
	}
	var message claudeAgentMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return
	}
	if message.Error != "" {
		return
	}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				d.emitted = true
				d.emit(StreamEvent{Content: block.Text})
			}
		case "thinking":
			if block.Thinking != "" {
				d.emit(StreamEvent{Reasoning: block.Thinking})
			}
		}
	}
}

func (d *claudeAgentDecoder) decodeRateLimit(_ json.RawMessage, raw []byte) {
	var payload struct {
		RateLimitInfo *claudeAgentRateLimit `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	d.rateLimit = payload.RateLimitInfo
}

func (d *claudeAgentDecoder) decodeResult(raw []byte) {
	var result claudeAgentResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return
	}
	d.sawResult = true

	if usage := result.Usage.toTokenUsage(result.TotalCostUSD); usage != nil {
		d.emit(StreamEvent{Usage: usage, TotalTokens: usage.TotalTokens})
	}

	text := strings.TrimSpace(result.Result)
	if text != "" && !d.emitted && !result.IsError {
		d.emitted = true
		d.emit(StreamEvent{Content: text})
	}

	if len(result.Denials) > 0 {
		names := make([]string, 0, len(result.Denials))
		for _, denial := range result.Denials {
			if name := strings.TrimSpace(denial.ToolName); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			d.stream.setError(fmt.Errorf("claude agent: Claude Code refused to run bridged tools: %s", strings.Join(names, ", ")))
			return
		}
	}

	if !result.IsError {
		return
	}
	message := text
	if message == "" {
		message = result.APIError
	}
	if message == "" {
		message = "run failed (" + result.Subtype + ")"
	}
	if note := d.resetNote(); note != "" {
		message += " (" + note + ")"
	}
	d.stream.setError(d.failure(message, result))
}

// failure maps the run's error onto the error types hosts already understand, so
// rate limits and overloads retry instead of looking like programming errors.
func (d *claudeAgentDecoder) failure(message string, result claudeAgentResult) error {
	lower := strings.ToLower(message + " " + result.APIError)
	switch {
	case result.APIErrorStatus == http.StatusTooManyRequests,
		result.APIError == "usage_limit_reached",
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "usage limit"),
		strings.Contains(lower, "hit your limit"):
		return &RateLimitError{
			StatusCode: http.StatusTooManyRequests,
			RetryAfter: d.retryAfter(),
			Message:    "claude agent: " + message,
		}
	case result.APIErrorStatus >= http.StatusInternalServerError,
		result.APIError == "server_error",
		strings.Contains(lower, "overloaded"),
		strings.Contains(lower, "server error"),
		strings.Contains(lower, "internal error"):
		status := result.APIErrorStatus
		if status < http.StatusInternalServerError {
			status = http.StatusServiceUnavailable
		}
		return &ServerError{StatusCode: status, Message: "claude agent: " + message}
	default:
		return fmt.Errorf("claude agent: %s", message)
	}
}

// retryAfter reports how long the reported limit still lasts, in seconds.
func (d *claudeAgentDecoder) retryAfter() int {
	if d.rateLimit == nil || d.rateLimit.ResetsAt == 0 {
		return 0
	}
	remaining := time.Until(time.Unix(d.rateLimit.ResetsAt, 0))
	if remaining <= 0 {
		return 0
	}
	return int(remaining.Seconds())
}

func (d *claudeAgentDecoder) resetNote() string {
	if d.rateLimit == nil || d.rateLimit.ResetsAt == 0 {
		return ""
	}
	reset := time.Unix(d.rateLimit.ResetsAt, 0).Local()
	window := strings.TrimSpace(d.rateLimit.Type)
	if window == "" {
		return "resets " + reset.Format("2 Jan 15:04")
	}
	return window + " limit resets " + reset.Format("2 Jan 15:04")
}
