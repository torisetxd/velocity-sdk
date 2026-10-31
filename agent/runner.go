package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	vcontext "velocitylabs.cc/sdk/context"
	"velocitylabs.cc/sdk/internal/debugtrace"
	"velocitylabs.cc/sdk/provider"
)

const (
	defaultMaxRetries      = 15
	defaultMaxEmptyRetries = 15
)

func (r Runner) effectiveMaxRetries() int {
	if r.MaxRetries > 0 {
		return r.MaxRetries
	}
	return defaultMaxRetries
}

func (r Runner) effectiveMaxEmptyRetries() int {
	if r.MaxEmptyRetries > 0 {
		return r.MaxEmptyRetries
	}
	if r.MaxEmptyRetries < 0 {
		return 0
	}
	return defaultMaxEmptyRetries
}

func (r Runner) effectiveMaxServerErrorRetries() int {
	if r.MaxServerErrorRetries > 0 {
		return r.MaxServerErrorRetries
	}
	return defaultMaxRetries
}

func (r Runner) Run(ctx context.Context, sess Session) (result TurnResult, runErr error) {
	if r.Provider == nil {
		return TurnResult{}, fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(r.Model.Model) == "" {
		return TurnResult{}, fmt.Errorf("model is required")
	}

	caps := vcontext.ProviderCapabilities(r.Provider)
	history := CloneEvents(sess.History)
	renderedHistory, warnings := vcontext.RenderHistory(history, caps)
	trace := []string{}
	allReasoning := ""
	var allReasoningDetails []map[string]any
	allContent := ""
	lastTotalTokens := sess.LastTotalTokens
	lastReportedTotalTokens := vcontext.LastReportedTotalTokens(history)
	prevTotalTokens := lastTotalTokens
	toolRounds := 0
	forceFinal := false

	vcontext.FlushInternalContext(&history, &renderedHistory, caps, r.InternalContext)

	for {
		if err := ctx.Err(); err != nil {
			return TurnResult{
				History:          history,
				Reasoning:        allReasoning,
				ReasoningDetails: allReasoningDetails,
				Reply:            allContent,
				Trace:            trace,
				Warnings:         warnings,
				TotalTokens:      lastTotalTokens,
			}, err
		}

		var definitions []provider.ToolDef
		if !forceFinal && r.Tools != nil {
			definitions = r.Tools.Definitions(ctx)
		}
		r.maybeAutoCompress(ctx, sess.ID, definitions, &history, &renderedHistory, &warnings, &lastReportedTotalTokens, &lastTotalTokens)

		messages := renderedHistory
		if r.PrepareMessages != nil {
			messages = r.PrepareMessages(ctx, messages)
		}

		req := provider.ChatRequest{
			Model:          r.Model.Model,
			Messages:       messages,
			Tools:          definitions,
			ToolChoice:     "auto",
			Stream:         true,
			ExtraBody:      provider.CopyMapAny(r.ModelExtra),
			PromptCacheKey: vcontext.PromptCacheKeyForSession(r.Provider, r.PromptCacheEnabled, sess.ID),
		}
		if len(definitions) == 0 {
			req.ToolChoice = ""
		}

		turnCtx, interruptTurn, cleanupTurn := withTurnInterrupt(ctx)
		if r.AwaitInterrupt != nil {
			go func() {
				if r.AwaitInterrupt(turnCtx) {
					interruptTurn()
				}
			}()
		}

		var toolResults []any
		assistantText, reasoningText, reasoningDetails, providerMeta, toolCalls, tokens, err := r.RunSingleTurn(turnCtx, req, &toolResults)
		interrupted := IsTurnInterrupted(turnCtx)
		if err != nil && !interrupted {
			cleanupTurn()
			return TurnResult{
				History:          history,
				Reasoning:        allReasoning,
				ReasoningDetails: allReasoningDetails,
				Reply:            allContent,
				Trace:            trace,
				Warnings:         warnings,
				TotalTokens:      lastTotalTokens,
			}, err
		}

		if reasoningText != "" {
			allReasoning += reasoningText
		}
		if len(reasoningDetails) > 0 {
			provider.AppendReasoningDetails(&allReasoningDetails, reasoningDetails)
		}
		if assistantText != "" {
			allContent += assistantText
		}
		if tokens > 0 {
			lastReportedTotalTokens = tokens
			prevTotalTokens = lastTotalTokens
			lastTotalTokens = tokens
		} else if !interrupted {
			lastReportedTotalTokens = tokens
		}

		if !interrupted && strings.TrimSpace(assistantText) == "" && len(toolCalls) == 0 {
			maxEmptyRetries := r.effectiveMaxEmptyRetries()
			for attempt := 1; attempt <= maxEmptyRetries; attempt++ {
				clearEmptyRetry := func() {
					r.emitRetry(RetryStatus{Attempt: attempt, Max: maxEmptyRetries, Done: true, Reason: "empty response"})
				}
				if err := turnCtx.Err(); err != nil {
					clearEmptyRetry()
					if IsTurnInterrupted(turnCtx) {
						interrupted = true
						break
					}
					cleanupTurn()
					return TurnResult{
						History:          history,
						Reasoning:        allReasoning,
						ReasoningDetails: allReasoningDetails,
						Reply:            allContent,
						Trace:            trace,
						Warnings:         warnings,
						TotalTokens:      lastTotalTokens,
					}, ctx.Err()
				}

				r.emitRetry(RetryStatus{Attempt: attempt, Max: maxEmptyRetries, Done: false, Reason: "empty response"})

				backoff := time.Duration(100+rand.Intn(200)) * time.Millisecond
				timer := time.NewTimer(backoff)
				select {
				case <-turnCtx.Done():
					timer.Stop()
					clearEmptyRetry()
					if IsTurnInterrupted(turnCtx) {
						interrupted = true
						break
					}
					cleanupTurn()
					return TurnResult{
						History:          history,
						Reasoning:        allReasoning,
						ReasoningDetails: allReasoningDetails,
						Reply:            allContent,
						Trace:            trace,
						Warnings:         warnings,
						TotalTokens:      lastTotalTokens,
					}, ctx.Err()
				case <-timer.C:
				}
				if interrupted {
					break
				}

				toolResults = nil
				var emptyRetryErr error
				assistantText, reasoningText, reasoningDetails, providerMeta, toolCalls, tokens, emptyRetryErr = r.RunSingleTurn(turnCtx, req, &toolResults)
				if IsTurnInterrupted(turnCtx) {
					clearEmptyRetry()
					interrupted = true
					if reasoningText != "" {
						allReasoning += reasoningText
					}
					if len(reasoningDetails) > 0 {
						provider.AppendReasoningDetails(&allReasoningDetails, reasoningDetails)
					}
					if assistantText != "" {
						allContent += assistantText
					}
					if tokens > 0 {
						lastReportedTotalTokens = tokens
						prevTotalTokens = lastTotalTokens
						lastTotalTokens = tokens
					}
					break
				}
				if emptyRetryErr != nil {
					clearEmptyRetry()
					cleanupTurn()
					return TurnResult{
						History:          history,
						Reasoning:        allReasoning,
						ReasoningDetails: allReasoningDetails,
						Reply:            allContent,
						Trace:            trace,
						Warnings:         warnings,
						TotalTokens:      lastTotalTokens,
					}, emptyRetryErr
				}

				if reasoningText != "" {
					allReasoning += reasoningText
				}
				if len(reasoningDetails) > 0 {
					provider.AppendReasoningDetails(&allReasoningDetails, reasoningDetails)
				}
				if assistantText != "" {
					allContent += assistantText
				}
				lastReportedTotalTokens = tokens
				if tokens > 0 {
					prevTotalTokens = lastTotalTokens
					lastTotalTokens = tokens
				}
				if strings.TrimSpace(assistantText) != "" || len(toolCalls) > 0 {
					clearEmptyRetry()
					break
				}
				if attempt >= maxEmptyRetries {
					clearEmptyRetry()
					cleanupTurn()
					return TurnResult{
						History:          history,
						Reasoning:        allReasoning,
						ReasoningDetails: allReasoningDetails,
						Reply:            allContent,
						Trace:            trace,
						Warnings:         warnings,
						TotalTokens:      lastTotalTokens,
					}, fmt.Errorf("model returned empty response after %d retries", maxEmptyRetries)
				}
			}
		}
		cleanupTurn()

		if interrupted {
			if strings.TrimSpace(assistantText) != "" || len(toolCalls) > 0 {
				turnTokens := tokens - prevTotalTokens
				if turnTokens < 0 {
					turnTokens = tokens
				}
				assistantEvent := AssistantEvent(assistantText, reasoningText, reasoningDetails, toolCalls, turnTokens)
				assistantEvent.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
				model := r.Model
				assistantEvent.Model = &model
				assistantEvent.ProviderMeta = provider.CopyMapAny(providerMeta)
				if r.DecorateAssistantEvent != nil {
					r.DecorateAssistantEvent(&assistantEvent)
				}
				history = append(history, assistantEvent)
				renderedHistory = append(renderedHistory, vcontext.EventToProviderMessage(assistantEvent, caps))
				for i, tc := range toolCalls {
					trace = append(trace, fmt.Sprintf("- %s(%s)", tc.Name, tc.Arguments))
					toolEvent := ToolResultEvent(tc.ID, toolResults[i])
					history = append(history, toolEvent)
					renderedHistory = append(renderedHistory, vcontext.EventToProviderMessage(toolEvent, caps))
				}
			}
			vcontext.FlushInternalContext(&history, &renderedHistory, caps, r.InternalContext)
			steered := r.drainQueuedPrompts(ctx, &history, &renderedHistory, caps)
			if !steered {
				steered = r.drainEndOfRunQueue(ctx, &history, &renderedHistory, caps)
			}
			if !steered && len(toolCalls) == 0 {
				return TurnResult{
					History:          history,
					Reasoning:        allReasoning,
					ReasoningDetails: allReasoningDetails,
					Reply:            assistantText,
					Trace:            trace,
					Warnings:         warnings,
					TotalTokens:      lastTotalTokens,
				}, nil
			}
			toolRounds = 0
			forceFinal = false
			continue
		}

		turnTokens := tokens - prevTotalTokens
		if turnTokens < 0 {
			turnTokens = tokens
		}
		assistantEvent := AssistantEvent(assistantText, reasoningText, reasoningDetails, toolCalls, turnTokens)
		assistantEvent.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
		model := r.Model
		assistantEvent.Model = &model
		assistantEvent.ProviderMeta = provider.CopyMapAny(providerMeta)
		if r.DecorateAssistantEvent != nil {
			r.DecorateAssistantEvent(&assistantEvent)
		}
		history = append(history, assistantEvent)
		renderedHistory = append(renderedHistory, vcontext.EventToProviderMessage(assistantEvent, caps))

		continuation := r.ContinuationPrompt
		if r.ContinuationPromptFn != nil {
			if dyn := r.ContinuationPromptFn(); dyn != "" {
				continuation = dyn
			}
		}
		if len(toolCalls) == 0 && continuation != "" {
			r.appendUserMessage(ctx, &history, &renderedHistory, continuation, caps)
			r.drainQueuedPrompts(ctx, &history, &renderedHistory, caps)
			r.drainEndOfRunQueue(ctx, &history, &renderedHistory, caps)
			continue
		}

		if len(toolCalls) > 0 && forceFinal {
			limitMessage := "I reached the task's tool-call safety limit. The work completed so far remains in the workspace; please ask me to continue."
			r.emitAssistantDelta(Delta{Content: limitMessage})
			allContent += limitMessage
			return TurnResult{
				History:          history,
				Reasoning:        allReasoning,
				ReasoningDetails: allReasoningDetails,
				Reply:            allContent,
				Trace:            trace,
				Warnings:         warnings,
				TotalTokens:      lastTotalTokens,
			}, nil
		}

		for i, tc := range toolCalls {
			trace = append(trace, fmt.Sprintf("- %s(%s)", tc.Name, tc.Arguments))
			result := toolResults[i]
			toolEvent := ToolResultEvent(tc.ID, result)
			history = append(history, toolEvent)
			renderedHistory = append(renderedHistory, vcontext.EventToProviderMessage(toolEvent, caps))
		}

		vcontext.FlushInternalContext(&history, &renderedHistory, caps, r.InternalContext)

		if r.drainQueuedPrompts(ctx, &history, &renderedHistory, caps) {
			toolRounds = 0
			forceFinal = false
			continue
		}

		if len(toolCalls) == 0 {
			if r.drainEndOfRunQueue(ctx, &history, &renderedHistory, caps) {
				toolRounds = 0
				forceFinal = false
				continue
			}
			return TurnResult{
				History:          history,
				Reasoning:        allReasoning,
				ReasoningDetails: allReasoningDetails,
				Reply:            assistantText,
				Trace:            trace,
				Warnings:         warnings,
				TotalTokens:      lastTotalTokens,
			}, nil
		}

		toolRounds++
		if r.MaxToolRounds > 0 && toolRounds >= r.MaxToolRounds {
			forceFinal = true
			renderedHistory = append(renderedHistory, provider.ChatMessage{
				Role:    "system",
				Content: "You have reached the tool-call safety limit for this response. Do not call more tools. Summarize the completed work, remaining work, and relevant files for the user.",
			})
		}
	}
}

func (r Runner) maybeAutoCompress(ctx context.Context, sessionID string, tools []provider.ToolDef, history *[]Event, renderedHistory *[]provider.ChatMessage, warnings *[]string, reportedTotalTokens *int, lastTotalTokens *int) {
	if history == nil {
		return
	}
	cfg := r.AutoCompress
	if r.AutoCompressFn != nil {
		cfg = r.AutoCompressFn()
	}
	reported := 0
	if reportedTotalTokens != nil {
		reported = *reportedTotalTokens
	}
	ok, reason := vcontext.ShouldAutoCompressWithTokens(cfg, *history, reported)
	if !ok {
		return
	}
	head, tail, splitOK := vcontext.SplitUnansweredTail(*history)
	if !splitOK {
		return
	}

	r.emitCompression(CompressionStatus{Reason: reason})

	var result CompressionResult
	var err error
	if r.Compress != nil {
		result, err = r.Compress(ctx, head, tools, r.Root)
	} else {
		result, err = vcontext.CompressConversation(ctx, r.Provider, r.Model, r.ModelExtra, head, CompressionRequestOptions{
			PromptCacheKey: vcontext.PromptCacheKeyForSession(r.Provider, r.PromptCacheEnabled, sessionID),
			Tools:          tools,
			Root:           r.Root,
			FileReader:     r.FileReader,
		})
	}
	if err != nil {
		r.emitCompression(CompressionStatus{Done: true, Reason: reason, Err: err})
		return
	}

	next := result.History
	if len(tail) > 0 {
		next = append(next, CloneEvents(tail)...)
	}
	*history = next
	caps := vcontext.ProviderCapabilities(r.Provider)
	rendered, w := vcontext.RenderHistory(*history, caps)
	*renderedHistory = rendered
	if warnings != nil && len(w) > 0 {
		*warnings = append(*warnings, w...)
	}
	vcontext.FlushInternalContext(history, renderedHistory, caps, r.InternalContext)
	if lastTotalTokens != nil && result.CompressedTokens > 0 {
		*lastTotalTokens = result.CompressedTokens
	}
	if reportedTotalTokens != nil {
		*reportedTotalTokens = result.CompressedTokens
	}
	r.emitCompression(CompressionStatus{
		Done:             true,
		Reason:           reason,
		Summary:          result.Summary,
		RetainedFiles:    result.RetainedFiles,
		History:          CloneEvents(*history),
		OriginalTokens:   result.OriginalTokens,
		CompressedTokens: result.CompressedTokens,
	})
}

func (r Runner) drainQueuedPrompts(ctx context.Context, history *[]Event, renderedHistory *[]provider.ChatMessage, caps provider.Capabilities) bool {
	injected := false
	if ctx.Err() == nil && r.InternalMessages != nil {
		for _, content := range r.InternalMessages() {
			ev := InternalEvent(content)
			*history = append(*history, ev)
			*renderedHistory = append(*renderedHistory, vcontext.EventToProviderMessage(ev, caps))
			injected = true
		}
	}
	if r.Steer == nil {
		return injected
	}
	prompts := r.Steer()
	if len(prompts) == 0 {
		return injected
	}
	for _, prompt := range prompts {
		r.appendUserMessage(ctx, history, renderedHistory, prompt, caps)
	}
	return true
}

func (r Runner) drainEndOfRunQueue(ctx context.Context, history *[]Event, renderedHistory *[]provider.ChatMessage, caps provider.Capabilities) bool {
	if ctx.Err() != nil || r.Queue == nil {
		return false
	}
	prompts := r.Queue()
	if len(prompts) == 0 {
		return false
	}
	for _, prompt := range prompts {
		r.appendUserMessage(ctx, history, renderedHistory, prompt, caps)
	}
	return true
}

func (r Runner) appendUserMessage(ctx context.Context, history *[]Event, renderedHistory *[]provider.ChatMessage, msg string, caps provider.Capabilities) {
	var content any = msg
	if r.TransformUserPrompt != nil {
		content = r.TransformUserPrompt(ctx, msg)
	}
	ev := UserEvent(content)
	*history = append(*history, ev)
	*renderedHistory = append(*renderedHistory, vcontext.EventToProviderMessage(ev, caps))
	if r.Observer.OnUserMessage != nil {
		r.Observer.OnUserMessage(msg)
	}
}

func (r Runner) executeOneTool(ctx context.Context, tc ToolCall) any {
	if err := ctx.Err(); err != nil {
		if IsTurnInterrupted(ctx) {
			r.emitToolOutput(ToolOutput{ToolCallID: tc.ID, Text: "tool skipped (turn steered)\n", Done: true})
			return "tool skipped: turn interrupted by user steer before execution"
		}
		r.emitToolOutput(ToolOutput{ToolCallID: tc.ID, Text: "turn cancelled\n", Done: true})
		return "turn cancelled"
	}
	if r.Tools == nil {
		msg := "tool error: no tool executor configured"
		r.emitToolOutput(ToolOutput{ToolCallID: tc.ID, Text: msg + "\n", Done: true})
		return msg
	}

	var doneEmitted bool
	result := r.Tools.Execute(ctx, tc, func(output ToolOutput) {
		output.ToolCallID = tc.ID
		if output.Done {
			doneEmitted = true
		}
		r.emitToolOutput(output)
	})
	if !doneEmitted {
		r.emitToolOutput(ToolOutput{ToolCallID: tc.ID, Done: true})
	}
	return result
}

type singleTurnResult struct {
	content          string
	reasoning        string
	reasoningDetails []map[string]any
	providerMeta     map[string]any
	toolCalls        []ToolCall
	totalTokens      int
}

func (r Runner) RunSingleTurn(ctx context.Context, req provider.ChatRequest, earlyResults *[]any) (content string, reasoning string, reasoningDetails []map[string]any, providerMeta map[string]any, toolCalls []ToolCall, totalTokens int, err error) {
	retryState := turnRetryState{}
	defer func() {
		r.finishTurnRetry(retryState)
	}()
	for {
		result, retrySafe, attemptErr := r.runSingleTurnAttempt(ctx, req, earlyResults)
		if attemptErr == nil {
			return result.content, result.reasoning, result.reasoningDetails, result.providerMeta, result.toolCalls, result.totalTokens, nil
		}
		if IsTurnInterrupted(ctx) {
			return result.content, result.reasoning, result.reasoningDetails, result.providerMeta, result.toolCalls, result.totalTokens, ErrTurnInterrupted
		}
		if !retrySafe {
			return "", "", nil, nil, nil, 0, attemptErr
		}
		retry, retryErr := r.retryTurnAfter(ctx, attemptErr, &retryState)
		if !retry {
			if IsTurnInterrupted(ctx) {
				return "", "", nil, nil, nil, 0, ErrTurnInterrupted
			}
			return "", "", nil, nil, nil, 0, retryErr
		}
	}
}

func (r Runner) runSingleTurnAttempt(ctx context.Context, req provider.ChatRequest, earlyResults *[]any) (singleTurnResult, bool, error) {
	stream, err := r.Provider.StreamChat(ctx, req)
	if err != nil {
		if IsTurnInterrupted(ctx) {
			return singleTurnResult{}, false, ErrTurnInterrupted
		}
		return singleTurnResult{}, true, err
	}
	defer stream.Close()
	retrySafe := true

	assistantText := ""
	seenContentText := false
	reasoningText := ""
	seenReasoningText := false
	details := []map[string]any{}
	meta := map[string]any{}
	toolCalls := []ToolCall{}
	toolCallBuilders := map[string]*ToolCall{}
	indexLastID := map[int]string{}
	toolIDToIndex := map[string]int{}
	lastTotalTokens := 0
	var lastSeenID string

	type execItem struct {
		idx int
		tc  ToolCall
	}
	var execMu sync.Mutex
	execResults := map[int]any{}
	execStarted := map[int]bool{}
	execCalls := map[int]ToolCall{}
	execCh := make(chan execItem, 16)
	streamDone := make(chan struct{})
	execClosed := false

	go func() {
		for item := range execCh {
			r.emitToolCall(item.tc)
			result := r.executeOneTool(ctx, item.tc)
			execMu.Lock()
			execResults[item.idx] = result
			execMu.Unlock()
		}
		close(streamDone)
	}()
	finishExec := func() {
		if execClosed {
			return
		}
		execClosed = true
		close(execCh)
		<-streamDone
	}
	defer finishExec()

	tryExec := func(idx int, tc ToolCall) {
		if strings.TrimSpace(tc.ID) == "" {
			tc.ID = uuid.New().String()
		}
		execMu.Lock()
		if execStarted[idx] {
			execMu.Unlock()
			return
		}
		execStarted[idx] = true
		execCalls[idx] = tc
		execMu.Unlock()
		execCh <- execItem{idx: idx, tc: tc}
	}

	lastSeenDeltaIndex := -1
	streamInterrupted := false

	for {
		if err := ctx.Err(); err != nil {
			if IsTurnInterrupted(ctx) {
				streamInterrupted = true
				break
			}
			return singleTurnResult{}, retrySafe, err
		}

		ev, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if IsTurnInterrupted(ctx) {
				streamInterrupted = true
				break
			}
			return singleTurnResult{}, retrySafe, recvErr
		}

		if ev.TotalTokens > 0 {
			lastTotalTokens = ev.TotalTokens
			r.emitAssistantDelta(Delta{TotalTokens: ev.TotalTokens})
		}

		if ev.Reasoning != "" {
			retrySafe = false
			reasoningDelta := ev.Reasoning
			if !seenReasoningText {
				reasoningDelta = strings.TrimPrefix(reasoningDelta, " ")
				seenReasoningText = true
			}
			if reasoningDelta != "" {
				reasoningText += reasoningDelta
				r.emitAssistantDelta(Delta{Reasoning: reasoningDelta})
			}
		}

		if ev.Content != "" {
			retrySafe = false
			contentDelta := ev.Content
			if !seenContentText {
				contentDelta = strings.TrimPrefix(contentDelta, " ")
				seenContentText = true
			}
			if contentDelta != "" {
				assistantText += contentDelta
				r.emitAssistantDelta(Delta{Content: contentDelta})
			}
		}

		if len(ev.ReasoningDetails) > 0 {
			provider.AppendReasoningDetails(&details, ev.ReasoningDetails)
			r.emitAssistantDelta(Delta{ReasoningDetails: provider.CopyReasoningDetails(ev.ReasoningDetails)})
		}
		if len(ev.ProviderMeta) > 0 {
			meta = provider.MergeMaps(meta, ev.ProviderMeta)
		}
		if ev.Usage != nil {
			meta = provider.MergeMaps(meta, provider.TokenUsageMetadata(ev.Usage))
		}

		for _, d := range ev.ToolCallDeltas {
			retrySafe = false
			r.emitToolCallDelta(d)
			debugtrace.Log("runner.tool_delta", map[string]any{
				"index": d.Index,
				"id":    d.ID,
				"name":  d.FunctionName,
			})

			if r.PipelinedTools && d.Index > lastSeenDeltaIndex && lastSeenDeltaIndex >= 0 {
				key := fmt.Sprintf("idx_%d", lastSeenDeltaIndex)
				if foundID, ok := indexLastID[lastSeenDeltaIndex]; ok && toolCallBuilders[foundID] != nil {
					key = foundID
				}
				if tc, ok2 := toolCallBuilders[key]; ok2 && tc.Name != "" {
					tryExec(lastSeenDeltaIndex, *tc)
				}
			}
			if d.Index > lastSeenDeltaIndex {
				lastSeenDeltaIndex = d.Index
			}

			key := fmt.Sprintf("idx_%d", d.Index)
			if d.ID != "" {
				if existingID, ok := indexLastID[d.Index]; ok && existingID != d.ID && d.FunctionName == "" {
					key = existingID
				} else {
					key = d.ID
					indexLastID[d.Index] = d.ID
					toolIDToIndex[d.ID] = d.Index
					lastSeenID = d.ID
				}
			} else if lastID, ok := indexLastID[d.Index]; ok {
				key = lastID
				lastSeenID = lastID
			} else if lastSeenID != "" {
				key = lastSeenID
			}

			tc, ok := toolCallBuilders[key]
			if !ok {
				tc = &ToolCall{}
				toolCallBuilders[key] = tc
			}
			if d.ID != "" {
				tc.ID = d.ID
			}
			if d.FunctionName != "" {
				tc.Name = d.FunctionName
			}
			if d.FunctionArguments != "" {
				tc.Arguments += d.FunctionArguments
			}
		}
	}

	if streamInterrupted {
		_ = stream.Close()
	}

	if len(toolCallBuilders) > 0 {
		type keyedTool struct {
			index int
			tc    ToolCall
		}
		sorted := make([]keyedTool, 0, len(toolCallBuilders))

		for key, tc := range toolCallBuilders {
			idx := 0
			if found, ok := toolIDToIndex[key]; ok {
				idx = found
			} else {
				fmt.Sscanf(key, "idx_%d", &idx)
			}
			if strings.TrimSpace(tc.ID) == "" && r.Tools != nil {
				tc.ID = uuid.New().String()
			}
			sorted = append(sorted, keyedTool{index: idx, tc: *tc})
		}

		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].index < sorted[j].index
		})

		if !streamInterrupted {
			for _, kt := range sorted {
				execMu.Lock()
				alreadyStarted := execStarted[kt.index]
				execMu.Unlock()
				if !alreadyStarted && kt.tc.Name != "" {
					tryExec(kt.index, kt.tc)
				}
			}
		}

		finishExec()

		execMu.Lock()
		for _, kt := range sorted {
			if kt.tc.Name == "" || (streamInterrupted && !execStarted[kt.index]) {
				continue
			}
			call := kt.tc
			if startedCall, ok := execCalls[kt.index]; ok {
				call = startedCall
			}
			toolCalls = append(toolCalls, call)
			if earlyResults != nil {
				*earlyResults = append(*earlyResults, execResults[kt.index])
			}
		}
		execMu.Unlock()
	} else {
		finishExec()
	}

	res := singleTurnResult{
		content:          assistantText,
		reasoning:        reasoningText,
		reasoningDetails: details,
		providerMeta:     meta,
		toolCalls:        toolCalls,
		totalTokens:      lastTotalTokens,
	}
	if streamInterrupted || IsTurnInterrupted(ctx) {
		res = sanitizeInterruptedTurn(res, !streamInterrupted)
		return res, false, ErrTurnInterrupted
	}
	return res, retrySafe, nil
}

func (r Runner) emitRetry(status RetryStatus) {
	if r.Observer.OnRetry != nil {
		r.Observer.OnRetry(status)
	}
}

func (r Runner) emitCompression(status CompressionStatus) {
	if r.Observer.OnCompression != nil {
		r.Observer.OnCompression(status)
	}
}

func (r Runner) emitAssistantDelta(delta Delta) {
	if r.Observer.OnAssistantDelta != nil {
		r.Observer.OnAssistantDelta(delta)
	}
}

func (r Runner) emitToolCall(call ToolCall) {
	if r.Observer.OnToolCall != nil {
		r.Observer.OnToolCall(call)
	}
}

func (r Runner) emitToolCallDelta(delta provider.ToolCallDelta) {
	if r.Observer.OnToolCallDelta != nil {
		r.Observer.OnToolCallDelta(delta)
	}
}

func (r Runner) emitToolOutput(output ToolOutput) {
	if r.Observer.OnToolOutput != nil {
		r.Observer.OnToolOutput(output)
	}
}
