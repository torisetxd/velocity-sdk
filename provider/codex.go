package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const DefaultCodexURL = "https://chatgpt.com/backend-api/codex/responses"

// NewCodexProvider uses the legacy Codex subscription endpoint. Authentication
// and refresh remain the host's responsibility via TokenSource.
func NewCodexProvider(apiURL, apiKey string, extra map[string]any, source ...TokenSource) Provider {
	if strings.TrimSpace(apiURL) == "" {
		apiURL = DefaultCodexURL
	}
	p := NewChatGPTProvider(apiURL, apiKey, extra, source...).(*chatgptProvider)
	p.legacy = true
	return p
}
func enableCodexReasoningPersistence(body []byte) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	for _, key := range []string{"max_output_tokens", "temperature", "top_p", "previous_response_id", "prompt_cache_retention", "truncation"} {
		delete(payload, key)
	}
	payload["store"], payload["stream"] = false, true
	include, _ := payload["include"].([]any)
	found := false
	for _, v := range include {
		if v == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		payload["include"] = append(include, "reasoning.encrypted_content")
	}
	if _, ok := payload["instructions"]; !ok {
		payload["instructions"] = ""
	}
	if _, ok := payload["input"].([]any); !ok {
		return nil, fmt.Errorf("Codex requires an input array")
	}
	return payload, nil
}
func addCodexHeaders(headers map[string]string, token string) {
	headers["originator"] = "codex_cli_rs"
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if id, ok := auth["chatgpt_account_id"].(string); ok {
		headers["chatgpt-account-id"] = id
	}
}

// codexTurnMetadata carries logical identity independently of cache routing.
func codexTurnMetadata(req ChatRequest, affinity string) string {
	if strings.TrimSpace(req.CodexTurnMetadata) != "" {
		return req.CodexTurnMetadata
	}
	metadata := map[string]string{"session_id": affinity, "thread_id": affinity}
	if req.TurnID != "" {
		metadata["turn_id"] = req.TurnID
	}
	window := req.CodexWindowID
	if window == "" {
		window = affinity
	}
	metadata["window_id"] = window
	if req.CodexParentThreadID != "" {
		metadata["parent_thread_id"] = req.CodexParentThreadID
	}
	data, _ := json.Marshal(metadata)
	return string(data)
}
