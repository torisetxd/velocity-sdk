package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Mistral Vibe browser sign-in, as implemented by Mistral Code: the console
// issues a one-time sign-in URL, the user approves in the browser, and the
// completed grant is exchanged (PKCE S256) for a regular La Plateforme API
// key that authenticates https://api.mistral.ai/v1 like any MISTRAL_API_KEY.

var (
	// mistralVibeConsoleURL is overridable for tests.
	mistralVibeConsoleURL = "https://console.mistral.ai"

	mistralVibeClient = &http.Client{Timeout: 30 * time.Second}
)

const (
	mistralVibeAPIPrefix      = "/api"
	mistralVibeRequestTimeout = 30 * time.Second
	mistralVibePollInterval   = 3 * time.Second
	mistralVibeSlowDownCap    = 30 * time.Second
	mistralVibeMaxPollErrors  = 3
	// mistralVibeEntitlementCode marks a workspace that is not allowed to use
	// Mistral Vibe (HTTP 403 during exchange).
	mistralVibeEntitlementCode = 9004
)

const (
	mistralVibePending   = "pending"
	mistralVibeCompleted = "completed"
	mistralVibeExpired   = "expired"
	mistralVibeDenied    = "denied"
	mistralVibeError     = "error"
)

var errMistralVibeExpired = errors.New("Mistral sign-in expired; run /login again")

// MistralVibeSession is a started Mistral Vibe browser sign-in. Open
// SignInURL in the user's browser, then call Complete to wait for approval
// and receive the resulting API key.
type MistralVibeSession struct {
	signInURL string
	pollURL   string
	processID string
	verifier  string
	expiresAt time.Time
}

// SignInURL is the console page the user signs in on.
func (s *MistralVibeSession) SignInURL() string { return s.signInURL }

// Complete polls the sign-in until the user approves and exchanges the grant
// for an API key. It blocks for up to the session lifetime; ctx cancels it.
func (s *MistralVibeSession) Complete(ctx context.Context) (string, error) {
	exchangeToken, err := s.awaitApproval(ctx)
	if err != nil {
		return "", err
	}
	return s.exchange(ctx, exchangeToken)
}

// StartMistralVibeSignIn begins the Mistral Vibe browser sign-in flow.
func StartMistralVibeSignIn(ctx context.Context) (*MistralVibeSession, error) {
	verifier, challenge, err := mistralVibePKCE()
	if err != nil {
		return nil, err
	}

	status, body, _, err := mistralVibeRequest(ctx, http.MethodPost,
		mistralVibeConsoleURL+mistralVibeAPIPrefix+"/vibe/sign-in",
		map[string]string{"code_challenge": challenge, "code_challenge_method": "S256"})
	if err != nil {
		return nil, fmt.Errorf("failed to start Mistral browser sign-in: %w", err)
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("failed to start Mistral browser sign-in: HTTP %d", status)
	}

	var payload struct {
		ProcessID string `json:"process_id"`
		SignInURL string `json:"sign_in_url"`
		PollURL   string `json:"poll_url"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to start Mistral browser sign-in: invalid sign-in payload: %w", err)
	}
	if strings.TrimSpace(payload.ProcessID) == "" {
		return nil, errors.New("failed to start Mistral browser sign-in: missing process id")
	}
	signInURL, err := mistralVibeCheckedURL(payload.SignInURL, "")
	if err != nil {
		return nil, err
	}
	pollURL, err := mistralVibeCheckedURL(payload.PollURL, mistralVibeAPIPrefix)
	if err != nil {
		return nil, err
	}
	expiresAt, err := mistralVibeParseTime(payload.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("failed to start Mistral browser sign-in: invalid expires_at: %w", err)
	}

	return &MistralVibeSession{
		signInURL: signInURL,
		pollURL:   pollURL,
		processID: payload.ProcessID,
		verifier:  verifier,
		expiresAt: expiresAt,
	}, nil
}

// awaitApproval polls pollURL until the sign-in completes, fails or expires.
// Transient poll failures are tolerated up to mistralVibeMaxPollErrors in a
// row; terminal outcomes (denied, server error, unexpected state, expiry)
// abort immediately.
func (s *MistralVibeSession) awaitApproval(ctx context.Context) (string, error) {
	consecutiveFailures := 0
	for {
		exchangeToken, retryAfter, err := s.poll(ctx)
		if err == nil {
			consecutiveFailures = 0
			if exchangeToken != "" {
				return exchangeToken, nil
			}
		} else {
			var terminal *mistralVibeFlowError
			if errors.Is(err, errMistralVibeExpired) || errors.As(err, &terminal) {
				return "", err
			}
			consecutiveFailures++
			if consecutiveFailures >= mistralVibeMaxPollErrors {
				return "", fmt.Errorf("Mistral sign-in status unavailable: %w", err)
			}
		}

		wait := mistralVibePollInterval
		if retryAfter > 0 {
			wait = retryAfter
		}
		if remaining := time.Until(s.expiresAt); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			return "", errMistralVibeExpired
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

// mistralVibeFlowError marks poll outcomes that abort the flow instead of
// counting toward the tolerated transient failures.
type mistralVibeFlowError struct{ err error }

func (e *mistralVibeFlowError) Error() string { return e.err.Error() }
func (e *mistralVibeFlowError) Unwrap() error { return e.err }

// polls once. A zero exchangeToken with a nil error means "keep waiting";
// retryAfter carries a server-requested poll delay (HTTP 429).
func (s *MistralVibeSession) poll(ctx context.Context) (exchangeToken string, retryAfter time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, mistralVibeRequestTimeout)
	defer cancel()

	status, body, retryAfter, err := mistralVibeRequest(ctx, http.MethodGet, s.pollURL, nil)
	if err != nil {
		return "", 0, err
	}
	if status == http.StatusGone {
		return "", 0, errMistralVibeExpired
	}
	if status == http.StatusTooManyRequests {
		return "", mistralVibeCapped(retryAfter), nil
	}

	var payload struct {
		Status        string `json:"status"`
		ExchangeToken string `json:"exchange_token"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", 0, err
	}
	switch payload.Status {
	case mistralVibePending:
		return "", 0, nil
	case mistralVibeCompleted:
		if strings.TrimSpace(payload.ExchangeToken) == "" {
			return "", 0, &mistralVibeFlowError{err: errors.New("Mistral sign-in completed without an exchange token")}
		}
		return payload.ExchangeToken, 0, nil
	case mistralVibeExpired:
		return "", 0, errMistralVibeExpired
	case mistralVibeDenied:
		return "", 0, &mistralVibeFlowError{err: errors.New("Mistral sign-in was denied")}
	case mistralVibeError:
		message := strings.TrimSpace(payload.Message)
		if message == "" {
			message = "Mistral sign-in failed"
		}
		return "", 0, &mistralVibeFlowError{err: errors.New(message)}
	default:
		return "", 0, &mistralVibeFlowError{err: fmt.Errorf("Mistral sign-in returned unknown state %q", payload.Status)}
	}
}

