package context

import (
	stdctx "context"

	"velocitylabs.cc/sdk/provider"
)

type EventKind string

const (
	EventKindSystem              EventKind = "system"
	EventKindUser                EventKind = "user"
	EventKindInternal            EventKind = "internal"
	EventKindAssistant           EventKind = "assistant"
	EventKindTool                EventKind = "tool"
	EventKindCompressionBoundary EventKind = "compression_boundary"
)

type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Event struct {
	Kind             EventKind        `json:"kind"`
	OccurredAt       string           `json:"occurred_at,omitempty"`
	Model            *ModelRef        `json:"model,omitempty"`
	Content          any              `json:"content,omitempty"`
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Reasoning        string           `json:"reasoning,omitempty"`
	ReasoningDetails []map[string]any `json:"reasoning_details,omitempty"`
	ProviderMeta     map[string]any   `json:"provider_meta,omitempty"`
	TotalTokens      int              `json:"total_tokens,omitempty"`
	CompressionData  *CompressionData `json:"compression,omitempty"`
}

type RetainedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Range   string `json:"range,omitempty"`
}

type CompressionData struct {
	Summary          string         `json:"summary,omitempty"`
	Transcript       string         `json:"transcript,omitempty"`
	RetainedFiles    []RetainedFile `json:"retained_files,omitempty"`
	OriginalTokens   int            `json:"original_tokens,omitempty"`
	CompressedTokens int            `json:"compressed_tokens,omitempty"`
}

type Session struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Model           ModelRef `json:"model"`
	History         []Event  `json:"history"`
	LastTotalTokens int      `json:"last_total_tokens,omitempty"`
}

type AutoCompress struct {
	Enabled       bool
	ContextWindow int
	Threshold     float64
}

type CompressionStatus struct {
	Done             bool
	Reason           string
	Summary          string
	RetainedFiles    []RetainedFile
	History          []Event
	OriginalTokens   int
	CompressedTokens int
	Err              error
}

// FileReader loads a workspace-relative file during context compression so
// callers can read retained files from a container, remote volume, or disk.
type FileReader func(ctx stdctx.Context, relPath string) ([]byte, error)

func UserEvent(content any) Event {
	return Event{Kind: EventKindUser, Content: CloneAny(content)}
}

func SystemEvent(content any) Event {
	return Event{Kind: EventKindSystem, Content: CloneAny(content)}
}

func InternalEvent(content any) Event {
	return Event{Kind: EventKindInternal, Content: CloneAny(content)}
}

func AssistantEvent(content any, reasoning string, reasoningDetails []map[string]any, toolCalls []ToolCall, totalTokens int) Event {
	return Event{
		Kind:             EventKindAssistant,
		Content:          CloneAny(content),
		Reasoning:        reasoning,
		ReasoningDetails: provider.CopyReasoningDetails(reasoningDetails),
		ToolCalls:        CloneToolCalls(toolCalls),
		TotalTokens:      totalTokens,
	}
}

func SessionPromptCacheKey(sessionID string) string {
	return "velocity-session-" + sessionID
}

func PromptCacheKeyForSession(p provider.Provider, configured bool, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	caps := ProviderCapabilities(p)
	if !caps.SupportsPromptCache || (!configured && !caps.PromptCacheEnabledByDefault) {
		return ""
	}
	return SessionPromptCacheKey(sessionID)
}

func ProviderCapabilities(p provider.Provider) provider.Capabilities {
	if cp, ok := p.(provider.CapabilityReporter); ok {
		return cp.Capabilities()
	}
	return provider.Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func ToolResultEvent(toolCallID string, content any) Event {
	return Event{
		Kind:       EventKindTool,
		ToolCallID: toolCallID,
		Content:    normalizeToolContent(content),
	}
}

func CompressionBoundaryEvent(content any) Event {
	return Event{Kind: EventKindCompressionBoundary, Content: CloneAny(content)}
}

func CompressionBoundaryEventWithData(summary, transcript string) Event {
	return CompressionBoundaryEventWithFiles(summary, transcript, nil)
}

func CompressionBoundaryEventWithFiles(summary, transcript string, retainedFiles []RetainedFile) Event {
	return Event{
		Kind:    EventKindCompressionBoundary,
		Content: summary,
		CompressionData: &CompressionData{
			Summary:       summary,
			Transcript:    transcript,
			RetainedFiles: retainedFiles,
		},
	}
}

func CloneSession(s Session) Session {
	return Session{
		ID:              s.ID,
		Name:            s.Name,
		Model:           s.Model,
		History:         CloneEvents(s.History),
		LastTotalTokens: s.LastTotalTokens,
	}
}

func CloneEvents(events []Event) []Event {
	if len(events) == 0 {
		return nil
	}
	out := make([]Event, len(events))
	for i, ev := range events {
		content := CloneAny(ev.Content)
		if ev.Kind == EventKindTool {
			content = normalizeToolContent(ev.Content)
		}
		out[i] = Event{
			Kind:             ev.Kind,
			OccurredAt:       ev.OccurredAt,
			Content:          content,
			ToolCalls:        CloneToolCalls(ev.ToolCalls),
			ToolCallID:       ev.ToolCallID,
			Reasoning:        ev.Reasoning,
			ReasoningDetails: provider.CopyReasoningDetails(ev.ReasoningDetails),
			ProviderMeta:     provider.CopyMapAny(ev.ProviderMeta),
			TotalTokens:      ev.TotalTokens,
			CompressionData:  CloneCompressionData(ev.CompressionData),
		}
		if ev.Model != nil {
			model := *ev.Model
			out[i].Model = &model
		}
	}
	return out
}

func CloneCompressionData(d *CompressionData) *CompressionData {
	if d == nil {
		return nil
	}
	var files []RetainedFile
	if len(d.RetainedFiles) > 0 {
		files = make([]RetainedFile, len(d.RetainedFiles))
		copy(files, d.RetainedFiles)
	}
	return &CompressionData{
		Summary:          d.Summary,
		Transcript:       d.Transcript,
		RetainedFiles:    files,
		OriginalTokens:   d.OriginalTokens,
		CompressedTokens: d.CompressedTokens,
	}
}

func CloneToolCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(calls))
	copy(out, calls)
	return out
}

func CloneAny(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return provider.CopyMapAny(v)
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i := range v {
			out[i] = provider.CopyMapAny(v[i])
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = CloneAny(v[i])
		}
		return out
	default:
		return v
	}
}
