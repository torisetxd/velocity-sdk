package context

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

const CompressionUserPrefix = "-- Conversation Compressed Below --"

const (
	compressedContextTag   = "compressed_context"
	compressedContextOpen  = "<" + compressedContextTag + ">"
	compressedContextClose = "</" + compressedContextTag + ">"
)

type CompressionResult struct {
	History          []Event
	Summary          string
	RetainedFiles    []RetainedFile
	OriginalTokens   int
	CompressedTokens int
	TotalTokens      int
}

type CompressionRequestOptions struct {
	PromptCacheKey string
	Tools          []provider.ToolDef
	Root           string
	FileReader     FileReader
}

func CompressConversation(ctx stdctx.Context, p provider.Provider, model ModelRef, modelExtra map[string]any, history []Event, opts CompressionRequestOptions) (CompressionResult, error) {
	if p == nil {
		return CompressionResult{}, fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(model.Model) == "" {
		return CompressionResult{}, fmt.Errorf("model is required")
	}

	events := CloneEvents(history)
	if len(activeConversation(events)) == 0 {
		return CompressionResult{}, fmt.Errorf("no conversation to compress")
	}

	renderedHistory, _ := RenderHistory(events, ProviderCapabilities(p))
	req := provider.ChatRequest{
		Model: model.Model,
		Messages: append(renderedHistory, provider.ChatMessage{
			Role:    "user",
			Content: compressionUserRequest(),
		}),
		Tools:          append([]provider.ToolDef(nil), opts.Tools...),
		ToolChoice:     "none",
		Stream:         true,
		ExtraBody:      provider.CopyMapAny(modelExtra),
		PromptCacheKey: strings.TrimSpace(opts.PromptCacheKey),
	}

	stream, err := p.StreamChat(ctx, req)
	if err != nil {
		return CompressionResult{}, err
	}
	defer stream.Close()

	var summary strings.Builder
	totalTokens := 0
	for {
		if err := ctx.Err(); err != nil {
			return CompressionResult{}, err
		}

		ev, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			return CompressionResult{}, recvErr
		}
		if ev.Content != "" {
			summary.WriteString(ev.Content)
		}
		if ev.TotalTokens > 0 {
			totalTokens = ev.TotalTokens
		}
	}

	rawSummary := strings.TrimSpace(summary.String())
	if rawSummary == "" {
		return CompressionResult{}, fmt.Errorf("failed to generate compression summary")
	}

	cleanedSummary, rawRetained := ExtractFilesToRetainBlock(rawSummary)
	retainedFiles := LoadRetainedFiles(ctx, opts.Root, rawRetained, opts.FileReader)
	compressedSummary := cleanedSummary

	transcript := userAssistantTranscript(events)
	originalTokens := EstimateTokens(ActiveHistory(events))
	nextHistory := CloneEvents(events)
	nextHistory = append(nextHistory, CompressionBoundaryEventWithFiles(compressedSummary, transcript, retainedFiles))
	compressedTokens := EstimateTokens(ActiveHistory(nextHistory))
	if data := nextHistory[len(nextHistory)-1].CompressionData; data != nil {
		data.OriginalTokens = originalTokens
		data.CompressedTokens = compressedTokens
	}

	return CompressionResult{
		History:          nextHistory,
		Summary:          compressedSummary,
		RetainedFiles:    retainedFiles,
		OriginalTokens:   originalTokens,
		CompressedTokens: compressedTokens,
		TotalTokens:      totalTokens,
	}, nil
}

const (
	DefaultCompressionContextWindow = 272000
	DefaultCompressionThreshold     = 0.80882
)

func FormatTokenCount(n int) string {
	abs := n
	if abs < 0 {
		abs = -abs
	}
	if abs < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if abs < 1000000 {
		return formatScaledCount(float64(n)/1000, "k")
	}
	return formatScaledCount(float64(n)/1000000, "M")
}

func formatScaledCount(v float64, suffix string) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f%s", v, suffix)
	}
	return fmt.Sprintf("%.1f%s", v, suffix)
}

