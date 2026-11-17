package provider

import "strings"

type RequestConfiguration struct {
	Model     string         `json:"model"`
	ExtraBody map[string]any `json:"extra_body,omitempty"`
	Tools     []ToolDef      `json:"tools"`
}

func CloneRequestConfiguration(config *RequestConfiguration) *RequestConfiguration {
	if config == nil {
		return nil
	}
	out := &RequestConfiguration{Model: config.Model, ExtraBody: CopyMapAny(config.ExtraBody)}
	if config.Tools != nil {
		out.Tools = make([]ToolDef, len(config.Tools))
		copy(out.Tools, config.Tools)
		for i := range out.Tools {
			out.Tools[i].Function.Parameters = copyValue(config.Tools[i].Function.Parameters)
		}
	}
	return out
}

func supportsEffortUpdates(model, protocol string, extra map[string]any) bool {
	model = strings.ToLower(model)
	if protocol == "responses" {
		reasoning, _ := extra["reasoning"].(map[string]any)
		mode, _ := reasoning["mode"].(string)
		return strings.HasPrefix(model, "gpt-6") && mode != "pro" && extra["multi_agent"] == nil
	}
	thinking, _ := extra["thinking"].(map[string]any)
	if mode, _ := thinking["type"].(string); mode == "disabled" || mode == "between_tools" {
		return false
	}
	for _, family := range []string{"claude-opus-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-fable-5-2", "claude-mythos-5-1", "claude-mythos-5-2"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return true
		}
	}
	return false
}

func effortConfiguration(messages []ChatMessage, model, protocol string, extra, providerExtra map[string]any) ([]ChatMessage, map[string]any) {
	out := make([]ChatMessage, 0, len(messages))
	key := "reasoning"
	if protocol == "anthropic" {
		key = "output_config"
	}
	var previous string
	seen := false
	supported := supportsEffortUpdates(model, protocol, extra)
	for _, msg := range messages {
		if msg.Role != "configuration" {
			out = append(out, msg)
			continue
		}
		config := msg.Configuration
		if !supported || config == nil || config.Model != model {
			continue
		}
		configuredExtra := MergeMaps(providerExtra, config.ExtraBody)
		settings, _ := configuredExtra[key].(map[string]any)
		effort, _ := settings["effort"].(string)
		if !seen {
			seen, previous = true, effort
			extra = CopyMapAny(extra)
			if extra == nil {
				extra = map[string]any{}
			}
			current, _ := extra[key].(map[string]any)
			current = CopyMapAny(current)
			if current == nil {
				current = map[string]any{}
			}
			if effort == "" {
				delete(current, "effort")
			} else {
				current["effort"] = effort
			}
			extra[key] = current
			continue
		}
		if effort == previous {
			continue
		}
		previous = effort
		if effort == "" {
			effort = defaultEffort(model, protocol)
		}
		msg.Configuration = &RequestConfiguration{Model: model, ExtraBody: map[string]any{key: map[string]any{"effort": effort}}}
		out = append(out, msg)
	}
	return out, extra
}

func defaultEffort(model, protocol string) string {
	if protocol == "responses" || strings.HasPrefix(model, "claude-opus-5-5") || strings.HasPrefix(model, "claude-haiku-5-5") {
		return "medium"
	}
	return "high"
}
