package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type responsesAPIError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

type responsesErrorEnvelope struct {
	Type     string             `json:"type"`
	Error    *responsesAPIError `json:"error"`
	Response *struct {
		Error *responsesAPIError `json:"error"`
	} `json:"response"`
}

func parseResponsesStreamError(payload string) (error, bool) {
	var envelope responsesErrorEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return nil, false
	}

	var apiErr *responsesAPIError
	switch envelope.Type {
	case "error":
		apiErr = envelope.Error
	case "response.failed", "response.incomplete":
		if envelope.Response != nil {
			apiErr = envelope.Response.Error
		}
		if apiErr == nil {
			apiErr = envelope.Error
		}
	default:
		return nil, false
	}

	if apiErr == nil {
		return fmt.Errorf("responses stream ended with %s without error details", envelope.Type), true
	}
	return classifyResponsesAPIError(*apiErr), true
}

func classifyResponsesAPIError(apiErr responsesAPIError) error {
	errorType := strings.ToLower(strings.TrimSpace(apiErr.Type))
	code := strings.ToLower(strings.TrimSpace(apiErr.Code))
	message := strings.TrimSpace(apiErr.Message)
	if message == "" {
		message = code
	}
	if message == "" {
		message = "responses API stream failed"
	}

	if isResponsesRateLimitError(errorType, code) {
		return &RateLimitError{
			StatusCode: http.StatusTooManyRequests,
			Message:    message,
		}
	}

	switch {
	case code == "server_is_overloaded", code == "serverisoverloaded",
		code == "slow_down", errorType == "service_unavailable_error":
		return &ServerError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    message,
		}
	case errorType == "server_error", code == "server_error":
		return &ServerError{
			StatusCode: http.StatusInternalServerError,
			Message:    message,
		}
	default:
		if code != "" {
			return fmt.Errorf("responses API error %s (param=%s): %s", code, apiErr.Param, message)
		}
		return fmt.Errorf("responses API error: %s", message)
	}
}

func isResponsesRateLimitError(errorType, code string) bool {
	if errorType == "rate_limit_error" {
		return true
	}
	switch code {
	case "rate_limit_exceeded", "ratelimitexceeded",
		"rate_limit_error", "too_many_requests",
		"subscription_sharing_usage_limit_exceeded":
		return true
	default:
		return false
	}
}
