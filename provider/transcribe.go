package provider

import (
	"context"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
)

type TranscribeRequest struct {
	Model       string
	Audio       []byte
	Format      string
	Filename    string
	Language    string
	Prompt      string
	Temperature *float64
	ExtraBody   map[string]any
}

type TranscribeUsage struct {
	Seconds      float64  `json:"seconds,omitempty"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	OutputTokens int      `json:"output_tokens,omitempty"`
	TotalTokens  int      `json:"total_tokens,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
}

type TranscribeSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type TranscribeResult struct {
	Text     string              `json:"text"`
	Language string              `json:"language,omitempty"`
	Duration float64             `json:"duration,omitempty"`
	Segments []TranscribeSegment `json:"segments,omitempty"`
	Usage    *TranscribeUsage    `json:"usage,omitempty"`
}

type TranscribeEvent struct {
	Delta  string
	Result *TranscribeResult
}

type Transcriber interface {
	Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error)
}

type TranscribeStreamer interface {
	StreamTranscribe(ctx context.Context, req TranscribeRequest, onEvent func(TranscribeEvent) error) (TranscribeResult, error)
}

func (r TranscribeRequest) validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("transcribe: model is required")
	}
	if len(r.Audio) == 0 {
		return fmt.Errorf("transcribe: audio is empty")
	}
	return nil
}

func (r TranscribeRequest) audioFormat() string {
	if f := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.Format), ".")); f != "" {
		return f
	}
	if ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(r.Filename), ".")); ext != "" {
		return ext
	}
	return "wav"
}

func (r TranscribeRequest) filename() string {
	if name := strings.TrimSpace(r.Filename); name != "" {
		return name
	}
	return "audio." + r.audioFormat()
}

func (r TranscribeRequest) contentType() string {
	if t := mime.TypeByExtension("." + r.audioFormat()); t != "" {
		return t
	}
	return "application/octet-stream"
}
