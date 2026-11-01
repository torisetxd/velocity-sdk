package provider

import (
	"context"
	"encoding/json"
	"fmt"
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
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Event   json.RawMessage `json:"event"`
	Message json.RawMessage `json:"message"`
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
	stream    *claudeAgentStream
	ctx       context.Context
	sawResult bool
	sawDelta  bool
	emitted   bool
	rateLimit *claudeAgentRateLimit
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
	case "assistant":
		d.decodeAssistant(envelope.Message)
	case "rate_limit_event":
		d.decodeRateLimit(envelope.Message, raw)
	case "result":
		d.decodeResult(raw)
	}
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
	if text != "" && !d.emitted {
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
	if reset := d.resetNote(); reset != "" {
		message += " (" + reset + ")"
	}
	d.stream.setError(fmt.Errorf("claude agent: %s", message))
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
