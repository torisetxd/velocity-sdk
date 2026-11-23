package provider

import "context"

// DefaultMistralChatURL is the La Plateforme chat-completions endpoint that
// both MISTRAL_API_KEY and Mistral Vibe sign-in keys authenticate.
const DefaultMistralChatURL = "https://api.mistral.ai/v1/chat/completions"

// mistralFallbackModels covers a sign-in whose /v1/models request fails before
// models can be fetched.
var mistralFallbackModels = []ModelInfo{
	{ID: "mistral-medium-latest", Name: "Mistral Medium (latest)", ContextLength: 262144},
	{ID: "mistral-large-latest", Name: "Mistral Large (latest)", ContextLength: 1048576},
	{ID: "mistral-small-latest", Name: "Mistral Small (latest)", ContextLength: 262144},
	{ID: "magistral-medium-latest", Name: "Magistral Medium (latest)", ContextLength: 262144},
	{ID: "codestral-latest", Name: "Codestral (latest)", ContextLength: 256000},
}

type mistralProvider struct {
	inner *openAICompatibleProvider
}

// NewMistralProvider builds the La Plateforme provider. An empty apiURL
// defaults to DefaultMistralChatURL; an empty reasoningField keeps the
// "reasoning_content" default the OpenAI-compatible layer already uses.
func NewMistralProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) Provider {
	if apiURL == "" {
		apiURL = DefaultMistralChatURL
	}
	return &mistralProvider{inner: NewOpenAICompatibleProvider(apiURL, apiKey, extraBody, reasoningField)}
}

func (p *mistralProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	return p.inner.StreamChat(ctx, req)
}

func (p *mistralProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := p.inner.FetchModels(ctx)
	if err != nil || len(models) == 0 {
		return copyModelInfos(mistralFallbackModels), nil
	}
	return models, nil
}

func (p *mistralProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}
