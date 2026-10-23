package provider

import "context"

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
