package context

import (
	stdctx "context"
	"time"

	"velocitylabs.cc/sdk"
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

type GoalState struct {
	Definition string `json:"definition"`
	Criteria   string `json:"criteria"`
	Status     string `json:"status"`
	Evidence   string `json:"evidence,omitempty"`
	Blocker    string `json:"blocker,omitempty"`
}

// Timing is where one assistant step spent its time, in milliseconds. A step
// is one model request plus the tool calls it made. Reasoning, Output and the
// stream part of API come from the response stream, so they are measured for
// every provider that streams.
type Timing struct {
	// Wall is the whole step: request sent until its tool calls finished.
	Wall int64 `json:"wall_ms,omitempty"`
	// API is the wait from sending the request to the first streamed token.
	API int64 `json:"api_ms,omitempty"`
	// Reasoning is how long reasoning streamed, from the first reasoning token
	// until the answer or a tool call began. Providers that only return a
	// finished reasoning summary report their wait under API.
	Reasoning int64 `json:"reasoning_ms,omitempty"`
	// Output is how long the answer and tool calls streamed.
	Output int64 `json:"output_ms,omitempty"`
	// Tools is the time spent executing the step's tool calls. Tools run while
	// later calls are still streaming, so this overlaps Output; Wall is the
	// true elapsed time.
	Tools int64 `json:"tools_ms,omitempty"`
	// Run is the length of the whole Run, set only on the last assistant event
	// the run produced.
	Run int64 `json:"run_ms,omitempty"`
}

func (t Timing) Thinking() time.Duration {
	if t.Reasoning <= 0 {
		return 0
	}
	return time.Duration(t.API+t.Reasoning) * time.Millisecond
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
	Goal             *GoalState       `json:"goal,omitempty"`
	Timing           *Timing          `json:"timing,omitempty"`
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
// callers can read retained files from a container, remote volume, or disk
// (see DirFileReader). Supplying one enables file retention: the compression
// prompt asks for a <files_to_retain> block and those files are carried over.
type FileReader func(ctx stdctx.Context, relPath string) ([]byte, error)

func CloneGoal(g *GoalState) *GoalState {
	if g == nil {
		return nil
	}
	c := *g
	return &c
}

func GoalFromHistory(history []Event) *GoalState {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Goal != nil {
			return CloneGoal(history[i].Goal)
		}
	}
	return nil
}

// Now is the current time in the format Event.OccurredAt uses.
func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func UserEvent(content any) Event {
	return Event{Kind: EventKindUser, OccurredAt: Now(), Content: CloneAny(content)}
}

func SystemEvent(content any) Event {
	return Event{Kind: EventKindSystem, OccurredAt: Now(), Content: CloneAny(content)}
}

func InternalEvent(content any) Event {
	return Event{Kind: EventKindInternal, OccurredAt: Now(), Content: CloneAny(content)}
}

func AssistantEvent(content any, reasoning string, reasoningDetails []map[string]any, toolCalls []ToolCall, totalTokens int) Event {
	return Event{
		Kind:             EventKindAssistant,
		OccurredAt:       Now(),
		Content:          CloneAny(content),
		Reasoning:        reasoning,
		ReasoningDetails: provider.CopyReasoningDetails(reasoningDetails),
		ToolCalls:        CloneToolCalls(toolCalls),
		TotalTokens:      totalTokens,
	}
}

func SessionPromptCacheKey(sessionID string) string {
	return sdk.Current().CacheKeyPrefix + sessionID
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
		OccurredAt: Now(),
		ToolCallID: toolCallID,
		Content:    normalizeToolContent(content),
	}
}

func CompressionBoundaryEvent(content any) Event {
	return Event{Kind: EventKindCompressionBoundary, OccurredAt: Now(), Content: CloneAny(content)}
}

func CompressionBoundaryEventWithData(summary, transcript string) Event {
	return CompressionBoundaryEventWithFiles(summary, transcript, nil)
}

func CompressionBoundaryEventWithFiles(summary, transcript string, retainedFiles []RetainedFile) Event {
	return Event{
		Kind:       EventKindCompressionBoundary,
		OccurredAt: Now(),
		Content:    summary,
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
			Goal:             CloneGoal(ev.Goal),
		}
		if ev.Timing != nil {
			timing := *ev.Timing
			out[i].Timing = &timing
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
