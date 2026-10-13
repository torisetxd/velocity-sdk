package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type openRouterProvider struct {
	base                    *openAICompatibleProvider
	useExplicitCacheMarkers bool
}

func NewOpenRouterProvider(url, apiKey string, useExplicitCacheMarkers bool, extraBody map[string]any) Provider {
	return &openRouterProvider{
		base:                    NewOpenAICompatibleProvider(url, apiKey, extraBody, "reasoning"),
		useExplicitCacheMarkers: useExplicitCacheMarkers,
	}
}

func (p *openRouterProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.base.resolveToken(ctx)
	if err != nil {
		return nil, err
	}

	var transform func([]map[string]any) []map[string]any
	if p.useExplicitCacheMarkers {
		transform = ApplyExplicitCacheMarkers
	}

	reasoning := OpenRouterReasoningPersistence()
	if f := strings.TrimSpace(p.base.reasoningField); f != "" && f != "reasoning" {
		reasoning.TextFields = []string{f}
	}

	body, err := BuildOpenAIChatBody(req, OpenAIRequestOptions{
		ForceStream:          true,
		IncludeUsage:         true,
		MessageTransform:     transform,
		ProviderExtraBody:    p.base.extraBody,
		RequestExtraBody:     req.ExtraBody,
		ReasoningPersistence: reasoning,
	})
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		"HTTP-Referer": "https://velocitylabs.cc",
		"X-Title":      "Velocity",
	}

	reasoningBuffer := make([]map[string]any, 0)

	return streamChatCompletions(
		ctx,
		p.base.httpClient,
		p.base.apiURL,
		token,
		body,
		headers,
		streamOpts{
			ReasoningFields: ReasoningReadFields(firstReasoningField(reasoning)),
			ProcessDelta: func(raw map[string]json.RawMessage, ev *StreamEvent) {
				_ = ev
				delta := raw
				if deltaBytes, ok := raw["delta"]; ok {
					var nested map[string]json.RawMessage
					if err := json.Unmarshal(deltaBytes, &nested); err == nil {
						delta = nested
					}
				}
				if detailsBytes, ok := delta[reasoning.DetailsField]; ok {
					var details []map[string]any
					if err := json.Unmarshal(detailsBytes, &details); err == nil {
						for _, detail := range details {
							appendReasoningDetail(&reasoningBuffer, detail)
						}
					}
				}
			},
			FinalEvent: func() (StreamEvent, bool) {
				if len(reasoningBuffer) == 0 {
					return StreamEvent{}, false
				}
				return StreamEvent{ReasoningDetails: CopyReasoningDetails(reasoningBuffer)}, true
			},
		},
	)
}

func (p *openRouterProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: true,
		SupportsImages:           true,
		SupportsToolCalls:        true,
		SupportsPromptCache:      true,
	}
}

func (p *openRouterProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	return p.base.FetchModels(ctx)
}

func (p *openRouterProvider) FetchUsage(ctx context.Context) (UsageInfo, error) {
	token, err := p.base.resolveToken(ctx)
	if err != nil {
		return UsageInfo{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/credits", nil)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("create credits request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("credits request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UsageInfo{}, fmt.Errorf("credits API error (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return UsageInfo{}, fmt.Errorf("decode credits response: %w", err)
	}
	remaining := payload.Data.TotalCredits - payload.Data.TotalUsage
	return UsageInfo{
		AccountCredits: &remaining,
	}, nil
}
