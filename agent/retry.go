package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"velocitylabs.cc/sdk/provider"
)

const maxTurnRetryDelay = 60 * time.Second

type turnRetryState struct {
	rateLimitAttempts int
	serverAttempts    int
	lastAttempt       int
	lastMax           int
	lastReason        string
}

func (r Runner) retryTurnAfter(ctx context.Context, err error, state *turnRetryState) (bool, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}

	if rateLimitErr, ok := provider.IsRateLimitError(err); ok {
		state.rateLimitAttempts++
		maxAttempts := r.effectiveMaxRetries()
		if state.rateLimitAttempts >= maxAttempts {
			r.emitRetry(RetryStatus{Attempt: state.rateLimitAttempts, Max: maxAttempts, Done: true, Reason: "rate limit"})
			return false, fmt.Errorf("rate limit exceeded after %d retries: %w", maxAttempts, rateLimitErr)
		}

		state.record(state.rateLimitAttempts, maxAttempts, "rate limit")
		r.emitRetry(RetryStatus{Attempt: state.rateLimitAttempts, Max: maxAttempts, Done: false, Reason: "rate limit"})
		delay := 2 * time.Second
		if rateLimitErr.RetryAfter > 0 {
			delay = time.Duration(rateLimitErr.RetryAfter) * time.Second
		}
		if delay > maxTurnRetryDelay {
			delay = maxTurnRetryDelay
		}
		return waitForTurnRetry(ctx, delay)
	}

	if serverErr, ok := provider.IsServerError(err); ok {
		state.serverAttempts++
		maxAttempts := r.effectiveMaxServerErrorRetries()
		if state.serverAttempts >= maxAttempts {
			r.emitRetry(RetryStatus{Attempt: state.serverAttempts, Max: maxAttempts, Done: true, Reason: "server error"})
			return false, fmt.Errorf("server error after %d retries: %w", maxAttempts, serverErr)
		}

		state.record(state.serverAttempts, maxAttempts, "server error")
		r.emitRetry(RetryStatus{Attempt: state.serverAttempts, Max: maxAttempts, Done: false, Reason: "server error"})
		return waitForTurnRetry(ctx, 100*time.Millisecond)
	}

	return false, err
}

func (r Runner) finishTurnRetry(state turnRetryState) {
	if state.lastReason == "" {
		return
	}
	r.emitRetry(RetryStatus{
		Attempt: state.lastAttempt,
		Max:     state.lastMax,
		Done:    true,
		Reason:  state.lastReason,
	})
}

func (s *turnRetryState) record(attempt, maxAttempts int, reason string) {
	s.lastAttempt = attempt
	s.lastMax = maxAttempts
	s.lastReason = reason
}

func waitForTurnRetry(ctx context.Context, delay time.Duration) (bool, error) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}
