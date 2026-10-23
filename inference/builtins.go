package inference

import (
	"strings"
	"sync"

	"velocitylabs.cc/sdk/provider"
)

const (
	TypeOpenAICompatible = "openai-compatible"
	TypeOpenRouter       = "openrouter"
	TypeResponses        = "responses"
	TypeCopilot          = "copilot"
	TypeCopilotResponses = "copilot-responses"
	TypeChatGPT          = "chatgpt"
	TypeXAI              = "xai"
	TypeCrof             = "crof"
	TypeZen              = "zen"
	TypeCline            = "cline"
	TypeKimi             = "kimi"

	DefaultOpenAICompatibleURL = "http://localhost:8787/chat/completions"
	DefaultOpenRouterURL       = "https://openrouter.ai/api/v1/chat/completions"
	DefaultResponsesURL        = "https://api.openai.com/v1/responses"
	DefaultXAIURL              = "https://api.x.ai/v1/responses"
	DefaultCrofURL             = "https://crof.ai/v1/chat/completions"
	DefaultZenURL              = "https://opencode.ai/zen/v1/chat/completions"
	DefaultClineURL            = "https://api.cline.bot/api/v1/chat/completions"
	DefaultKimiURL             = "https://api.kimi.com/coding/v1/chat/completions"
)

var (
	builtInRegistryOnce sync.Once
	builtInRegistry     *Registry
)

func NewDefaultRegistry() *Registry {
	registry := NewRegistry()
	registerBuiltins(registry)
	return registry
}

func NewProvider(config Config) (Client, error) {
	return defaultRegistry().New(config)
}

func CanonicalType(raw string) string {
	normalized := normalizeType(raw)
	if canonical := defaultRegistry().CanonicalType(normalized); canonical != "" {
		return canonical
	}
	return normalized
}

// DetectType resolves an explicit provider type or infers it from the URL when
// rawType is empty or "auto".
func DetectType(rawType, apiURL string) string {
	trimmed := normalizeType(rawType)
	if trimmed != "" && trimmed != "auto" {
		return CanonicalType(trimmed)
	}
	lowerURL := strings.ToLower(strings.TrimSpace(apiURL))
	switch {
	case strings.Contains(lowerURL, "openrouter.ai"):
		return TypeOpenRouter
	case strings.Contains(lowerURL, "crof.ai"):
		return TypeCrof
	case strings.Contains(lowerURL, "api.x.ai"):
		return TypeXAI
	case strings.Contains(lowerURL, "opencode.ai/zen"):
		return TypeZen
	case strings.Contains(lowerURL, "api.cline.bot"):
		return TypeCline
	case strings.Contains(lowerURL, "api.kimi.com/coding"), strings.Contains(lowerURL, "api.kimi.ai/coding"):
		return TypeKimi
	case strings.HasSuffix(strings.TrimRight(lowerURL, "/"), "/responses"):
		return TypeResponses
	default:
		return TypeOpenAICompatible
	}
}

func DefaultURLForType(providerType string) string {
	switch CanonicalType(providerType) {
	case TypeOpenRouter:
		return DefaultOpenRouterURL
	case TypeResponses, TypeChatGPT:
		return DefaultResponsesURL
	case TypeXAI:
		return DefaultXAIURL
	case TypeCrof:
		return DefaultCrofURL
	case TypeZen:
		return DefaultZenURL
	case TypeCline:
		return DefaultClineURL
	case TypeKimi:
		return DefaultKimiURL
	default:
		return DefaultOpenAICompatibleURL
	}
}

func defaultRegistry() *Registry {
	builtInRegistryOnce.Do(func() {
		builtInRegistry = NewDefaultRegistry()
	})
	return builtInRegistry
}

func registerBuiltins(registry *Registry) {
	mustRegister(registry, TypeOpenRouter, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultOpenRouterURL
		}
		return provider.NewOpenRouterProvider(config.APIURL, config.APIKey, config.UseExplicitCacheMarkers, config.ExtraBody), nil
	})
	mustRegister(registry, TypeOpenAICompatible, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultOpenAICompatibleURL
		}
		p := provider.NewOpenAICompatibleProvider(config.APIURL, config.APIKey, config.ExtraBody, config.ReasoningField)
		if config.TokenSource != nil {
			p.SetTokenSource(config.TokenSource)
		}
		return p, nil
	}, "openai", "openai_compatible", "llmui")
	mustRegister(registry, TypeResponses, func(config Config) (Client, error) {
		p := provider.NewResponsesProvider(config.APIURL, config.APIKey, config.ExtraBody)
		if config.TokenSource != nil {
			p.SetTokenSource(config.TokenSource)
		}
		return p, nil
	}, "openai-responses")
	mustRegister(registry, TypeCopilot, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultOpenAICompatibleURL
		}
		return provider.NewCopilotCompatibleProvider(config.APIURL, config.APIKey, config.ExtraBody, config.ReasoningField), nil
	}, "github-copilot")
	mustRegister(registry, TypeCopilotResponses, func(config Config) (Client, error) {
		return provider.NewCopilotResponsesProvider(config.APIURL, config.APIKey, config.ExtraBody), nil
	}, "copilot_responses")
	mustRegister(registry, TypeChatGPT, func(config Config) (Client, error) {
		return provider.NewChatGPTProvider(config.APIURL, config.APIKey, config.ExtraBody, config.TokenSource), nil
	})
	mustRegister(registry, TypeXAI, func(config Config) (Client, error) {
		return provider.NewXAIProvider(config.APIURL, config.APIKey, config.ExtraBody, config.TokenSource), nil
	}, "grok")
	mustRegister(registry, TypeCrof, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultCrofURL
		}
		return provider.NewCrofProvider(config.APIURL, config.APIKey, config.ExtraBody, config.ReasoningField), nil
	})
	mustRegister(registry, TypeZen, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultZenURL
		}
		return provider.NewZenProvider(config.APIURL, config.APIKey, config.ExtraBody, config.ReasoningField), nil
	})
	mustRegister(registry, TypeCline, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultClineURL
		}
		return provider.NewClineProvider(config.APIURL, config.APIKey, config.ExtraBody, config.ReasoningField), nil
	})
	mustRegister(registry, TypeKimi, func(config Config) (Client, error) {
		if config.APIURL == "" {
			config.APIURL = DefaultKimiURL
		}
		return provider.NewKimiProvider(config.APIURL, config.APIKey, config.ExtraBody, config.ReasoningField), nil
	}, "kimi-code")
}

func mustRegister(registry *Registry, providerType string, factory Factory, aliases ...string) {
	if err := registry.Register(providerType, factory, aliases...); err != nil {
		panic(err)
	}
}
