package provider

import (
	"context"
	"fmt"
	"strings"
)

// TokenSource optionally supplies a fresh bearer token per request when the
// host application manages rotating OAuth credentials externally.
type TokenSource func(ctx context.Context) (string, error)

type ChatMessage struct {
	Role          string         `json:"role"`
	Content       any            `json:"content,omitempty"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID    string         `json:"tool_call_id,omitempty"`
	ReasoningMeta map[string]any `json:"reasoning_meta,omitempty"`
	ProviderMeta  map[string]any `json:"provider_meta,omitempty"`
	Reasoning     string         `json:"reasoning,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatRequest struct {
	Model          string         `json:"model"`
	Messages       []ChatMessage  `json:"messages"`
	Tools          []ToolDef      `json:"tools,omitempty"`
	ToolChoice     string         `json:"tool_choice,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
	StreamOpts     *StreamOptions `json:"stream_options,omitempty"`
	PromptCacheKey string         `json:"-"`
	ExtraBody      map[string]any `json:"-"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type StreamEvent struct {
	Role             string
	Content          string
	Reasoning        string
	ReasoningDetails []map[string]any
	ProviderMeta     map[string]any
	Usage            *TokenUsage
	ToolCallDeltas   []ToolCallDelta
	FinishReason     string
	TotalTokens      int
}

type ToolCallDelta struct {
	Index             int
	ID                string
	Type              string
	FunctionName      string
	FunctionArguments string
}

type UsageInfo struct {
	UsableRequests *int               `json:"usable_requests,omitempty"`
	Credits        *float64           `json:"credits,omitempty"`
	CreditsLabel   string             `json:"credits_label,omitempty"`
	AccountCredits *float64           `json:"account_credits,omitempty"`
	Label          string             `json:"label,omitempty"`
	Resets         []UsageWindowReset `json:"resets,omitempty"`
}

type UsageWindowReset struct {
	Window      string `json:"window"`
	UsedPercent int    `json:"used_percent"`
	ResetAt     int64  `json:"reset_at"`
}

type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// InputModalities and OutputModalities come from the listing's
	// architecture block when the provider reports one (e.g. OpenRouter):
	// "text", "image", "audio", "embeddings", "transcription", ...
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	// ContextLength is the model's context window in tokens when the listing
	// reports one (OpenRouter's context_length, vLLM's max_model_len, ...).
	ContextLength int `json:"context_length,omitempty"`
}

type ChatStream interface {
	Recv() (StreamEvent, error)
	Close() error
}

type Provider interface {
	StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error)
}

func ConvertMessages(messages []ChatMessage) []map[string]any {
	return ConvertMessagesWithOptions(messages, ConvertMessageOptions{})
}

type ConvertMessageOptions struct {
	IncludeReasoningDetails bool
	ReasoningField          string
	ReasoningPersistence    ReasoningPersistence
	ToolResultImagesAsUser  bool
}

func ConvertMessagesWithOptions(messages []ChatMessage, opts ConvertMessageOptions) []map[string]any {
	reasoning := opts.reasoningPersistence()
	converted := make([]map[string]any, 0, len(messages))

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}

		if role == "tool" && opts.ToolResultImagesAsUser {
			if toolMsg, userMsg := splitToolResultImages(msg); toolMsg != nil {
				converted = append(converted, toolMsg)
				if userMsg != nil {
					converted = append(converted, userMsg)
				}
				continue
			}
		}

		chatMsg := map[string]any{
			"role":    role,
			"content": normalizeContentForRole(msg.Content, role),
		}

		if len(msg.ToolCalls) > 0 {
			chatMsg["tool_calls"] = msg.ToolCalls
		}
		if strings.TrimSpace(msg.ToolCallID) != "" {
			chatMsg["tool_call_id"] = msg.ToolCallID
		}
		if role == "assistant" && reasoning.DetailsField != "" {
			details, _ := extractReasoningDetails(msg.ReasoningMeta)
			if details == nil && reasoning.IncludeEmptyDetails {
				details = []map[string]any{}
			}
			if details != nil {
				chatMsg[reasoning.DetailsField] = details
			}
		}
		if msg.Reasoning != "" && role == "assistant" {
			for _, field := range reasoning.TextFields {
				chatMsg[field] = msg.Reasoning
			}
		} else if role == "assistant" && reasoning.IncludeEmptyOnToolCalls && len(msg.ToolCalls) > 0 {
			for _, field := range reasoning.TextFields {
				chatMsg[field] = ""
			}
		}

		converted = append(converted, chatMsg)
	}

	return converted
}

func (opts ConvertMessageOptions) reasoningPersistence() ReasoningPersistence {
	if len(opts.ReasoningPersistence.TextFields) > 0 || strings.TrimSpace(opts.ReasoningPersistence.DetailsField) != "" {
		return normalizeReasoningPersistence(opts.ReasoningPersistence)
	}

	p := ReasoningPersistence{}
	if strings.TrimSpace(opts.ReasoningField) != "" {
		p.TextFields = []string{opts.ReasoningField}
	}
	if opts.IncludeReasoningDetails {
		p.DetailsField = defaultReasoningDetailsField
		p.IncludeEmptyDetails = true
	}
	return normalizeReasoningPersistence(p)
}

