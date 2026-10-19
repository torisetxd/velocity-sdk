package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"velocitylabs.cc/sdk"
)

type openRouterTranscriber struct {
	httpClient  *http.Client
	apiURL      string
	apiKey      string
	tokenSource TokenSource
}

func NewOpenRouterTranscriber(apiURL, apiKey string) *openRouterTranscriber {
	return &openRouterTranscriber{
		httpClient: defaultHTTPClient(),
		apiURL:     apiURL,
		apiKey:     apiKey,
	}
}

func (t *openRouterTranscriber) SetTokenSource(ts TokenSource) {
	t.tokenSource = ts
}

func (t *openRouterTranscriber) Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	if err := req.validate(); err != nil {
		return TranscribeResult{}, err
	}
	token := t.apiKey
	if t.tokenSource != nil {
		var err error
		if token, err = t.tokenSource(ctx); err != nil {
			return TranscribeResult{}, err
		}
	}

	payload := map[string]any{
		"model": req.Model,
		"input_audio": map[string]any{
			"data":   base64.StdEncoding.EncodeToString(req.Audio),
			"format": req.audioFormat(),
		},
	}
	if v := strings.TrimSpace(req.Language); v != "" {
		payload["language"] = v
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	for key, value := range req.ExtraBody {
		payload[key] = value
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return TranscribeResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterAudioEndpoint(t.apiURL), bytes.NewReader(body))
	if err != nil {
		return TranscribeResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	settings := sdk.Current()
	if settings.AppURL != "" {
		httpReq.Header.Set("HTTP-Referer", settings.AppURL)
	}
	if settings.AppName != "" {
		httpReq.Header.Set("X-Title", settings.AppName)
	}

	resp, err := doTranscribeRequest(t.httpClient, httpReq)
	if err != nil {
		return TranscribeResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("read transcription response: %w", err)
	}
	return parseOpenAITranscription(raw)
}

func openRouterAudioEndpoint(apiURL string) string {
	base := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	for _, suffix := range []string{"/chat/completions", "/audio/transcriptions"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	return base + "/audio/transcriptions"
}
