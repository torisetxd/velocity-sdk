package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"velocitylabs.cc/sdk/internal/debugtrace"
)

const (
	chatGPTResponsesOutputKey = "chatgpt_responses_output_v1"
	chatGPTModelsCatalogURL   = "https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json"
)

type chatgptProvider struct {
	inner    *responsesProvider
	turns    chatGPTTurnCache
	warnOnce sync.Once
}

// NewChatGPTProvider constructs a ChatGPT Responses provider. Requests are
// stateless apart from the sticky-routing token the route returns for the
// current turn. Credentials/tokens are supplied by the host application via
// apiKey or an optional TokenSource.
func NewChatGPTProvider(apiURL, apiKey string, extraBody map[string]any, tokenSource ...TokenSource) Provider {
	inner := NewResponsesProvider(apiURL, apiKey, extraBody)
	if len(tokenSource) > 0 && tokenSource[0] != nil {
		inner.SetTokenSource(tokenSource[0])
	}
	return &chatgptProvider{inner: inner}
}

func (p *chatgptProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := BuildResponsesBody(translateSystemToDeveloper(req), OpenAIRequestOptions{
		ForceStream:            true,
		ProviderExtraBody:      p.inner.extraBody,
		RequestExtraBody:       req.ExtraBody,
		ResponsesOutputMetaKey: chatGPTResponsesOutputKey,
	})
	if err != nil {
		return nil, err
	}
	payload, err := enableChatGPTStatelessReasoningPersistence(body)
	if err != nil {
		return nil, err
	}
	affinity, err := chatGPTAffinityKey(req.PromptCacheKey, payload)
	if err != nil {
		return nil, err
	}
	payload["prompt_cache_key"] = affinity
	body, err = json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode ChatGPT Responses request: %w", err)
	}

	turnState := p.turns.get(req.TurnID, token)
	trace := chatGPTRequestTrace{
		sentSessionID: affinity,
		sentCacheKey:  affinity,
		turnStateSent: turnState != "",
		turnStateLen:  len(turnState),
	}
	observer := &responsesObserver{
		Headers: func(streamID string, header http.Header) {
			logChatGPTResponseCacheHeaders(streamID, header)
			state := strings.TrimSpace(header.Get(chatGPTTurnStateHeader))
			trace.turnStateSeen, trace.turnStateNewLen = state != "", len(state)
			p.turns.put(req.TurnID, token, state)
		},
		Complete: func(echoedPromptCacheKey string, usage *TokenUsage) {
			trace.echoedCacheKey = echoedPromptCacheKey
			if usage != nil {
				trace.cachedTokens, trace.inputTokens = usage.CachedTokens, usage.InputTokens
			}
			trace.log()
			p.warnAffinityUnhonored(affinity, echoedPromptCacheKey)
		},
	}
	stream, err := streamResponses(ctx, p.inner.httpClient, p.inner.apiURL, token, body, chatGPTRequestHeaders(req, affinity, turnState), chatGPTResponsesOutputKey, observer)
	if err != nil {
		return nil, err
	}
	if s, ok := stream.(*responsesStream); ok {
		s.requireCompletion = true
	}
	return stream, nil
}

func (p *chatgptProvider) warnAffinityUnhonored(sent, echoed string) {
	if echoed == "" || echoed == sent {
		return
	}
	p.warnOnce.Do(func() {
		debugtrace.Log("provider.affinity_unhonored", map[string]any{
			"sent_prompt_cache_key":   sent,
			"echoed_prompt_cache_key": echoed,
		})
		fmt.Fprintln(os.Stderr, "affinity header not honored on this route")
	})
}

func (p *chatgptProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:           true,
		SupportsImages:              true,
		SupportsToolCalls:           true,
		SupportsPromptCache:         true,
		PromptCacheEnabledByDefault: true,
	}
}

func (p *chatgptProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatGPTModelsCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var catalog struct {
				Models []struct {
					Slug        string `json:"slug"`
					DisplayName string `json:"display_name"`
				} `json:"models"`
			}
			if json.NewDecoder(resp.Body).Decode(&catalog) == nil && len(catalog.Models) > 0 {
				models := make([]ModelInfo, 0, len(catalog.Models))
				for _, m := range catalog.Models {
					if slug := strings.TrimSpace(m.Slug); slug != "" {
						models = append(models, ModelInfo{ID: slug, Name: strings.TrimSpace(m.DisplayName)})
					}
				}
				if len(models) > 0 {
					return models, nil
				}
			}
		}
	}
	return p.inner.FetchModels(ctx)
}

// enableChatGPTStatelessReasoningPersistence normalizes a Responses request for
// the subscription route and returns the decoded payload so the caller can
// derive the cache affinity from it before encoding.
func enableChatGPTStatelessReasoningPersistence(body []byte) (map[string]any, error) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil, fmt.Errorf("invalid ChatGPT Responses request")
	}
	for _, key := range []string{
		"background", "conversation", "max_output_tokens", "max_tool_calls",
		"metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention",
		"safety_identifier", "temperature", "top_logprobs", "top_p", "truncation",
		"user", "previous_response_id",
	} {
		delete(payload, key)
	}
	payload["store"], payload["stream"] = false, true
	include, _ := payload["include"].([]any)
	found := false
	for _, value := range include {
		if value == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		payload["include"] = append(include, "reasoning.encrypted_content")
	}
	if _, ok := payload["input"].([]any); !ok {
		return nil, fmt.Errorf("ChatGPT Responses requires conversation context as an input array")
	}
	for _, raw := range payload["input"].([]any) {
		if item, ok := raw.(map[string]any); ok {
			if item["role"] == "system" {
				item["role"] = "developer"
			}
			if item["type"] == "function_call" && item["namespace"] == nil {
				item["namespace"] = "functions"
			}
		}
	}
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		local := make([]any, 0, len(tools))
		supported := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ChatGPT route supports function/custom tools, not hosted tools")
			}
			switch tool["type"] {
			case "function", "custom":
				local = append(local, tool)
			case "namespace":
				nested, ok := tool["tools"].([]any)
				if !ok {
					return nil, fmt.Errorf("invalid ChatGPT tool namespace")
				}
				for _, entry := range nested {
					child, ok := entry.(map[string]any)
					if !ok || (child["type"] != "function" && child["type"] != "custom") {
						return nil, fmt.Errorf("ChatGPT tool namespaces require function/custom tools")
					}
				}
				supported = append(supported, tool)
			case "web_search":
				supported = append(supported, tool)
			default:
				return nil, fmt.Errorf("unsupported ChatGPT hosted tool: %v", tool["type"])
			}
		}
		if len(local) > 0 {
			supported = append(supported, map[string]any{
				"type":        "namespace",
				"name":        "functions",
				"description": "Local harness tools",
				"tools":       local,
			})
		}
		payload["tools"] = supported
	}
	return payload, nil
}

func translateSystemToDeveloper(req ChatRequest) ChatRequest {
	out := req
	out.Messages = make([]ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		out.Messages[i] = m
		if strings.EqualFold(m.Role, "system") {
			out.Messages[i].Role = "developer"
		}
	}
	return out
}