func FormatCompactedLabel(original, compressed int) string {
	return FormatCompactedLabelWithFiles(original, compressed, nil)
}

func FormatCompactedLabelWithFiles(original, compressed int, retainedFiles []RetainedFile) string {
	if original <= 0 || compressed <= 0 {
		return "Context Compacted"
	}
	saved := (1 - float64(compressed)/float64(original)) * 100
	fileNote := ""
	if n := len(retainedFiles); n > 0 {
		names := make([]string, 0, n)
		for _, rf := range retainedFiles {
			names = append(names, filepath.Base(rf.Path))
		}
		if n == 1 {
			fileNote = fmt.Sprintf(" · 1 file retained: %s", names[0])
		} else {
			fileNote = fmt.Sprintf(" · %d files retained: %s", n, strings.Join(names, ", "))
		}
	}
	return fmt.Sprintf("Context Compacted (%s → %s tokens · %.1f%% saved%s)", FormatTokenCount(original), FormatTokenCount(compressed), saved, fileNote)
}

func ShouldAutoCompress(cfg AutoCompress, history []Event) (bool, string) {
	return shouldAutoCompress(cfg, history, EstimateTokens(ActiveHistory(history)))
}

func ShouldAutoCompressWithTokens(cfg AutoCompress, history []Event, totalTokens int) (bool, string) {
	if totalTokens <= 0 {
		return ShouldAutoCompress(cfg, history)
	}
	return shouldAutoCompress(cfg, history, totalTokens)
}

func LastReportedTotalTokens(events []Event) int {
	active := ActiveHistory(events)
	for i := len(active) - 1; i >= 0; i-- {
		if active[i].Kind != EventKindAssistant {
			continue
		}
		if active[i].TotalTokens > 0 {
			return active[i].TotalTokens
		}
		usage, ok := active[i].ProviderMeta[provider.UsageMetadataKey].(map[string]any)
		if !ok {
			continue
		}
		switch total := usage["total_tokens"].(type) {
		case int:
			if total > 0 {
				return total
			}
		case float64:
			if total > 0 && total <= math.MaxInt {
				return int(total)
			}
		}
	}
	return 0
}

func shouldAutoCompress(cfg AutoCompress, history []Event, current int) (bool, string) {
	if !cfg.Enabled {
		return false, ""
	}
	if cfg.ContextWindow <= 0 && cfg.Threshold <= 1 {
		return false, ""
	}
	if IsCompressedOnlyHistory(history) {
		return false, ""
	}

	window := cfg.ContextWindow
	if window <= 0 {
		window = DefaultCompressionContextWindow
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = DefaultCompressionThreshold
	}

	if threshold > 1 {
		tokenThreshold := int(threshold)
		if current >= tokenThreshold {
			return true, fmt.Sprintf("%d tokens reached threshold %d", current, tokenThreshold)
		}
		return false, ""
	}

	limit := int(float64(window) * threshold)
	if current >= limit {
		return true, fmt.Sprintf("%d tokens is %.0f%% of %d context", current, (float64(current)/float64(window))*100, window)
	}
	return false, ""
}

func SplitUnansweredTail(history []Event) (head, tail []Event, ok bool) {
	last := -1
	for i, ev := range history {
		if ev.Kind == EventKindAssistant || ev.Kind == EventKindTool {
			last = i
		}
	}
	if last < 0 {
		return history, nil, false
	}
	return history[:last+1], history[last+1:], true
}

func EstimateTokens(events []Event) int {
	if len(events) == 0 {
		return 0
	}
	chars := 0
	for _, ev := range events {
		chars += len(string(ev.Kind)) + 8
		chars += estimateAnyContentChars(ev.Content)
		chars += len(ev.Reasoning)
		for _, tc := range ev.ToolCalls {
			chars += len(tc.ID) + len(tc.Name) + len(tc.Arguments) + 16
		}
		chars += len(ev.ToolCallID)
		if ev.CompressionData != nil {
			chars += len(ev.CompressionData.Transcript)
			for _, rf := range ev.CompressionData.RetainedFiles {
				chars += len(rf.Path) + len(rf.Content) + len(rf.Range) + 64
			}
		}
	}
	return int(math.Ceil(float64(chars) / 4.0))
}

