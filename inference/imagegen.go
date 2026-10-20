package inference

import (
	"fmt"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

const (
	ImageTypeOpenAI     = "openai"
	ImageTypeOpenRouter = "openrouter"

	DefaultOpenAIImageURL     = "https://api.openai.com/v1"
	DefaultOpenRouterImageURL = "https://openrouter.ai/api/v1"
)

type ImageGenerator = provider.ImageGenerator
type ImageRequest = provider.ImageRequest
type ImageResult = provider.ImageResult
type ImageInput = provider.ImageInput

type ImageConfig struct {
	// Type is "openai" (/images/generations and /images/edits), "openrouter"
	// (image output through /chat/completions) or "auto" to pick by APIURL.
	Type        string               `yaml:"type" json:"type"`
	APIKey      string               `yaml:"api_key" json:"api_key"`
	APIURL      string               `yaml:"api_url" json:"api_url"`
	TokenSource provider.TokenSource `yaml:"-" json:"-"`
}

func NewImageGenerator(config ImageConfig) (ImageGenerator, error) {
	kind := strings.ToLower(strings.TrimSpace(config.Type))
	apiURL := strings.TrimSpace(config.APIURL)
	if kind == "" || kind == "auto" {
		kind = ImageTypeOpenAI
		if strings.Contains(strings.ToLower(apiURL), "openrouter.ai") {
			kind = ImageTypeOpenRouter
		}
	}
	switch kind {
	case ImageTypeOpenAI:
		if apiURL == "" {
			apiURL = DefaultOpenAIImageURL
		}
		g := provider.NewOpenAIImageGenerator(apiURL, strings.TrimSpace(config.APIKey))
		if config.TokenSource != nil {
			g.SetTokenSource(config.TokenSource)
		}
		return g, nil
	case ImageTypeOpenRouter:
		if apiURL == "" {
			apiURL = DefaultOpenRouterImageURL
		}
		g := provider.NewOpenRouterImageGenerator(apiURL, strings.TrimSpace(config.APIKey))
		if config.TokenSource != nil {
			g.SetTokenSource(config.TokenSource)
		}
		return g, nil
	default:
		return nil, fmt.Errorf("unsupported image generator type %q", config.Type)
	}
}
