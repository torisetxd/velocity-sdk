package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	defaultClineAPIURL = "https://api.cline.bot/api/v1/chat/completions"
	clineModelsAPIURL  = "https://api.cline.bot/api/v1/ai/cline/recommended-models"
)

var clineFallbackModels = []ModelInfo{
	{ID: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash (free)"},
	{ID: "poolside/laguna-s-2.1:free", Name: "Laguna S 2.1 (free)"},
	{ID: "stepfun/step-3.7-flash", Name: "Step 3.7 Flash (free)"},
}

type clineModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type clineProvider struct {
	inner *openAICompatibleProvider
}

func NewClineProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) Provider {
	if apiURL == "" {
		apiURL = defaultClineAPIURL
	}
	headers := map[string]string{"X-CLIENT-TYPE": "cline-cli"}
	inner := NewOpenAICompatibleProviderWithHeaders(apiURL, apiKey, extraBody, reasoningField, headers)
	return &clineProvider{inner: inner}
}

func (p *clineProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	return p.inner.StreamChat(ctx, req)
}

func (p *clineProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := fetchClineRecommendedModels(ctx)
	if err != nil {
		return copyModelInfos(clineFallbackModels), nil
	}
	return models, nil
}

func (p *clineProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func fetchClineRecommendedModels(ctx context.Context) ([]ModelInfo, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clineModelsAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cline models request failed: %s", resp.Status)
	}

	var payload struct {
		Recommended []clineModel `json:"recommended"`
		Free        []clineModel `json:"free"`
		ClinePass   []clineModel `json:"clinePass"`
		ClineCloud  []clineModel `json:"clineCloud"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, len(payload.Recommended)+len(payload.Free)+len(payload.ClinePass)+len(payload.ClineCloud))
	appendModels := func(list []clineModel) {
		for _, model := range list {
			if id := strings.TrimSpace(model.ID); id != "" {
				models = append(models, ModelInfo{ID: id, Name: strings.TrimSpace(model.Name)})
			}
		}
	}

	appendModels(payload.Free)
	appendModels(payload.Recommended)
	appendModels(payload.ClinePass)
	appendModels(payload.ClineCloud)

	return models, nil
}