func estimateAnyContentChars(content any) int {
	switch v := content.(type) {
	case nil:
		return 0
	case string:
		return collapseImageDataURLs(v)
	case []map[string]any:
		total := 0
		for _, item := range v {
			total += estimateMapChars(item)
		}
		return total
	case []any:
		total := 0
		for _, item := range v {
			total += estimateAnyContentChars(item)
		}
		return total
	case map[string]any:
		return estimateMapChars(v)
	default:
		return len(fmt.Sprint(v))
	}
}

const imagePartCharCost = 1024

func estimateMapChars(m map[string]any) int {
	if t, _ := m["type"].(string); t == "image_url" || t == "input_image" {
		return imagePartCharCost
	}
	total := 0
	for k, v := range m {
		total += len(k) + estimateAnyContentChars(v) + 4
	}
	return total
}

var dataImageURLRe = regexp.MustCompile(`data:image/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+`)

func collapseImageDataURLs(s string) int {
	if !strings.Contains(s, "data:image/") {
		return len(s)
	}
	collapsed := dataImageURLRe.ReplaceAllStringFunc(s, func(string) string {
		return strings.Repeat("x", imagePartCharCost)
	})
	return len(collapsed)
}

func nonSystemEvents(events []Event) []Event {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if ev.Kind != EventKindSystem && ev.Kind != EventKindInternal && ev.Kind != EventKindCompressionBoundary {
			out = append(out, ev)
		}
	}
	return out
}

func LastBoundaryIndex(events []Event) int {
	idx := -1
	for i, ev := range events {
		if ev.Kind == EventKindCompressionBoundary {
			idx = i
		}
	}
	return idx
}

func ActiveHistory(events []Event) []Event {
	boundary := LastBoundaryIndex(events)
	if boundary < 0 {
		return CloneEvents(events)
	}
	out := make([]Event, 0, len(events))
	for i, ev := range events {
		switch {
		case i < boundary && ev.Kind != EventKindSystem:
			continue
		case i == boundary:
			block := boundaryAsBlock(ev)
			if block != "" {
				out = append(out, InternalEvent(block))
			}
		default:
			out = append(out, CloneEvents(events[i : i+1])[0])
		}
	}
	return out
}

