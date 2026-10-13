package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

type OpenAIChatCompletionsRequest struct {
	Model         string           `json:"model"`
	Messages      []map[string]any `json:"messages"`
	Tools         []ToolDef        `json:"tools,omitempty"`
	ToolChoice    string           `json:"tool_choice,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions StreamOptions    `json:"stream_options,omitempty"`
}

type OpenAIRequestOptions struct {
	ForceStream             bool
	IncludeUsage            bool
	IncludeReasoningDetails bool
	MessageTransform        func([]map[string]any) []map[string]any
	ProviderExtraBody       map[string]any
	RequestExtraBody        map[string]any
	ReasoningField          string
	ReasoningPersistence    ReasoningPersistence
	ToolResultImagesAsUser  bool
	ResponsesOutputMetaKey  string
}

func BuildOpenAIChatBody(req ChatRequest, opts OpenAIRequestOptions) ([]byte, error) {
	messages := ConvertMessagesWithOptions(req.Messages, ConvertMessageOptions{
		IncludeReasoningDetails: opts.IncludeReasoningDetails,
		ReasoningField:          opts.ReasoningField,
		ReasoningPersistence:    opts.ReasoningPersistence,
		ToolResultImagesAsUser:  opts.ToolResultImagesAsUser,
	})
	if opts.MessageTransform != nil {
		messages = opts.MessageTransform(messages)
	}

	payload := OpenAIChatCompletionsRequest{
		Model:         req.Model,
		Messages:      messages,
		Tools:         req.Tools,
		ToolChoice:    strings.TrimSpace(req.ToolChoice),
		Stream:        req.Stream || opts.ForceStream,
		StreamOptions: StreamOptions{IncludeUsage: opts.IncludeUsage},
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	mergedExtra := MergeMaps(opts.ProviderExtraBody, opts.RequestExtraBody)
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

func ApplyExplicitCacheMarkers(messages []map[string]any) []map[string]any {
	converted := make([]map[string]any, 0, len(messages))
	partIndex := 0
	var lastTextPart map[string]any

	for _, msg := range messages {
		newMsg := CopyMapAny(msg)
		parts := normalizeMessageParts(msg["content"])

		for _, part := range parts {
			if partType, _ := part["type"].(string); partType == "text" {
				if strings.TrimSpace(fmt.Sprint(part["text"])) != "" {
					partIndex++
					lastTextPart = part
					if partIndex%20 == 0 {
						part["cache_control"] = map[string]any{"type": "ephemeral"}
					}
				}
			}
		}

		newMsg["content"] = parts
		converted = append(converted, newMsg)
	}

	if partIndex > 0 && lastTextPart != nil {
		if _, has := lastTextPart["cache_control"]; !has {
			lastTextPart["cache_control"] = map[string]any{"type": "ephemeral"}
		}
	}

	return converted
}

func normalizeMessageParts(content any) []map[string]any {
	switch c := content.(type) {
	case string:
		return []map[string]any{{"type": "text", "text": c}}
	case []map[string]any:
		parts := make([]map[string]any, 0, len(c))
		for _, part := range c {
			parts = append(parts, CopyMapAny(part))
		}
		return parts
	case []any:
		parts := make([]map[string]any, 0, len(c))
		for _, raw := range c {
			if partMap, ok := raw.(map[string]any); ok {
				parts = append(parts, CopyMapAny(partMap))
				continue
			}
			parts = append(parts, map[string]any{"type": "text", "text": fmt.Sprint(raw)})
		}
		return parts
	case nil:
		return []map[string]any{}
	default:
		return []map[string]any{{"type": "text", "text": fmt.Sprint(c)}}
	}
}
