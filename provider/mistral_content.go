package provider

import (
	"encoding/json"
	"strings"
)

const mistralContentMetaKey = "mistral_content"

func mistralAssistantContent(msg ChatMessage, content any) any {
	if parts := mistralContentParts(msg.ProviderMeta[mistralContentMetaKey]); len(parts) > 0 {
		return parts
	}
	if strings.TrimSpace(msg.Reasoning) == "" {
		return content
	}
	return append([]map[string]any{mistralThinkingPart(msg.Reasoning)}, mistralContentParts(content)...)
}

type mistralContentStream struct {
	parts       []*mistralContentPart
	hasThinking bool
}

func (s *mistralContentStream) decode(raw json.RawMessage, ev *StreamEvent) {
	if len(raw) == 0 && ev.Reasoning == "" {
		return
	}
	var value any
	_ = json.Unmarshal(raw, &value)
	parts := mistralContentParts(value)
	var text, thinking strings.Builder
	firstThinking := -1
	for i, part := range parts {
		switch part["type"] {
		case "text":
			value, _ := part["text"].(string)
			text.WriteString(value)
		case "thinking":
			if firstThinking < 0 {
				firstThinking = i
			}
			for _, inner := range mistralContentParts(part["thinking"]) {
				if inner["type"] == "text" {
					value, _ := inner["text"].(string)
					thinking.WriteString(value)
				}
			}
		}
	}
	ev.Content = text.String()
	if thinking.Len() > 0 {
		ev.Reasoning = thinking.String()
	} else if ev.Reasoning != "" {
		if firstThinking < 0 {
			s.parts = appendMistralPart(s.parts, mistralThinkingPart(ev.Reasoning), true)
			s.hasThinking = true
		} else {
			part := parts[firstThinking]
			part["thinking"] = append([]map[string]any{mistralTextPart(ev.Reasoning)}, mistralContentParts(part["thinking"])...)
		}
	}
	for i, part := range parts {
		if part["type"] == "thinking" && (len(mistralContentParts(part["thinking"])) > 0 || part["signature"] != nil) {
			s.hasThinking = true
		}
		s.parts = appendMistralPart(s.parts, part, i == 0)
	}
}

// Persist the ordered wire blocks separately from the flattened display text.
func (s *mistralContentStream) finalEvent() (StreamEvent, bool) {
	if !s.hasThinking {
		return StreamEvent{}, false
	}
	return StreamEvent{ProviderMeta: map[string]any{mistralContentMetaKey: mistralWireParts(s.parts)}}, true
}

type mistralContentPart struct {
	fields   map[string]any
	text     strings.Builder
	thinking []*mistralContentPart
}

func appendMistralPart(parts []*mistralContentPart, part map[string]any, continuation bool) []*mistralContentPart {
	kind := part["type"]
	var current *mistralContentPart
	if continuation && len(parts) > 0 {
		last := parts[len(parts)-1]
		if last.fields["type"] == kind {
			switch kind {
			case "text":
				current = last
			case "thinking":
				closed, _ := last.fields["closed"].(bool)
				priorSignature, _ := last.fields["signature"].(string)
				nextSignature, _ := part["signature"].(string)
				if (!closed || len(mistralContentParts(part["thinking"])) == 0) &&
					(priorSignature == "" || nextSignature == "" || priorSignature == nextSignature) {
					current = last
				}
			}
		}
	}
	merging := current != nil
	if !merging {
		current = &mistralContentPart{fields: part}
		parts = append(parts, current)
	}
	switch kind {
	case "text":
		text, _ := part["text"].(string)
		current.text.WriteString(text)
	case "thinking":
		for i, inner := range mistralContentParts(part["thinking"]) {
			current.thinking = appendMistralPart(current.thinking, inner, merging && i == 0)
		}
		for _, key := range []string{"signature", "closed"} {
			if value := part[key]; value != nil {
				current.fields[key] = value
			}
		}
	}
	return parts
}

func mistralWireParts(parts []*mistralContentPart) []map[string]any {
	wire := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		fields := CopyMapAny(part.fields)
		switch fields["type"] {
		case "text":
			fields["text"] = part.text.String()
		case "thinking":
			if len(part.thinking) == 0 && fields["signature"] == nil {
				continue
			}
			fields["thinking"] = mistralWireParts(part.thinking)
		}
		wire = append(wire, fields)
	}
	return wire
}

func mistralContentParts(value any) []map[string]any {
	switch value := value.(type) {
	case string:
		if value != "" {
			return []map[string]any{mistralTextPart(value)}
		}
	case []map[string]any:
		return value
	case []any:
		parts := make([]map[string]any, 0, len(value))
		for _, item := range value {
			switch item := item.(type) {
			case string:
				if item != "" {
					parts = append(parts, mistralTextPart(item))
				}
			case map[string]any:
				if kind, _ := item["type"].(string); kind != "" {
					parts = append(parts, item)
				}
			}
		}
		return parts
	}
	return nil
}

func mistralTextPart(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func mistralThinkingPart(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": []map[string]any{mistralTextPart(text)}}
}
