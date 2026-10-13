package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultCrofAPIURL = "https://crof.ai/v1/chat/completions"
const crofUsageAPIURL = "https://crof.ai/usage_api/"

type crofProvider struct {
	inner *openAICompatibleProvider
}

func NewCrofProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) Provider {
	if apiURL == "" {
		apiURL = defaultCrofAPIURL
	}
	if reasoningField == "" {
		reasoningField = "reasoning_content"
	}
	inner := NewOpenAICompatibleProvider(apiURL, apiKey, extraBody, reasoningField)
	return &crofProvider{inner: inner}
}

func (p *crofProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	return p.inner.StreamChat(ctx, req)
}

func (p *crofProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	return p.inner.FetchModels(ctx)
}

func (p *crofProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func (p *crofProvider) FetchUsage(ctx context.Context) (UsageInfo, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return UsageInfo{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, crofUsageAPIURL, nil)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("create usage request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UsageInfo{}, fmt.Errorf("usage API error (%s)", resp.Status)
	}

	var raw struct {
		UsableRequests *int     `json:"usable_requests"`
		Credits        *float64 `json:"credits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return UsageInfo{}, fmt.Errorf("decode usage response: %w", err)
	}

	info := UsageInfo{}
	if raw.UsableRequests != nil {
		v := *raw.UsableRequests
		info.UsableRequests = &v
	}
	if raw.Credits != nil {
		v := *raw.Credits
		info.Credits = &v
	}
	return info, nil
}
