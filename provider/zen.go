package provider

import "context"

const defaultZenAPIURL = "https://opencode.ai/zen/v1/chat/completions"

type zenProvider struct {
	inner *openAICompatibleProvider
}

func NewZenProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) Provider {
	if apiURL == "" {
		apiURL = defaultZenAPIURL
	}
	inner := NewOpenAICompatibleProvider(apiURL, apiKey, extraBody, reasoningField)
	return &zenProvider{inner: inner}
}

func (p *zenProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	return p.inner.StreamChat(ctx, req)
}

func (p *zenProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	return p.inner.FetchModels(ctx)
}

func (p *zenProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}
