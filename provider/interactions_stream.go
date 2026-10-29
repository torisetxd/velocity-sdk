package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"velocitylabs.cc/sdk/internal/debugtrace"
)

const (
	// A silent connection is dropped and resumed rather than waited on forever.
	// Background (long decode) runs are given the 15-20 minutes Google
	// recommends because they pause for minutes between 5K-token chunks.
	interactionsIdleTimeout           = 5 * time.Minute
	interactionsBackgroundIdleTimeout = 20 * time.Minute

	interactionsMaxResumes   = 12
	interactionsPollInterval = 5 * time.Second
	interactionsMaxBackoff   = 15 * time.Second
	interactionsMaxPollFails = 12
)

type interactionsUsage struct {
	TotalInputTokens   int `json:"total_input_tokens"`
	InitialInputTokens int `json:"initial_input_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens"`
	TotalTokens        int `json:"total_tokens"`
}

// contextTokens estimates the prompt-plus-generation size. total_input_tokens
// sums the input of every long-decode hop, so it overstates the context.
func (u interactionsUsage) contextTokens() int {
	input := u.TotalInputTokens
	if u.InitialInputTokens > 0 {
		input = u.InitialInputTokens
	}
	if total := input + u.TotalOutputTokens + u.TotalThoughtTokens; total > 0 {
		return total
	}
	return u.TotalTokens
}

func (u interactionsUsage) tokenUsage() *TokenUsage {
	usage := TokenUsage{
		InputTokens:  u.TotalInputTokens,
		OutputTokens: u.TotalOutputTokens + u.TotalThoughtTokens,
		TotalTokens:  u.contextTokens(),
		CachedTokens: u.TotalCachedTokens,
	}
	if usage == (TokenUsage{}) {
		return nil
	}
	return &usage
}

type interactionsError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

func (e interactionsError) text() string {
	code := strings.Trim(string(e.Code), `"`)
	switch {
	case code != "" && e.Message != "":
		return code + ": " + e.Message
	case e.Message != "":
		return e.Message
	}
	return code
}

type interactionsEvent struct {
	EventType     string `json:"event_type"`
	EventID       string `json:"event_id"`
	Index         int    `json:"index"`
	InteractionID string `json:"interaction_id"`
	Status        string `json:"status"`
	Step          struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"step"`
	Delta struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Signature string          `json:"signature"`
		Arguments json.RawMessage `json:"arguments"`
		Content   struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"delta"`
	Usage       *interactionsUsage `json:"usage"`
	Interaction *struct {
		ID     string              `json:"id"`
		Status string              `json:"status"`
		Usage  *interactionsUsage  `json:"usage"`
		Errors []interactionsError `json:"errors"`
	} `json:"interaction"`
	Error *interactionsError `json:"error"`
}

type interactionsStep struct {
	typ       string
	toolIndex int
	argsSeen  bool
	signature string
}

type interactionsTransient struct{ err error }

func (e *interactionsTransient) Error() string { return e.err.Error() }
func (e *interactionsTransient) Unwrap() error { return e.err }

type interactionsStream struct {
	ctx        context.Context
	p          *interactionsProvider
	streamID   string
	background bool

	body      io.ReadCloser
	reader    *bufio.Reader
	cancel    context.CancelFunc
	idleTimer *time.Timer
	idleFired atomic.Bool

	interactionID string
	lastEventID   string
	status        string
	usage         *interactionsUsage
	failure       error

	steps     map[int]*interactionsStep
	thoughts  []map[string]any
	toolIndex int
	emitted   bool
	textBytes int

	resumes int
	pending []StreamEvent
	done    bool
}

func newInteractionsStream(ctx context.Context, p *interactionsProvider, background bool) *interactionsStream {
	return &interactionsStream{
		ctx:        ctx,
		p:          p,
		streamID:   debugtrace.NextID(),
		background: background,
		steps:      map[int]*interactionsStep{},
	}
}

type idleReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (i *idleReader) Read(buf []byte) (int, error) {
	n, err := i.r.Read(buf)
	if n > 0 {
		i.timer.Reset(i.d)
	}
	return n, err
}

