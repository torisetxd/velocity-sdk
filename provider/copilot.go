package provider

import (
	"context"
	"strings"
)

type copilotCompatibleProvider struct {
	inner *openAICompatibleProvider
}

func NewCopilotCompatibleProvider(url, apiKey string, extraBody map[string]any, reasoningField string) Provider {
	inner := NewOpenAICompatibleProvider(url, apiKey, extraBody, reasoningField)
	return &copilotCompatibleProvider{inner: inner}
}

func (p *copilotCompatibleProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	return p.inner.FetchModels(ctx)
}

func (p *copilotCompatibleProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	reasoning := OpenAICompatibleReasoningPersistence(p.inner.reasoningField)

	body, err := BuildOpenAIChatBody(req, OpenAIRequestOptions{
		ForceStream:          true,
		IncludeUsage:         true,
		ProviderExtraBody:    p.inner.extraBody,
		RequestExtraBody:     req.ExtraBody,
		ReasoningPersistence: reasoning,
	})
	if err != nil {
		return nil, err
	}

	extraHeaders := copilotHeaders(req.Messages)

	return streamChatCompletions(
		ctx,
		p.inner.httpClient,
		p.inner.apiURL,
		token,
		body,
		extraHeaders,
		streamOpts{ReasoningFields: ReasoningReadFields(firstReasoningField(reasoning))},
	)
}

func (p *copilotCompatibleProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

type copilotResponsesProvider struct {
	inner *responsesProvider
}

func NewCopilotResponsesProvider(apiURL, apiKey string, extraBody map[string]any) Provider {
	inner := NewResponsesProvider(apiURL, apiKey, extraBody)
	return &copilotResponsesProvider{inner: inner}
}

func (p *copilotResponsesProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.inner.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := BuildResponsesBody(req, OpenAIRequestOptions{
		ForceStream:       true,
		IncludeUsage:      true,
		ProviderExtraBody: p.inner.extraBody,
		RequestExtraBody:  req.ExtraBody,
	})
	if err != nil {
		return nil, err
	}

	extraHeaders := copilotHeaders(req.Messages)

	return streamResponses(
		ctx,
		p.inner.httpClient,
		p.inner.apiURL,
		token,
		body,
		extraHeaders,
		"",
	)
}

func (p *copilotResponsesProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func (p *copilotResponsesProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	return p.inner.FetchModels(ctx)
}

func copilotHeaders(messages []ChatMessage) map[string]string {
	isVision := false
	isAgent := false
	if len(messages) > 0 {
		last := messages[len(messages)-1]
		if strings.TrimSpace(last.Role) != "user" {
			isAgent = true
		}
		for _, m := range messages {
			if parts, ok := m.Content.([]map[string]any); ok {
				for _, part := range parts {
					if t, _ := part["type"].(string); t == "image_url" || t == "input_image" {
						isVision = true
						break
					}
				}
			}
			if isVision {
				break
			}
		}
	}

	headers := map[string]string{
		"Openai-Intent": "conversation-edits",
		"X-Initiator":   "user",
		"Content-Type":  "application/json",
	}
	if isAgent {
		headers["X-Initiator"] = "agent"
	}
	if isVision {
		headers["Copilot-Vision-Request"] = "true"
	}
	return headers
}
