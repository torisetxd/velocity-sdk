package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	xaiResponsesURL       = "https://api.x.ai/v1/responses"
	xaiLanguageModelsURL  = "https://api.x.ai/v1/language-models"
	xaiResponsesOutputKey = "xai_responses_output_v1"
)

var xaiFallbackModels = []ModelInfo{{ID: "grok-4.5", Name: "Grok 4.5"}}

type xaiProvider struct {
	inner *responsesProvider
}

func NewXAIProvider(apiURL, apiKey string, extraBody map[string]any, tokenSource ...TokenSource) Provider {
	if strings.TrimSpace(apiURL) == "" {
		apiURL = xaiResponsesURL
	}
	inner := NewResponsesProvider(apiURL, apiKey, extraBody)
	if len(tokenSource) > 0 && tokenSource[0] != nil {
		inner.SetTokenSource(tokenSource[0])
	}
	return &xaiProvider{inner: inner}
}

func (p *xaiProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := BuildResponsesBody(req, OpenAIRequestOptions{
		ForceStream:            true,
		IncludeUsage:           true,
		ProviderExtraBody:      p.inner.extraBody,
		RequestExtraBody:       req.ExtraBody,
		ResponsesOutputMetaKey: xaiResponsesOutputKey,
	})
	if err != nil {
		return nil, err
	}
	body, err = enableXAIReasoningPersistence(body)
	if err != nil {
		return nil, err
	}
	return streamResponses(
		ctx,
		p.inner.httpClient,
		p.inner.apiURL,
		token,
		body,
		nil,
		xaiResponsesOutputKey,
	)
}

func enableXAIReasoningPersistence(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode xAI Responses request: %w", err)
	}
	payload["store"] = false

	include := []any{}
	if current, ok := payload["include"].([]any); ok {
		include = append(include, current...)
	}
	const encryptedReasoning = "reasoning.encrypted_content"
	found := false
	for _, item := range include {
		if value, ok := item.(string); ok && value == encryptedReasoning {
			found = true
			break
		}
	}
	if !found {
		include = append(include, encryptedReasoning)
	}
	payload["include"] = include

	return json.Marshal(payload)
}

func (p *xaiProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func (p *xaiProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return copyModelInfos(xaiFallbackModels), nil
	}
	if models, err := p.fetchLanguageModels(ctx, token); err == nil && len(models) > 0 {
		return preferXAIModel(models, "grok-4.5"), nil
	}
	if models, err := FetchModelsFromEndpoint(ctx, token, p.inner.apiURL, nil); err == nil && len(models) > 0 {
		return preferXAIModel(models, "grok-4.5"), nil
	}
	return copyModelInfos(xaiFallbackModels), nil
}

func (p *xaiProvider) fetchLanguageModels(ctx context.Context, accessToken string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiLanguageModelsURL, nil)
	if err != nil {
		return nil, err
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.inner.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xAI models API error (%d)", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			ID      string   `json:"id"`
			Aliases []string `json:"aliases"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	models := make([]ModelInfo, 0, len(payload.Models))
	appendModel := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, ModelInfo{ID: id, Name: id})
	}
	for _, model := range payload.Models {
		appendModel(model.ID)
		for _, alias := range model.Aliases {
			appendModel(alias)
		}
	}
	return models, nil
}

func preferXAIModel(models []ModelInfo, preferred string) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if model.ID == preferred {
			out = append(out, model)
			break
		}
	}
	for _, model := range models {
		if model.ID != preferred {
			out = append(out, model)
		}
	}
	return out
}
