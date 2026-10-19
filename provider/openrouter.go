package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"velocitylabs.cc/sdk"
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

func (p *openRouterProvider) SetToolResultImagesAsUser(enabled bool) {
	p.base.SetToolResultImagesAsUser(enabled)
}

func (p *openRouterProvider) SetTokenSource(ts TokenSource) {
	p.base.SetTokenSource(ts)
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

	body, err := BuildOpenAIChatBody(req, OpenAIRequestOptions{
		ForceStream:            true,
		IncludeUsage:           true,
		MessageTransform:       transform,
		ProviderExtraBody:      p.base.extraBody,
		RequestExtraBody:       req.ExtraBody,
		ReasoningPersistence:   reasoning,
		ToolResultImagesAsUser: p.base.toolResultImagesAsUser,
	})
	if err != nil {
		return nil, err
	}

	headers := map[string]string{}
	settings := sdk.Current()
	if settings.AppURL != "" {
		headers["HTTP-Referer"] = settings.AppURL
	}
	if settings.AppName != "" {
		headers["X-Title"] = settings.AppName
	}

	return streamChatCompletions(
		ctx,
		p.base.httpClient,
		p.base.apiURL,
		token,
		body,
		headers,
		streamOpts{
			ReasoningFields: ReasoningReadFields(firstReasoningField(reasoning)),
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

// FetchModels lists every model type. OpenRouter's /models only returns
// text-output models unless output_modalities is given, which would hide
// embedding, transcription and other models.
func (p *openRouterProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	token, err := p.base.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	return fetchModelList(ctx, token, p.base.apiURL, p.base.extraHeaders, "output_modalities=all")
}

func (p *openRouterProvider) FetchUsage(ctx context.Context) (UsageInfo, error) {
	token, err := p.base.resolveToken(ctx)
	if err != nil {
		return UsageInfo{}, err
	}
	if token == "" {
		return UsageInfo{}, fmt.Errorf("OpenRouter API key is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/key", nil)
	if err != nil {
		return UsageInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.base.httpClient.Do(req)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("OpenRouter usage request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return UsageInfo{}, fmt.Errorf("OpenRouter usage API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Data *struct {
			LimitRemaining *float64 `json:"limit_remaining"`
			LimitReset     string   `json:"limit_reset"`
			UsageDaily     float64  `json:"usage_daily"`
			UsageWeekly    float64  `json:"usage_weekly"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return UsageInfo{}, fmt.Errorf("decode OpenRouter usage: %w", err)
	}
	if result.Data == nil {
		return UsageInfo{}, fmt.Errorf("OpenRouter usage response has no key data")
	}
	data := result.Data
	info := UsageInfo{Label: fmt.Sprintf("OpenRouter: $%.2f today · $%.2f week", data.UsageDaily, data.UsageWeekly)}
	if data.LimitRemaining != nil {
		remaining := *data.LimitRemaining
		info.Credits = &remaining
		info.CreditsLabel = "key cap left"
		if remaining <= 0 {
			info.Resets = []UsageWindowReset{{Window: "Key limit", UsedPercent: 100, ResetAt: openRouterResetAt(data.LimitReset)}}
		}
	}
	if balance, err := p.fetchOpenRouterBalance(ctx, token); err == nil {
		info.AccountCredits = &balance
	}
	return info, nil
}

func (p *openRouterProvider) fetchOpenRouterBalance(ctx context.Context, token string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/credits", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.base.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("OpenRouter credits API error (%d)", resp.StatusCode)
	}
	var result struct {
		Data *struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return 0, err
	}
	if result.Data == nil {
		return 0, fmt.Errorf("OpenRouter credits response has no data")
	}
	return result.Data.TotalCredits - result.Data.TotalUsage, nil
}

func openRouterResetAt(window string) int64 {
	now := time.Now().UTC()
	switch strings.ToLower(window) {
	case "daily":
		return now.Truncate(24 * time.Hour).Add(24 * time.Hour).Unix()
	case "weekly":
		days := (8 - int(now.Weekday())) % 7
		if days == 0 {
			days = 7
		}
		return now.Truncate(24*time.Hour).AddDate(0, 0, days).Unix()
	case "monthly":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	return 0
}