func (s *interactionsStream) attach(body io.ReadCloser, cancel context.CancelFunc, idle time.Duration) {
	s.dropConnection()
	s.idleFired.Store(false)
	s.body = body
	s.cancel = cancel
	s.idleTimer = time.AfterFunc(idle, func() {
		s.idleFired.Store(true)
		cancel()
	})
	s.reader = bufio.NewReaderSize(&idleReader{r: body, timer: s.idleTimer, d: idle}, 64*1024)
}

func (s *interactionsStream) dropConnection() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.body != nil {
		_ = s.body.Close()
		s.body = nil
	}
	s.reader = nil
}

func (s *interactionsStream) Close() error {
	s.dropConnection()
	return nil
}

func (s *interactionsStream) Recv() (StreamEvent, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			if s.failure != nil {
				return StreamEvent{}, s.failure
			}
			return StreamEvent{}, io.EOF
		}

		payload, err := s.readPayload()
		if err == nil {
			if payload == "[DONE]" {
				s.finish()
				continue
			}
			handleErr := s.handle(payload)
			if handleErr == nil {
				continue
			}
			var transient *interactionsTransient
			if !errors.As(handleErr, &transient) {
				s.done = true
				return StreamEvent{}, handleErr
			}
			err = transient.err
		}

		if s.ctx.Err() != nil {
			return StreamEvent{}, s.ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			if isTerminalInteractionStatus(s.status) {
				s.finish()
				continue
			}
			err = io.ErrUnexpectedEOF
		}
		if err := s.recover(err); err != nil {
			s.done = true
			return StreamEvent{}, err
		}
	}
}

func (s *interactionsStream) readPayload() (string, error) {
	if s.reader == nil {
		return "", io.EOF
	}
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
			if s.idleFired.Load() {
				return "", fmt.Errorf("interactions stream idle for too long: %w", io.ErrUnexpectedEOF)
			}
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

func (s *interactionsStream) emit(ev StreamEvent) {
	s.emitted = true
	s.pending = append(s.pending, ev)
}

func (s *interactionsStream) handle(payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	var ev interactionsEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		debugtrace.Log("provider.parse_error", map[string]any{
			"protocol":  "interactions",
			"stream_id": s.streamID,
			"error":     err.Error(),
		})
		return nil
	}
	debugtrace.Raw("provider.sse_payload", map[string]any{"protocol": "interactions", "stream_id": s.streamID}, payload)
	if ev.EventID != "" {
		s.lastEventID = ev.EventID
	}

	switch ev.EventType {
	case "interaction.created":
		if ev.Interaction != nil && ev.Interaction.ID != "" {
			s.interactionID = ev.Interaction.ID
		}
	case "interaction.status_update":
		if ev.InteractionID != "" {
			s.interactionID = ev.InteractionID
		}
		if ev.Status != "" {
			s.status = ev.Status
		}
		if ev.Usage != nil {
			s.usage = ev.Usage
		}
	case "interaction.completed":
		if ev.Interaction != nil {
			if ev.Interaction.ID != "" {
				s.interactionID = ev.Interaction.ID
			}
			s.status = ev.Interaction.Status
			if ev.Interaction.Usage != nil {
				s.usage = ev.Interaction.Usage
			}
			s.setFailure(ev.Interaction.Errors)
		}
		s.finish()
	case "step.start":
		s.startStep(ev)
	case "step.delta":
		s.stepDelta(ev)
	case "step.stop":
		s.stopStep(ev.Index)
	case "error":
		return s.streamError(ev.Error)
	}
	return nil
}

func (s *interactionsStream) startStep(ev interactionsEvent) {
	step := &interactionsStep{typ: ev.Step.Type}
	s.steps[ev.Index] = step
	if step.typ != "function_call" {
		return
	}

	id := ev.Step.ID
	if id == "" {
		id = "call_" + uuid.NewString()
	}
	step.toolIndex = s.toolIndex
	s.toolIndex++
	delta := ToolCallDelta{Index: step.toolIndex, ID: id, Type: "function", FunctionName: ev.Step.Name}
	if args := strings.TrimSpace(string(ev.Step.Arguments)); args != "" && args != "{}" && args != "null" {
		delta.FunctionArguments = args
		step.argsSeen = true
	}
	s.emit(StreamEvent{ToolCallDeltas: []ToolCallDelta{delta}})
}

