package agent

import (
	"context"

	vcontext "velocitylabs.cc/sdk/context"
	"velocitylabs.cc/sdk/provider"
)

type EventKind = vcontext.EventKind

const (
	EventKindSystem              = vcontext.EventKindSystem
	EventKindUser                = vcontext.EventKindUser
	EventKindInternal            = vcontext.EventKindInternal
	EventKindAssistant           = vcontext.EventKindAssistant
	EventKindTool                = vcontext.EventKindTool
	EventKindCompressionBoundary = vcontext.EventKindCompressionBoundary
)

type ModelRef = vcontext.ModelRef
type ToolCall = vcontext.ToolCall
type GoalState = vcontext.GoalState
type Event = vcontext.Event
type RetainedFile = vcontext.RetainedFile
type CompressionData = vcontext.CompressionData
type Session = vcontext.Session
type AutoCompress = vcontext.AutoCompress
type CompressionStatus = vcontext.CompressionStatus
type CompressionResult = vcontext.CompressionResult
type CompressionRequestOptions = vcontext.CompressionRequestOptions
type FileReader = vcontext.FileReader
type ContextSection = vcontext.ContextSection
type DiffMode = vcontext.DiffMode

const (
	DiffWhole = vcontext.DiffWhole
	DiffLines = vcontext.DiffLines
)

var (
	UserEvent                         = vcontext.UserEvent
	SystemEvent                       = vcontext.SystemEvent
	InternalEvent                     = vcontext.InternalEvent
	AssistantEvent                    = vcontext.AssistantEvent
	ToolResultEvent                   = vcontext.ToolResultEvent
	CompressionBoundaryEvent          = vcontext.CompressionBoundaryEvent
	CompressionBoundaryEventWithData  = vcontext.CompressionBoundaryEventWithData
	CompressionBoundaryEventWithFiles = vcontext.CompressionBoundaryEventWithFiles
	CloneSession                      = vcontext.CloneSession
	CloneEvents                       = vcontext.CloneEvents
	CloneToolCalls                    = vcontext.CloneToolCalls
	CloneAny                          = vcontext.CloneAny
	CloneGoal                         = vcontext.CloneGoal
	GoalFromHistory                   = vcontext.GoalFromHistory
)

type RetryStatus struct {
	Attempt int
	Max     int
	Done    bool
	Reason  string
}

type Delta struct {
	Reasoning        string
	ReasoningDetails []map[string]any
	Content          string
	TotalTokens      int
}

type ToolOutput struct {
	ToolCallID string
	Text       string
	Done       bool
}

type Observer struct {
	OnRetry          func(RetryStatus)
	OnAssistantDelta func(Delta)
	OnToolCall       func(ToolCall)
	OnToolCallDelta  func(provider.ToolCallDelta)
	OnToolOutput     func(ToolOutput)
	OnUserMessage    func(string)
	OnCompression    func(CompressionStatus)
}

// ToolExecutor provides tool definitions and executes tool calls for a Runner.
type ToolExecutor interface {
	Definitions(ctx context.Context) []provider.ToolDef
	Execute(ctx context.Context, call ToolCall, onOutput func(ToolOutput)) any
}

// FuncToolExecutor adapts functions to the ToolExecutor interface.
type FuncToolExecutor struct {
	DefinitionsFn func(ctx context.Context) []provider.ToolDef
	ExecuteFn     func(ctx context.Context, call ToolCall, onOutput func(ToolOutput)) any
}

func (f FuncToolExecutor) Definitions(ctx context.Context) []provider.ToolDef {
	if f.DefinitionsFn == nil {
		return nil
	}
	return f.DefinitionsFn(ctx)
}

func (f FuncToolExecutor) Execute(ctx context.Context, call ToolCall, onOutput func(ToolOutput)) any {
	if f.ExecuteFn == nil {
		return "error: no tool executor configured"
	}
	return f.ExecuteFn(ctx, call, onOutput)
}

type Runner struct {
	Provider             provider.Provider
	Tools                ToolExecutor
	Model                ModelRef
	ModelExtra           map[string]any
	Root                 string
	FileReader           FileReader
	Observer             Observer
	ContinuationPrompt   string
	ContinuationPromptFn func() string
	PromptCacheEnabled   bool
	PipelinedTools       bool
	Steer                func() []string
	InternalMessages     func() []string
	// InternalContext registers the sections of volatile context (date, cwd,
	// uploads, ...) sent as <internal_context> messages instead of in the
	// system prompt. Only changes are appended, keeping the prompt cache warm.
	InternalContext        []ContextSection
	TransformUserPrompt    func(ctx context.Context, prompt string) any
	PrepareMessages        func(ctx context.Context, messages []provider.ChatMessage) []provider.ChatMessage
	DecorateAssistantEvent func(ev *Event)
	AutoCompress           AutoCompress
	AutoCompressFn         func() AutoCompress
	Compress               func(context.Context, []Event, []provider.ToolDef, string) (CompressionResult, error)
	MaxRetries             int
	MaxEmptyRetries        int
	MaxServerErrorRetries  int
	MaxToolRounds          int
}

type TurnResult struct {
	History          []Event
	Reasoning        string
	ReasoningDetails []map[string]any
	Reply            string
	Trace            []string
	Warnings         []string
	TotalTokens      int
}
