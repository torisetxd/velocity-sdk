package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxSpeechBytes = 64 << 20

type SpeechRequest struct {
	Model string
	Input string
	Voice string
	// Format is the audio container: "mp3" (default), "wav", "opus", "aac", "flac", "pcm".
	Format string
	Speed  *float64
	// Instructions steers delivery on models that support it (e.g. gpt-4o-mini-tts).
	Instructions string
	ExtraBody    map[string]any
}

type SpeechResult struct {
	Audio    []byte
	Format   string
	MIMEType string
}

// Speaker turns text into speech.
type Speaker interface {
	Speak(ctx context.Context, req SpeechRequest) (SpeechResult, error)
}

// SpeechStreamer is implemented by speakers that can hand over audio as it is
// produced, so playback can start before synthesis finishes.
type SpeechStreamer interface {
	StreamSpeech(ctx context.Context, req SpeechRequest, w io.Writer) (SpeechResult, error)
}

func (r SpeechRequest) validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("speech: model is required")
	}
	if strings.TrimSpace(r.Input) == "" {
		return fmt.Errorf("speech: input is empty")
	}
	return nil
}

func (r SpeechRequest) format() string {
	if f := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.Format), ".")); f != "" {
		return f
	}
	return "mp3"
}

func speechMIME(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "pcm":
		return "audio/L16"
	default:
		return "application/octet-stream"
	}
}

// openAISpeaker calls an OpenAI-style POST /audio/speech endpoint, which many
// local TTS servers also implement.
type openAISpeaker struct {
	httpClient  *http.Client
	apiURL      string
	apiKey      string
	tokenSource TokenSource
}

func NewOpenAISpeaker(apiURL, apiKey string) *openAISpeaker {
	return &openAISpeaker{httpClient: defaultHTTPClient(), apiURL: apiURL, apiKey: apiKey}
}

func (s *openAISpeaker) SetTokenSource(ts TokenSource) { s.tokenSource = ts }

func (s *openAISpeaker) Speak(ctx context.Context, req SpeechRequest) (SpeechResult, error) {
	var buf bytes.Buffer
	res, err := s.StreamSpeech(ctx, req, &limitedWriter{w: &buf, left: maxSpeechBytes})
	if err != nil {
		return SpeechResult{}, err
	}
	res.Audio = buf.Bytes()
	return res, nil
}

func (s *openAISpeaker) StreamSpeech(ctx context.Context, req SpeechRequest, w io.Writer) (SpeechResult, error) {
	if err := req.validate(); err != nil {
		return SpeechResult{}, err
	}
	format := req.format()
	payload := map[string]any{
		"model":           req.Model,
		"input":           req.Input,
		"response_format": format,
	}
	if v := strings.TrimSpace(req.Voice); v != "" {
		payload["voice"] = v
	} else {
		payload["voice"] = "alloy"
	}
	if req.Speed != nil {
		payload["speed"] = *req.Speed
	}
	if v := strings.TrimSpace(req.Instructions); v != "" {
		payload["instructions"] = v
	}
	for k, v := range req.ExtraBody {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SpeechResult{}, err
	}

	base := strings.TrimRight(strings.TrimSpace(s.apiURL), "/")
	base = strings.TrimSuffix(base, "/audio/speech")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return SpeechResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := setBearerAuth(ctx, httpReq, s.apiKey, s.tokenSource); err != nil {
		return SpeechResult{}, err
	}
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return SpeechResult{}, fmt.Errorf("speech request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxImageErrorBytes))
		return SpeechResult{}, fmt.Errorf("speech API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return SpeechResult{}, fmt.Errorf("read speech response: %w", err)
	}
	mimeType := speechMIME(format)
	if ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]); strings.HasPrefix(ct, "audio/") {
		mimeType = ct
	}
	return SpeechResult{Format: format, MIMEType: mimeType}, nil
}

// limitedWriter fails once more than left bytes are written.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.left {
		return 0, fmt.Errorf("speech audio exceeds %d bytes", maxSpeechBytes)
	}
	l.left -= len(p)
	return l.w.Write(p)
}
