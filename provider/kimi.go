package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultKimiAPIURL = "https://api.kimi.com/coding/v1/chat/completions"

var kimiFallbackModels = []ModelInfo{
	{ID: "k3", Name: "K3", ContextLength: 1048576},
	{ID: "k3-256k", Name: "K3 (256K)", ContextLength: 262144},
	{ID: "kimi-for-coding", Name: "K2.8 Preview", ContextLength: 1048576},
	{ID: "kimi-for-coding-highspeed", Name: "K2.7 Code HighSpeed", ContextLength: 262144},
}

type kimiProvider struct {
	inner *openAICompatibleProvider
}

func NewKimiProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) Provider {
	if apiURL == "" {
		apiURL = defaultKimiAPIURL
	}
	if reasoningField == "" {
		reasoningField = "reasoning_content"
	}
	inner := NewOpenAICompatibleProvider(apiURL, apiKey, extraBody, reasoningField)
	inner.emptyToolCallReasoning = true
	return &kimiProvider{inner: inner}
}

func (p *kimiProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	return p.inner.StreamChat(ctx, req)
}

func (p *kimiProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := p.inner.FetchModels(ctx)
	if err != nil || len(models) == 0 {
		return copyModelInfos(kimiFallbackModels), nil
	}
	return models, nil
}

func (p *kimiProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

type kimiUsageCount json.Number

func (c *kimiUsageCount) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "null" {
		text = ""
	}
	*c = kimiUsageCount(text)
	return nil
}

func (c kimiUsageCount) value() (int64, bool) {
	n, err := strconv.ParseInt(string(c), 10, 64)
	return n, err == nil
}

type kimiUsageDetail struct {
	Limit     kimiUsageCount `json:"limit"`
	Used      kimiUsageCount `json:"used"`
	Remaining kimiUsageCount `json:"remaining"`
	ResetTime string         `json:"resetTime"`
}

type kimiUsageResponse struct {
	Usage  *kimiUsageDetail `json:"usage"`
	Limits []struct {
		Window struct {
			Duration int64  `json:"duration"`
			TimeUnit string `json:"timeUnit"`
		} `json:"window"`
		Detail kimiUsageDetail `json:"detail"`
	} `json:"limits"`
}

func (d kimiUsageDetail) reset(window string) (UsageWindowReset, bool) {
	limit, ok := d.Limit.value()
	if !ok || limit <= 0 {
		return UsageWindowReset{}, false
	}
	used, ok := d.Used.value()
	if !ok {
		remaining, remainingOK := d.Remaining.value()
		if !remainingOK {
			return UsageWindowReset{}, false
		}
		used = limit - remaining
	}
	percent := int(used * 100 / limit)
	if percent < 0 {
		percent = 0
	}
	var resetAt int64
	if t, err := time.Parse(time.RFC3339Nano, d.ResetTime); err == nil {
		resetAt = t.Unix()
	}
	return UsageWindowReset{Window: window, UsedPercent: percent, ResetAt: resetAt}, true
}

func kimiWindowLabel(duration int64, unit string) string {
	var minutes int64
	switch unit {
	case "TIME_UNIT_MINUTE":
		minutes = duration
	case "TIME_UNIT_HOUR":
		minutes = duration * 60
	case "TIME_UNIT_DAY":
		minutes = duration * 24 * 60
	default:
		return ""
	}
	switch {
	case minutes <= 0:
		return ""
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func (p *kimiProvider) FetchUsage(ctx context.Context) (UsageInfo, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return UsageInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProviderEndpoint(p.inner.apiURL, "usages"), nil)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("create usage request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return UsageInfo{}, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UsageInfo{}, fmt.Errorf("usage API error (%s)", resp.Status)
	}

	var raw kimiUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return UsageInfo{}, fmt.Errorf("decode usage response: %w", err)
	}

	info := UsageInfo{}
	for _, limit := range raw.Limits {
		label := kimiWindowLabel(limit.Window.Duration, limit.Window.TimeUnit)
		if label == "" {
			continue
		}
		if window, ok := limit.Detail.reset(label); ok {
			info.Resets = append(info.Resets, window)
		}
	}
	if raw.Usage != nil {
		if window, ok := raw.Usage.reset("weekly"); ok {
			info.Resets = append(info.Resets, window)
		}
	}
	if len(info.Resets) == 0 {
		return UsageInfo{}, fmt.Errorf("usage response had no recognised quota windows")
	}
	return info, nil
}
