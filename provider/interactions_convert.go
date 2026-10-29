package provider

import (
	"encoding/json"
	"strings"
)

// convertMessagesToInteractionsInput maps chat history to a stateless
// Interactions `input` (an array of steps) plus the system instruction.
func convertMessagesToInteractionsInput(messages []ChatMessage) (string, []map[string]any) {
	var system []string
	steps := make([]map[string]any, 0, len(messages))
	toolNames := map[string]string{}

	for _, msg := range messages {
		switch strings.TrimSpace(msg.Role) {
		case "system", "developer":
			for _, part := range normalizeContent(msg.Content) {
				if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
					system = append(system, text)
				}
			}
		case "user":
			if content := interactionsContent(msg.Content); len(content) > 0 {
				steps = append(steps, map[string]any{"type": "user_input", "content": content})
			}
		case "assistant":
			steps = append(steps, interactionsAssistantSteps(msg, toolNames)...)
		case "tool":
			steps = append(steps, interactionsToolResult(msg, toolNames))
		}
	}
	return strings.Join(system, "\n\n"), steps
}

func interactionsAssistantSteps(msg ChatMessage, toolNames map[string]string) []map[string]any {
	var steps []map[string]any
	steps = append(steps, interactionsThoughtSteps(msg.ProviderMeta)...)

	var text strings.Builder
	for _, part := range normalizeContent(msg.Content) {
		if t, _ := part["text"].(string); t != "" {
			text.WriteString(t)
		}
	}
	if text.Len() > 0 {
		steps = append(steps, map[string]any{
			"type":    "model_output",
			"content": []map[string]any{{"type": "text", "text": text.String()}},
		})
	}

	for _, call := range msg.ToolCalls {
		id := strings.TrimSpace(call.ID)
		if id == "" {
			id = "call_" + generateShortID()
		}
		toolNames[id] = call.Function.Name
		steps = append(steps, map[string]any{
			"type":      "function_call",
			"id":        id,
			"name":      call.Function.Name,
			"arguments": interactionsArguments(call.Function.Arguments),
		})
	}
	return steps
}

func interactionsThoughtSteps(meta map[string]any) []map[string]any {
	var out []map[string]any
	switch raw := meta[InteractionsThoughtsMetaKey].(type) {
	case []map[string]any:
		out = raw
	case []any:
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	steps := make([]map[string]any, 0, len(out))
	for _, item := range out {
		if sig, _ := item["signature"].(string); sig != "" {
			steps = append(steps, map[string]any{"type": "thought", "signature": sig})
		}
	}
	return steps
}

func interactionsArguments(raw string) map[string]any {
	args := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &args)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args
}

func interactionsToolResult(msg ChatMessage, toolNames map[string]string) map[string]any {
	callID := strings.TrimSpace(msg.ToolCallID)
	if callID == "" {
		callID = "call_" + generateShortID()
	}
	result := interactionsContent(msg.Content)
	if len(result) == 0 {
		result = []map[string]any{{"type": "text", "text": EmptyToolResponse}}
	}
	step := map[string]any{
		"type":    "function_result",
		"call_id": callID,
		"result":  result,
	}
	if name := toolNames[callID]; name != "" {
		step["name"] = name
	}
	return step
}

// interactionsContent converts chat content parts to Interactions content
// blocks. Content with no usable part yields nil.
func interactionsContent(content any) []map[string]any {
	parts := normalizeContent(content)
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part["type"] {
		case "text", "input_text", "output_text":
			if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		case "image_url":
			if block := interactionsImage(part["image_url"]); block != nil {
				out = append(out, block)
			}
		}
	}
	return out
}

func interactionsImage(raw any) map[string]any {
	var target string
	switch v := raw.(type) {
	case string:
		target = v
	case map[string]any:
		target, _ = v["url"].(string)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	if rest, ok := strings.CutPrefix(target, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		mime, _, _ := strings.Cut(meta, ";")
		if !found || mime == "" || !strings.Contains(meta, "base64") {
			return nil
		}
		return map[string]any{"type": "image", "data": data, "mime_type": mime}
	}
	return map[string]any{"type": "image", "uri": target}
}
