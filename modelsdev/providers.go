package modelsdev

import (
	"net/url"
	"strings"

	"velocitylabs.cc/sdk/inference"
)

// providerByType maps an inference provider type to the models.dev provider
// that lists the same models.
var providerByType = map[string]string{
	inference.TypeResponses:        "openai",
	inference.TypeChatGPT:          "openai",
	inference.TypeCopilot:          "github-copilot",
	inference.TypeCopilotResponses: "github-copilot",
	inference.TypeAnthropic:        "anthropic",
	inference.TypeClaudeAgent:      "anthropic",
	inference.TypeOpenRouter:       "openrouter",
	inference.TypeXAI:              "xai",
	inference.TypeGemini:           "google",
	inference.TypeVertex:           "google-vertex",
	inference.TypeKimi:             "moonshotai",
	inference.TypeZen:              "opencode",
}

// providerByHost maps an API host to the models.dev provider that serves it,
// for OpenAI-compatible providers the catalog lists under their own name.
var providerByHost = map[string]string{
	"api.openai.com":                    "openai",
	"api.anthropic.com":                 "anthropic",
	"api.x.ai":                          "xai",
	"api.groq.com":                      "groq",
	"api.deepseek.com":                  "deepseek",
	"api.mistral.ai":                    "mistral",
	"api.together.xyz":                  "togetherai",
	"api.fireworks.ai":                  "fireworks-ai",
	"generativelanguage.googleapis.com": "google",
	"aiplatform.googleapis.com":         "google-vertex",
	"api.cerebras.ai":                   "cerebras",
	"openrouter.ai":                     "openrouter",
	"api.moonshot.ai":                   "moonshotai",
	"api.kimi.com":                      "moonshotai",
}

// ProviderID is the models.dev provider ID for an inference provider of the
// given type and API base URL. The provider type wins over the host.
//
// It returns "" when the catalog cannot be attributed with confidence, which
// callers pass to Catalog.Lookup to match the model ID across every provider
// instead.
func ProviderID(providerType, baseURL string) string {
	if id, ok := providerByType[inference.CanonicalType(providerType)]; ok {
		return id
	}
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil {
		if id, ok := providerByHost[strings.ToLower(u.Hostname())]; ok {
			return id
		}
	}
	return ""
}