func (s *interactionsStream) stepDelta(ev interactionsEvent) {
	step := s.steps[ev.Index]
	if step == nil {
		step = &interactionsStep{}
		s.steps[ev.Index] = step
	}
	delta := ev.Delta

	switch delta.Type {
	case "text":
		if delta.Text == "" {
			return
		}
		if step.typ == "thought" {
			s.emit(StreamEvent{Reasoning: delta.Text})
			return
		}
		s.textBytes += len(delta.Text)
		s.emit(StreamEvent{Content: delta.Text})
	case "thought_summary":
		if delta.Content.Text != "" {
			s.emit(StreamEvent{Reasoning: delta.Content.Text})
		}
	case "thought_signature":
		step.signature = delta.Signature
	case "arguments_delta":
		chunk := rawArgumentsChunk(delta.Arguments)
		if chunk == "" {
			return
		}
		step.argsSeen = true
		s.emit(StreamEvent{ToolCallDeltas: []ToolCallDelta{{Index: step.toolIndex, FunctionArguments: chunk}}})
	}
}

func rawArgumentsChunk(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, `"`) {
		var chunk string
		if json.Unmarshal(raw, &chunk) == nil {
			return chunk
		}
	}
	return trimmed
}

func (s *interactionsStream) stopStep(index int) {
	step := s.steps[index]
	if step == nil {
		return
	}
	switch step.typ {
	case "function_call":
		if !step.argsSeen {
			step.argsSeen = true
			s.emit(StreamEvent{ToolCallDeltas: []ToolCallDelta{{Index: step.toolIndex, FunctionArguments: "{}"}}})
		}
	case "thought":
		if step.signature != "" {
			s.thoughts = append(s.thoughts, map[string]any{"type": "thought", "signature": step.signature})
			step.signature = ""
		}
	}
}

func (s *interactionsStream) setFailure(errs []interactionsError) {
	if len(errs) == 0 {
		return
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		if text := e.text(); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) > 0 {
		s.failure = &ServerError{StatusCode: http.StatusInternalServerError, Message: "interaction failed: " + strings.Join(parts, "; ")}
	}
}

func (s *interactionsStream) streamError(e *interactionsError) error {
	if e == nil {
		return &interactionsTransient{err: &ServerError{StatusCode: http.StatusInternalServerError, Message: "interactions stream error"}}
	}
	text := e.text()
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "429"), strings.Contains(lower, "resource_exhausted"), strings.Contains(lower, "rate limit"):
		return &RateLimitError{StatusCode: http.StatusTooManyRequests, Message: "rate limit exceeded: " + text}
	case strings.Contains(lower, "unavailable"), strings.Contains(lower, "internal"), strings.Contains(lower, "deadline"),
		strings.Contains(lower, "overloaded"), strings.Contains(lower, "500"), strings.Contains(lower, "503"), strings.Contains(lower, "504"):
		return &interactionsTransient{err: &ServerError{StatusCode: http.StatusServiceUnavailable, Message: "interactions stream error: " + text}}
	}
	return fmt.Errorf("interactions API error: %s", text)
}

func isTerminalInteractionStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "incomplete", "requires_action":
		return true
	}
	return false
}

// finish queues the closing event with usage and the data needed to replay
// this turn, and records a failure for statuses that did not produce output.
func (s *interactionsStream) finish() {
	if s.done {
		return
	}
	s.done = true

	switch s.status {
	case "failed":
		if s.failure == nil {
			s.failure = &ServerError{StatusCode: http.StatusInternalServerError, Message: "interaction failed"}
		}
		return
	case "cancelled":
		s.failure = errors.New("interaction was cancelled")
		return
	}
	s.failure = nil

	ev := StreamEvent{FinishReason: "stop"}
	switch {
	case s.status == "incomplete":
		ev.FinishReason = "length"
	case s.status == "requires_action", s.toolIndex > 0:
		ev.FinishReason = "tool_calls"
	}
	if s.usage != nil {
		ev.Usage = s.usage.tokenUsage()
		ev.TotalTokens = s.usage.contextTokens()
	}
	meta := map[string]any{}
	if len(s.thoughts) > 0 {
		meta[InteractionsThoughtsMetaKey] = s.thoughts
	}
	if s.interactionID != "" {
		meta[InteractionsIDMetaKey] = s.interactionID
	}
	if len(meta) > 0 {
		ev.ProviderMeta = meta
	}
	s.pending = append(s.pending, ev)
}

