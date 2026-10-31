package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"velocitylabs.cc/sdk/provider"
)

var ErrTurnInterrupted = errors.New("turn interrupted")

type turnInterruptKey struct{}

type turnInterruptState struct {
	parent      context.Context
	interrupted atomic.Bool
}

func withTurnInterrupt(parent context.Context) (context.Context, func(), func()) {
	if parent == nil {
		parent = context.Background()
	}
	state := &turnInterruptState{parent: parent}
	ctx, cancel := context.WithCancel(context.WithValue(parent, turnInterruptKey{}, state))
	interrupt := func() {
		if parent.Err() == nil {
			state.interrupted.Store(true)
		}
		cancel()
	}
	return ctx, interrupt, cancel
}

// IsTurnInterrupted reports whether ctx ended because the current turn was
// interrupted (e.g. by a rapid steer) while the parent run remains active.
func IsTurnInterrupted(ctx context.Context) bool {
	if ctx == nil || ctx.Err() == nil {
		return false
	}
	state, ok := ctx.Value(turnInterruptKey{}).(*turnInterruptState)
	if !ok || state == nil {
		return false
	}
	if state.parent != nil && state.parent.Err() != nil {
		return false
	}
	return state.interrupted.Load()
}

func sanitizeInterruptedTurn(res singleTurnResult, streamEndedCleanly bool) singleTurnResult {
	if streamEndedCleanly {
		return res
	}
	if len(res.reasoningDetails) > 0 {
		signed := make([]map[string]any, 0, len(res.reasoningDetails))
		for _, item := range res.reasoningDetails {
			sig, _ := item["signature"].(string)
			data, _ := item["data"].(string)
			if strings.TrimSpace(sig) != "" || strings.TrimSpace(data) != "" {
				signed = append(signed, item)
			}
		}
		res.reasoningDetails = signed
	}
	if len(res.providerMeta) > 0 {
		cleaned := make(map[string]any, len(res.providerMeta))
		for k, v := range res.providerMeta {
			if strings.HasSuffix(k, "_responses_output") {
				continue
			}
			cleaned[k] = v
		}
		if len(cleaned) == 0 {
			res.providerMeta = nil
		} else {
			res.providerMeta = provider.CopyMapAny(cleaned)
		}
	}
	return res
}
