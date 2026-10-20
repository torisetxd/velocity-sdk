package inference

import (
	"fmt"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

const DefaultOpenAISpeechURL = "https://api.openai.com/v1"

type Speaker = provider.Speaker
type SpeechRequest = provider.SpeechRequest
type SpeechResult = provider.SpeechResult

type SpeechConfig struct {
	// Type is "openai" (POST /audio/speech, also used by many local TTS servers).
	Type        string               `yaml:"type" json:"type"`
	APIKey      string               `yaml:"api_key" json:"api_key"`
	APIURL      string               `yaml:"api_url" json:"api_url"`
	TokenSource provider.TokenSource `yaml:"-" json:"-"`
}

func NewSpeaker(config SpeechConfig) (Speaker, error) {
	kind := strings.ToLower(strings.TrimSpace(config.Type))
	if kind == "" || kind == "auto" {
		kind = "openai"
	}
	if kind != "openai" {
		return nil, fmt.Errorf("unsupported speaker type %q", config.Type)
	}
	apiURL := strings.TrimSpace(config.APIURL)
	if apiURL == "" {
		apiURL = DefaultOpenAISpeechURL
	}
	s := provider.NewOpenAISpeaker(apiURL, strings.TrimSpace(config.APIKey))
	if config.TokenSource != nil {
		s.SetTokenSource(config.TokenSource)
	}
	return s, nil
}