// exchange trades the approval for the final API key.
func (s *MistralVibeSession) exchange(ctx context.Context, exchangeToken string) (string, error) {
	status, body, _, err := mistralVibeRequest(ctx, http.MethodPost,
		mistralVibeConsoleURL+mistralVibeAPIPrefix+"/vibe/sign-in/"+url.PathEscape(s.processID)+"/exchange",
		map[string]string{"exchange_token": exchangeToken, "code_verifier": s.verifier})
	if err != nil {
		return "", fmt.Errorf("failed to exchange Mistral sign-in for an API key: %w", err)
	}

	var failure struct {
		ErrorCode int    `json:"error_code"`
		Message   string `json:"message"`
	}
	if status == http.StatusForbidden {
		_ = json.Unmarshal(body, &failure)
		if failure.ErrorCode == mistralVibeEntitlementCode {
			return "", errors.New("Your Mistral workspace can't be used for Mistral Vibe; run /login again and pick another workspace in the browser, or ask your administrator")
		}
	}
	if status < 200 || status > 299 {
		return "", fmt.Errorf("failed to exchange Mistral sign-in for an API key: HTTP %d", status)
	}

	var payload struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("failed to exchange Mistral sign-in for an API key: invalid exchange payload: %w", err)
	}
	if strings.TrimSpace(payload.APIKey) == "" {
		return "", errors.New("failed to exchange Mistral sign-in for an API key: no API key returned")
	}
	return payload.APIKey, nil
}

// mistralVibeRequest performs one console request. retryAfter (seconds or
// HTTP-date) is parsed from Retry-After, zero when absent or unparsable.
func mistralVibeRequest(ctx context.Context, method, target string, jsonBody map[string]string) (status int, body []byte, retryAfter time.Duration, err error) {
	var reader io.Reader
	if jsonBody != nil {
		raw, marshalErr := json.Marshal(jsonBody)
		if marshalErr != nil {
			return 0, nil, 0, marshalErr
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := mistralVibeClient.Do(req)
	if err != nil {
		return 0, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, 0, err
	}
	return resp.StatusCode, body, mistralVibeRetryAfter(resp.Header.Get("Retry-After")), nil
}

func mistralVibePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 96)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("failed to start Mistral browser sign-in: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// mistralVibeCheckedURL accepts only console URLs, optionally requiring the
// given path prefix, so responses cannot redirect sign-in elsewhere.
func mistralVibeCheckedURL(raw, pathPrefix string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("unexpected sign-in URL %q", raw)
	}
	console, err := url.Parse(mistralVibeConsoleURL)
	if err != nil || parsed.Scheme != console.Scheme || parsed.Host != console.Host {
		return "", fmt.Errorf("unexpected sign-in URL %q", raw)
	}
	if pathPrefix != "" && parsed.Path != pathPrefix && !strings.HasPrefix(parsed.Path, pathPrefix+"/") {
		return "", fmt.Errorf("unexpected sign-in URL %q", raw)
	}
	return parsed.String(), nil
}

func mistralVibeParseTime(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700"} {
		if t, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable timestamp %q", raw)
}

func mistralVibeRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		return time.Until(at)
	}
	return 0
}

func mistralVibeCapped(after time.Duration) time.Duration {
	if after <= 0 || after > mistralVibeSlowDownCap {
		return mistralVibeSlowDownCap
	}
	return after
}
