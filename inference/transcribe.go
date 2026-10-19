package inference

import (
	"fmt"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

const (
	TranscribeTypeOpenAI     = "openai"
	TranscribeTypeOpenRouter = "openrouter"
	TranscribeTypeChat       = "chat"

	DefaultOpenAITranscribeURL     = "https://api.openai.com/v1"
	DefaultOpenRouterTranscribeURL = "https://openrouter.ai/api/v1"
)

type Transcriber = provider.Transcriber
type TranscribeRequest = provider.TranscribeRequest
type TranscribeResult = provider.TranscribeResult
type TranscribeEvent = provider.TranscribeEvent

type TranscribeConfig struct {
	Type        string               `yaml:"type" json:"type"`
	APIKey      string               `yaml:"api_key" json:"api_key"`
	APIURL      string               `yaml:"api_url" json:"api_url"`
	Prompt      string               `yaml:"prompt" json:"prompt"`
	Chat        *Config              `yaml:"chat" json:"chat"`
	TokenSource provider.TokenSource `yaml:"-" json:"-"`
}

func NewTranscriber(config TranscribeConfig) (Transcriber, error) {
	kind := strings.ToLower(strings.TrimSpace(config.Type))
	apiURL := strings.TrimSpace(config.APIURL)
	if kind == "" || kind == "auto" {
		kind = detectTranscribeType(apiURL, config.Chat)
	}
	switch kind {
	case TranscribeTypeOpenAI:
		if apiURL == "" {
			apiURL = DefaultOpenAITranscribeURL
		}
		t := provider.NewOpenAITranscriber(apiURL, strings.TrimSpace(config.APIKey), nil)
		if config.TokenSource != nil {
			t.SetTokenSource(config.TokenSource)
		}
		return t, nil
	case TranscribeTypeOpenRouter:
		if apiURL == "" {
			apiURL = DefaultOpenRouterTranscribeURL
		}
		t := provider.NewOpenRouterTranscriber(apiURL, strings.TrimSpace(config.APIKey))
		if config.TokenSource != nil {
			t.SetTokenSource(config.TokenSource)
		}
		return t, nil
	case TranscribeTypeChat:
		if config.Chat == nil {
			return nil, fmt.Errorf("chat transcriber requires a chat provider config")
		}
		chat, err := defaultRegistry().New(*config.Chat)
		if err != nil {
			return nil, fmt.Errorf("build chat transcriber: %w", err)
		}
		return provider.NewChatTranscriber(chat, config.Prompt), nil
	default:
		return nil, fmt.Errorf("unsupported transcriber type %q", config.Type)
	}
}

func detectTranscribeType(apiURL string, chat *Config) string {
	if chat != nil {
		return TranscribeTypeChat
	}
	if strings.Contains(strings.ToLower(apiURL), "openrouter.ai") {
		return TranscribeTypeOpenRouter
	}
	return TranscribeTypeOpenAI
}