func extractReasoningDetails(meta map[string]any) ([]map[string]any, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	raw, ok := meta["reasoning_details"]
	if !ok || raw == nil {
		return nil, false
	}

	switch v := raw.(type) {
	case []map[string]any:
		if len(v) == 0 {
			return nil, false
		}
		return CopyReasoningDetails(v), true
	case []any:
		if len(v) == 0 {
			return nil, false
		}
		details := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if itemMap, ok := item.(map[string]any); ok {
				details = append(details, CopyMapAny(itemMap))
			}
		}
		if len(details) == 0 {
			return nil, false
		}
		return details, true
	default:
		return nil, false
	}
}

const EmptyToolResponse = "(empty tool response)"

const ToolResultImagesNote = "images are not supported in tool results; refer to the following user message."

func splitToolResultImages(msg ChatMessage) (map[string]any, map[string]any) {
	parts := normalizeContent(msg.Content)
	if len(parts) == 0 {
		return nil, nil
	}

	var textParts []map[string]any
	var imageParts []map[string]any
	for _, part := range parts {
		if partType, _ := part["type"].(string); partType == "image_url" {
			imageParts = append(imageParts, part)
			continue
		}
		textParts = append(textParts, part)
	}
	if len(imageParts) == 0 {
		return nil, nil
	}

	toolMsg := map[string]any{
		"role":    "tool",
		"content": toolResultTextWithNote(textParts),
	}
	if id := strings.TrimSpace(msg.ToolCallID); id != "" {
		toolMsg["tool_call_id"] = id
	}

	userMsg := map[string]any{
		"role":    "user",
		"content": imageParts,
	}
	return toolMsg, userMsg
}

func toolResultTextWithNote(parts []map[string]any) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text := strings.TrimSpace(fmt.Sprint(part["text"])); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return ToolResultImagesNote
	}
	return strings.Join(texts, "\n") + "\n\n" + ToolResultImagesNote
}

func NormalizeToolResponse(content any) string {
	switch c := content.(type) {
	case nil:
		return EmptyToolResponse
	case string:
		if strings.TrimSpace(c) == "" {
			return EmptyToolResponse
		}
		return c
	default:
		text := strings.TrimSpace(fmt.Sprint(c))
		if text == "" {
			return EmptyToolResponse
		}
		return text
	}
}

func normalizeContentForRole(content any, role string) any {
	if strings.TrimSpace(role) == "tool" {
		if parts, ok := content.([]map[string]any); ok && len(parts) > 0 {
			normalized := make([]map[string]any, 0, len(parts))
			for _, part := range parts {
				if norm := normalizeContentPart(part); norm != nil {
					normalized = append(normalized, norm)
				}
			}
			if len(normalized) > 0 {
				return normalized
			}
		}
		return NormalizeToolResponse(content)
	}

	if s, ok := content.(string); ok {
		return s
	}

	if normalized := normalizeContent(content); normalized != nil {
		return normalized
	}
	return ""
}

func normalizeContent(content any) []map[string]any {
	switch c := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(c) == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": c}}
	case []map[string]any:
		if len(c) == 0 {
			return nil
		}
		parts := make([]map[string]any, 0, len(c))
		for _, part := range c {
			if normalized := normalizeContentPart(part); normalized != nil {
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
				if normalized := normalizeContentPart(partMap); normalized != nil {
					parts = append(parts, normalized)
				}
				continue
			}
			text := strings.TrimSpace(fmt.Sprint(rawPart))
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		}
		return parts
	default:
		text := strings.TrimSpace(fmt.Sprint(c))
		if text == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": text}}
	}
}

func normalizeContentPart(part map[string]any) map[string]any {
	if len(part) == 0 {
		return nil
	}

	partType, _ := part["type"].(string)

	if partType == "text" {
		if text := strings.TrimSpace(fmt.Sprint(part["text"])); text == "" {
			return nil
		}
	}

	if partType == "image_url" {
		iu := part["image_url"]
		switch v := iu.(type) {
		case string:
			iu = repairImageDataURL(v)
		case map[string]any:
			if u, ok := v["url"].(string); ok {
				url := repairImageDataURL(u)
				mapCopy := CopyMapAny(v)
				mapCopy["url"] = url
				iu = mapCopy
			}
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": iu,
		}
	}

	out := make(map[string]any, len(part))
	for k, v := range part {
		out[k] = v
	}
	return out
}

func CopyMapAny(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = copyValue(value)
	}
	return out
}

func copyValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return CopyMapAny(v)
	case []any:
		copied := make([]any, len(v))
		for i, item := range v {
			copied[i] = copyValue(item)
		}
		return copied
	case []map[string]any:
		copied := make([]map[string]any, len(v))
		for i, item := range v {
			copied[i] = CopyMapAny(item)
		}
		return copied
	default:
		return v
	}
}

func MergeMaps(base map[string]any, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := CopyMapAny(base)
	if out == nil {
		out = map[string]any{}
	}
	for key, value := range override {
		overrideMap, isMap := value.(map[string]any)
		if !isMap {
			out[key] = copyValue(value)
			continue
		}

		existingMap, ok := out[key].(map[string]any)
		if ok {
			out[key] = MergeMaps(existingMap, overrideMap)
			continue
		}
		out[key] = CopyMapAny(overrideMap)
	}
	return out
}

func CopyReasoningDetails(details []map[string]any) []map[string]any {
	if details == nil {
		return []map[string]any{}
	}
	copied := make([]map[string]any, len(details))
	for i, d := range details {
		copied[i] = CopyMapAny(d)
	}
	return copied
}