func (s *interactionsStream) wait(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resumeBackoff(attempt int) time.Duration {
	d := time.Second << min(attempt-1, 4)
	return min(d, interactionsMaxBackoff)
}

// recover keeps a long interaction alive across dropped connections. The
// interaction runs server side, so the stream is reattached from the last
// event seen, and if that is not possible the result is polled instead.
func (s *interactionsStream) recover(cause error) error {
	s.dropConnection()
	if s.interactionID == "" {
		return cause
	}
	debugtrace.Log("provider.interactions_recover", map[string]any{
		"stream_id": s.streamID,
		"id":        s.interactionID,
		"cause":     cause.Error(),
	})

	for s.resumes < interactionsMaxResumes {
		if s.lastEventID == "" && s.emitted {
			break
		}
		s.resumes++
		if err := s.wait(resumeBackoff(s.resumes)); err != nil {
			return err
		}

		query := url.Values{"stream": {"true"}}
		if s.lastEventID != "" {
			query.Set("last_event_id", s.lastEventID)
		}
		err := s.open(http.MethodGet, s.p.interactionURL(s.interactionID)+"?"+query.Encode(), nil)
		if err == nil {
			return nil
		}
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		_, rateLimited := IsRateLimitError(err)
		_, serverErr := IsServerError(err)
		var netErr interface{ Timeout() bool }
		if !rateLimited && !serverErr && !errors.As(err, &netErr) && !errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
	}
	return s.poll(cause)
}

type interactionsResource struct {
	Status string `json:"status"`
	Steps  []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Signature string          `json:"signature"`
		Arguments json.RawMessage `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"steps"`
	Usage  *interactionsUsage  `json:"usage"`
	Errors []interactionsError `json:"errors"`
}

// poll waits for the interaction to reach a final state and delivers whatever
// the stream had not yet shown.
func (s *interactionsStream) poll(cause error) error {
	failures := 0
	for {
		if err := s.wait(interactionsPollInterval); err != nil {
			return err
		}
		var res interactionsResource
		if err := s.p.getJSON(s.ctx, s.p.interactionURL(s.interactionID), &res); err != nil {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			failures++
			if failures >= interactionsMaxPollFails {
				return fmt.Errorf("lost interaction %s (%v): %w", s.interactionID, cause, err)
			}
			if err := s.wait(resumeBackoff(failures)); err != nil {
				return err
			}
			continue
		}
		failures = 0
		if !isTerminalInteractionStatus(res.Status) {
			continue
		}
		s.replayResource(res)
		return nil
	}
}

func (s *interactionsStream) replayResource(res interactionsResource) {
	var text strings.Builder
	calls := 0
	var thoughts []map[string]any
	for _, step := range res.Steps {
		switch step.Type {
		case "model_output":
			for _, part := range step.Content {
				if part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
		case "thought":
			if step.Signature != "" {
				thoughts = append(thoughts, map[string]any{"type": "thought", "signature": step.Signature})
			}
		case "function_call":
			if calls >= s.toolIndex {
				id := step.ID
				if id == "" {
					id = "call_" + uuid.NewString()
				}
				args := strings.TrimSpace(string(step.Arguments))
				if args == "" || args == "null" {
					args = "{}"
				}
				s.emit(StreamEvent{ToolCallDeltas: []ToolCallDelta{{Index: calls, ID: id, Type: "function", FunctionName: step.Name, FunctionArguments: args}}})
			}
			calls++
		}
	}
	if full := text.String(); len(full) > s.textBytes {
		s.pending = append([]StreamEvent{{Content: full[s.textBytes:]}}, s.pending...)
		s.textBytes = len(full)
	}
	if len(s.thoughts) == 0 {
		s.thoughts = thoughts
	}
	s.toolIndex = max(s.toolIndex, calls)
	s.status = res.Status
	if res.Usage != nil {
		s.usage = res.Usage
	}
	s.setFailure(res.Errors)
	s.finish()
}
