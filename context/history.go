package context

import (
	"strings"

	"velocitylabs.cc/sdk/provider"
)

func EventsFromProviderMessages(messages []provider.ChatMessage) []Event {
	if len(messages) == 0 {
		return nil
	}
	out := make([]Event, 0, len(messages))
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		switch role {
		case "system":
			out = append(out, SystemEvent(CloneAny(msg.Content)))
		case "user":
			out = append(out, UserEvent(CloneAny(msg.Content)))
		case "assistant":
			toolCalls := make([]ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				toolCalls = append(toolCalls, ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
			var reasoningDetails []map[string]any
			if len(msg.ReasoningMeta) > 0 {
				if details, ok := msg.ReasoningMeta["reasoning_details"].([]map[string]any); ok {
					reasoningDetails = provider.CopyReasoningDetails(details)
				}
			}
			event := AssistantEvent(CloneAny(msg.Content), msg.Reasoning, reasoningDetails, toolCalls, 0)
			event.ProviderMeta = provider.CopyMapAny(msg.ProviderMeta)
			out = append(out, event)
		case "tool":
			out = append(out, ToolResultEvent(msg.ToolCallID, CloneAny(msg.Content)))
		}
	}
	return out
}

func RenderHistory(events []Event, caps provider.Capabilities) ([]provider.ChatMessage, []string) {
	events = ActiveHistory(events)
	if len(events) == 0 {
		return nil, nil
	}

	msgs := make([]provider.ChatMessage, 0, len(events))
	warnings := make([]string, 0, 2)
	warned := map[string]struct{}{}

	addWarning := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if _, ok := warned[text]; ok {
			return
		}
		warned[text] = struct{}{}
		warnings = append(warnings, text)
	}

	for _, ev := range events {
		msg := provider.ChatMessage{
			Content: sanitizeContent(ev.Content, caps, addWarning),
		}

		switch ev.Kind {
		case EventKindSystem:
			msg.Role = "system"
		case EventKindUser:
			msg.Role = "user"
		case EventKindInternal:
			msg.Role = "user"
		case EventKindAssistant:
			msg.Role = "assistant"
			msg.ProviderMeta = provider.CopyMapAny(ev.ProviderMeta)
			if len(ev.ToolCalls) > 0 {
				if !caps.SupportsToolCalls {
					addWarning("tool call history was compacted because the active provider does not support native tool calls")
				} else {
					msg.ToolCalls = make([]provider.ToolCall, 0, len(ev.ToolCalls))
					for _, tc := range ev.ToolCalls {
						msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
							ID:   tc.ID,
							Type: "function",
							Function: provider.ToolFunction{
								Name:      tc.Name,
								Arguments: tc.Arguments,
							},
						})
					}
				}
			}
			if len(ev.ReasoningDetails) > 0 {
				if caps.SupportsReasoningDetails {
					msg.ReasoningMeta = map[string]any{
						"reasoning_details": provider.CopyReasoningDetails(ev.ReasoningDetails),
					}
				} else {
					addWarning("reasoning details from earlier turns were omitted because the active provider cannot replay them")
				}
			}
			if ev.Reasoning != "" {
				if !caps.SupportsReasoning {
					addWarning("reasoning text from earlier turns was omitted because the active provider cannot replay it")
				} else {
					msg.Reasoning = ev.Reasoning
				}
			}
		case EventKindTool:
			msg.Role = "tool"
			msg.ToolCallID = strings.TrimSpace(ev.ToolCallID)
			msg.Content = sanitizeContent(normalizeToolContent(limitToolContent(ev.Content)), caps, addWarning)
		default:
			continue
		}

		msgs = append(msgs, msg)
	}

	return msgs, warnings
}

func EventToProviderMessage(ev Event, caps provider.Capabilities) provider.ChatMessage {
	msgs, _ := RenderHistory([]Event{ev}, caps)
	if len(msgs) == 0 {
		return provider.ChatMessage{}
	}
	return msgs[0]
}

func sanitizeContent(content any, caps provider.Capabilities, addWarning func(string)) any {
	switch c := content.(type) {
	case nil:
		return nil
	case []map[string]any:
		return sanitizeContentParts(c, caps, addWarning)
	case []any:
		parts := make([]map[string]any, 0, len(c))
		usedParts := false
		for _, item := range c {
			partMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			usedParts = true
			parts = append(parts, partMap)
		}
		if !usedParts {
			return CloneAny(c)
		}
		return sanitizeContentParts(parts, caps, addWarning)
	default:
		return CloneAny(content)
	}
}

func sanitizeContentParts(parts []map[string]any, caps provider.Capabilities, addWarning func(string)) []map[string]any {
	if len(parts) == 0 {
		return nil
	}

	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		partType, _ := part["type"].(string)
		if (partType == "image_url" || partType == "input_image") && !caps.SupportsImages {
			addWarning("image content from earlier turns was omitted because the active provider does not support images")
			continue
		}
		out = append(out, provider.CopyMapAny(part))
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func limitToolContent(content any) any {
	return ModelLimit(content, DefaultModelTokenBudget)
}

func normalizeToolContent(content any) any {
	switch c := content.(type) {
	case []map[string]any:
		if len(c) == 0 {
			return provider.EmptyToolResponse
		}
		return c
	case []any:
		parts := make([]map[string]any, 0, len(c))
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				parts = append(parts, provider.CopyMapAny(m))
			}
		}
		if len(parts) > 0 {
			return parts
		}
		return provider.NormalizeToolResponse(content)
	default:
		return provider.NormalizeToolResponse(content)
	}
}