func boundaryAsBlock(ev Event) string {
	summary := transcriptContent(ev.Content)
	var b strings.Builder
	b.WriteString(compressedContextOpen)
	b.WriteString("\n")
	b.WriteString(CompressionUserPrefix)
	b.WriteString("\n")
	b.WriteString(summary)
	if ev.CompressionData != nil && len(ev.CompressionData.RetainedFiles) > 0 {
		b.WriteString("\n\n<active_files>\n")
		b.WriteString("The following active files were retained from the prior session and loaded from workspace storage. You do NOT need to re-read them:\n\n")
		for _, rf := range ev.CompressionData.RetainedFiles {
			b.WriteString("### File: " + rf.Path)
			if rf.Range != "" {
				b.WriteString(" (" + rf.Range + ")")
			}
			b.WriteString("\n```\n")
			b.WriteString(rf.Content)
			if !strings.HasSuffix(rf.Content, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("```\n\n")
		}
		b.WriteString("</active_files>")
	}
	if ev.CompressionData != nil && ev.CompressionData.Transcript != "" {
		b.WriteString("\n\n")
		b.WriteString(ev.CompressionData.Transcript)
	}
	b.WriteString("\n")
	b.WriteString(compressedContextClose)
	return b.String()
}

func activeConversation(events []Event) []Event {
	boundary := LastBoundaryIndex(events)
	start := 0
	if boundary >= 0 {
		start = boundary + 1
	}
	out := make([]Event, 0, len(events))
	for _, ev := range events[start:] {
		if ev.Kind == EventKindSystem || ev.Kind == EventKindInternal || ev.Kind == EventKindCompressionBoundary {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func IsCompressedOnlyHistory(events []Event) bool {
	if LastBoundaryIndex(events) < 0 {
		return false
	}
	for _, ev := range events[LastBoundaryIndex(events)+1:] {
		if ev.Kind == EventKindSystem || ev.Kind == EventKindCompressionBoundary {
			continue
		}
		return false
	}
	return true
}

func FormatEventsForCompression(events []Event) string {
	var b strings.Builder
	for i, ev := range nonSystemEvents(events) {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		b.WriteString("### ")
		b.WriteString(strings.ToUpper(string(ev.Kind)))
		b.WriteString("\n")
		if ev.Reasoning != "" {
			b.WriteString("\nReasoning:\n")
			b.WriteString(ev.Reasoning)
			b.WriteString("\n")
		}
		if len(ev.ToolCalls) > 0 {
			b.WriteString("\nTool Calls:\n")
			for _, tc := range ev.ToolCalls {
				b.WriteString("- ")
				b.WriteString(tc.Name)
				if tc.Arguments != "" {
					b.WriteString(": ")
					b.WriteString(tc.Arguments)
				}
				b.WriteString("\n")
			}
		}
		if ev.ToolCallID != "" {
			b.WriteString("\nTool Call ID: ")
			b.WriteString(ev.ToolCallID)
			b.WriteString("\n")
		}
		b.WriteString("\nContent:\n")
		b.WriteString(formatContentForCompression(ev.Content))
	}
	return strings.TrimSpace(b.String())
}

const transcriptMaxChars = 5000

func userAssistantTranscript(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		var role string
		switch ev.Kind {
		case EventKindUser:
			role = "user"
		case EventKindAssistant:
			role = "assistant"
		default:
			continue
		}
		text := strings.TrimSpace(transcriptContent(ev.Content))
		if text == "" {
			continue
		}
		if len(text) > transcriptMaxChars {
			text = text[:transcriptMaxChars] + "…"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func transcriptContent(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []map[string]any:
		var parts []string
		for _, part := range v {
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case []any:
		var parts []string
		for _, item := range v {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(v)
	}
}

func formatContentForCompression(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		if b, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(b)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func compressionUserRequest() string {
	return `This is a context-compression handoff. After you respond, ALL prior conversation context will be discarded and unavailable. Your response is the only context the next continuation will have, so it is REQUIRED to preserve EVERYTHING relevant to continuing the work accurately. Do not omit details just to be concise.

You are NOT allowed to make tool calls. Do not attempt to use tools, request tools, or continue the task. Return only the complete compression response now.

Summarize everything we've done so far, what's next, and your intentional architectural choices into one accurate, continuation-ready message.

Capture:
- the overall goal, current state, completed work, in-flight work, and precise next steps
- all relevant files and their current state, including important code locations or changes
- decisions and rationale (architecture, approach, tradeoffs)
- requirements, constraints, gotchas, unresolved issues, and anything that could block continuation
- attempts that failed or were abandoned, and why, so they are not repeated
- relevant commands, configuration, outputs, and other details needed to resume without the prior context

Write the response so another session can pick up exactly where we are without asking for information already present in the conversation. Be exhaustive about relevant details and specific; remove only irrelevant noise. Use clear structure where helpful.

CRITICAL - ACTIVE FILES RETENTION:
If your immediate next continuation turn requires inspecting active code files to continue editing, refactoring, or implementing without re-reading them, specify up to 3-5 files at the very end of your response inside a <files_to_retain> block:
<files_to_retain>
path/to/file1.go
path/to/file2.go:40-120
</files_to_retain>

Rules for <files_to_retain>:
- Only list files strictly necessary for the immediate next steps.
- Use relative workspace paths (or paths under /workspace). You may optionally specify 1-based line ranges (path:start-end) to retain only relevant sections.
- If the current task is finished, wrapping up, or only needs commands/tests to verify, leave the block empty: <files_to_retain></files_to_retain>.`
}
