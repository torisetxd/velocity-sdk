package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

const defaultReasoningDetailsField = "reasoning_details"

var commonReasoningTextFields = []string{
	"reasoning",
	"reasoning_content",
	"reasoning_text",
	"thinking",
	"thought",
	"thoughts",
	"thought_content",
}

type ReasoningPersistence struct {
	TextFields          []string
	DetailsField        string
	IncludeEmptyDetails bool
}

func OpenAICompatibleReasoningPersistence(reasoningField string) ReasoningPersistence {
	field := strings.TrimSpace(reasoningField)
	if field == "" {
		field = "reasoning"
	}
	return ReasoningPersistence{
		TextFields: []string{field},
	}
}

func OpenRouterReasoningPersistence() ReasoningPersistence {
	return ReasoningPersistence{
		TextFields:          []string{"reasoning"},
		DetailsField:        defaultReasoningDetailsField,
		IncludeEmptyDetails: true,
	}
}

func ReasoningReadFields(primary string) []string {
	return uniqueNonEmpty(append([]string{primary}, commonReasoningTextFields...))
}

func firstReasoningField(p ReasoningPersistence) string {
	p = normalizeReasoningPersistence(p)
	if len(p.TextFields) == 0 {
		return ""
	}
	return p.TextFields[0]
}

func normalizeReasoningPersistence(p ReasoningPersistence) ReasoningPersistence {
	p.TextFields = uniqueNonEmpty(p.TextFields)
	p.DetailsField = strings.TrimSpace(p.DetailsField)
	return p
}

func uniqueNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func parseReasoning(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}

	var asObj map[string]any
	if err := json.Unmarshal(raw, &asObj); err == nil {
		return reasoningValueText(asObj)
	}

	var asArray []any
	if err := json.Unmarshal(raw, &asArray); err == nil {
		var parts []string
		for _, item := range asArray {
			if text := reasoningValueText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}

	var scalar any
	if err := json.Unmarshal(raw, &scalar); err == nil {
		if scalar == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(scalar))
	}

	return ""
}

func reasoningValueText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		var parts []string
		for _, key := range []string{"text", "summary", "content", "data", "value", "delta"} {
			if child, ok := v[key]; ok {
				if text := reasoningValueText(child); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	case []any:
		var parts []string
		for _, item := range v {
			if text := reasoningValueText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func AppendReasoningDetails(buffer *[]map[string]any, incoming []map[string]any) {
	for _, detail := range incoming {
		appendReasoningDetail(buffer, detail)
	}
}

func appendReasoningDetail(buffer *[]map[string]any, detail map[string]any) {
	if detail == nil {
		return
	}
	match := -1
	for i := len(*buffer) - 1; i >= 0; i-- {
		existing := (*buffer)[i]
		if fmt.Sprint(existing["index"]) != fmt.Sprint(detail["index"]) || fmt.Sprint(existing["type"]) != fmt.Sprint(detail["type"]) {
			continue
		}
		existingID, _ := existing["id"].(string)
		newID, _ := detail["id"].(string)
		if existingID != "" && newID != "" && existingID != newID {
			continue
		}
		match = i
		break
	}
	if match < 0 {
		*buffer = append(*buffer, CopyMapAny(detail))
		return
	}
	existing := (*buffer)[match]
	for key, value := range detail {
		if value == nil {
			continue
		}
		if key == "text" || key == "summary" || key == "data" {
			if fragment, ok := value.(string); ok {
				if prior, ok := existing[key].(string); ok {
					existing[key] = prior + fragment
				} else {
					existing[key] = fragment
				}
				continue
			}
		}
		existing[key] = value
	}
}
