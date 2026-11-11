package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const (
	// QwenTokenPlanAPIKeyEnv is the environment variable holding the Qwen Cloud
	// token plan API key. Requests without one fail before reaching the API.
	QwenTokenPlanAPIKeyEnv = "QWEN_TP_API_KEY"
	// DefaultQwenTokenPlanURL is the OpenAI-compatible base URL of the Qwen
	// Cloud token plan.
	DefaultQwenTokenPlanURL = "https://token-plan.maas.qwencloud.com/compatible-mode/v1"
)

const (
	defaultQwenTokenPlanReasoningField = "reasoning_content"
	qwenTokenPlanImageNote             = "[image omitted: this provider accepts text only]"
)

var qwenTokenPlanImagePartTypes = map[string]bool{
	"image_url":   true,
	"image":       true,
	"input_image": true,
}

// qwenTokenPlanProvider speaks the OpenAI chat completions protocol as the
// Qwen Cloud token plan exposes it: thinking is switched on with a top-level
// enable_thinking field and returns in reasoning_content, output is capped with
// max_completion_tokens (thinking plus answer), and requests are text only.
type qwenTokenPlanProvider struct {
	httpClient     *http.Client
	apiURL         string
	apiKey         string
	extraBody      map[string]any
	reasoningField string
}

// NewQwenTokenPlanProvider builds a token plan provider. An empty apiKey falls
// back to QWEN_TP_API_KEY; with neither set it returns an error instead of
// sending an unauthenticated request.
func NewQwenTokenPlanProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) (Provider, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv(QwenTokenPlanAPIKeyEnv))
	}
	if apiKey == "" {
		return nil, fmt.Errorf("qwen token plan requires an API key: set %s (token plan keys start with sk-sp-)", QwenTokenPlanAPIKeyEnv)
	}
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = DefaultQwenTokenPlanURL
	}
	reasoningField = strings.TrimSpace(reasoningField)
	if reasoningField == "" {
		reasoningField = defaultQwenTokenPlanReasoningField
	}

	httpClient := defaultHTTPClient()
	preconnectHTTPClient(httpClient, apiURL, apiKey)
	return &qwenTokenPlanProvider{
		httpClient:     httpClient,
		apiURL:         apiURL,
		apiKey:         apiKey,
		extraBody:      MergeMaps(map[string]any{"enable_thinking": true}, extraBody),
		reasoningField: reasoningField,
	}, nil
}

func (p *qwenTokenPlanProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	messages, err := qwenTokenPlanTextOnly(req.Messages)
	if err != nil {
		return nil, err
	}
	req.Messages = messages

	reasoning := OpenAICompatibleReasoningPersistence(p.reasoningField)
	body, err := BuildOpenAIChatBody(req, OpenAIRequestOptions{
		ForceStream:          true,
		IncludeUsage:         true,
		ProviderExtraBody:    qwenTokenPlanOutputLimit(p.extraBody),
		RequestExtraBody:     qwenTokenPlanOutputLimit(req.ExtraBody),
		ReasoningPersistence: reasoning,
	})
	if err != nil {
		return nil, err
	}

	return streamChatCompletions(
		ctx,
		p.httpClient,
		p.apiURL,
		p.apiKey,
		body,
		nil,
		streamOpts{ReasoningFields: ReasoningReadFields(firstReasoningField(reasoning))},
	)
}

func (p *qwenTokenPlanProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	return FetchModelsFromEndpoint(ctx, p.apiKey, p.apiURL, nil)
}

func (p *qwenTokenPlanProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning: true,
		SupportsToolCalls: true,
	}
}

// qwenTokenPlanOutputLimit rewrites a max_tokens cap into max_completion_tokens,
// which is the only cap the endpoint accepts. An explicit max_completion_tokens
// always wins.
func qwenTokenPlanOutputLimit(body map[string]any) map[string]any {
	maxTokens, ok := body["max_tokens"]
	if !ok {
		return body
	}
	out := CopyMapAny(body)
	delete(out, "max_tokens")
	if _, set := out["max_completion_tokens"]; !set {
		out["max_completion_tokens"] = maxTokens
	}
	return out
}

// qwenTokenPlanTextOnly drops image content parts. Images carried over from
// earlier turns or tool results are removed; an image in the prompt being sent
// fails locally instead of being silently dropped or rejected by the API.
func qwenTokenPlanTextOnly(messages []ChatMessage) ([]ChatMessage, error) {
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) == "user" {
			lastUser = i
			break
		}
	}

	out := make([]ChatMessage, len(messages))
	copy(out, messages)
	for i := range out {
		content, stripped := stripQwenTokenPlanImageParts(out[i].Content)
		if !stripped {
			continue
		}
		if i == lastUser {
			return nil, fmt.Errorf("qwen token plan is text only: remove the attached image or use a vision-capable provider")
		}
		if !hasQwenTokenPlanTextContent(content) {
			content = qwenTokenPlanImageNote
		}
		out[i].Content = content
	}
	return out, nil
}

func stripQwenTokenPlanImageParts(content any) (any, bool) {
	switch parts := content.(type) {
	case []map[string]any:
		kept := make([]map[string]any, 0, len(parts))
		stripped := false
		for _, part := range parts {
			if qwenTokenPlanImagePartTypes[contentPartTypeName(part)] {
				stripped = true
				continue
			}
			kept = append(kept, part)
		}
		if !stripped {
			return content, false
		}
		return kept, true
	case []any:
		kept := make([]any, 0, len(parts))
		stripped := false
		for _, raw := range parts {
			if part, ok := raw.(map[string]any); ok && qwenTokenPlanImagePartTypes[contentPartTypeName(part)] {
				stripped = true
				continue
			}
			kept = append(kept, raw)
		}
		if !stripped {
			return content, false
		}
		return kept, true
	}
	return content, false
}

func hasQwenTokenPlanTextContent(content any) bool {
	switch parts := content.(type) {
	case string:
		return strings.TrimSpace(parts) != ""
	case []map[string]any:
		for _, part := range parts {
			if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
				return true
			}
		}
	case []any:
		for _, raw := range parts {
			if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
				return true
			}
			if part, ok := raw.(map[string]any); ok {
				if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
					return true
				}
			}
		}
	}
	return false
}

func contentPartTypeName(part map[string]any) string {
	partType, _ := part["type"].(string)
	return strings.ToLower(strings.TrimSpace(partType))
}
