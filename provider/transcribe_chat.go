package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const DefaultChatTranscribePrompt = "Transcribe the audio verbatim. Output only the transcript text with no commentary, labels, or formatting."

type chatTranscriber struct {
	chat   Provider
	prompt string
}

func NewChatTranscriber(chat Provider, prompt string) *chatTranscriber {
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultChatTranscribePrompt
	}
	return &chatTranscriber{chat: chat, prompt: prompt}
}

func (t *chatTranscriber) Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	return t.StreamTranscribe(ctx, req, nil)
}

func (t *chatTranscriber) StreamTranscribe(ctx context.Context, req TranscribeRequest, onEvent func(TranscribeEvent) error) (TranscribeResult, error) {
	if err := req.validate(); err != nil {
		return TranscribeResult{}, err
	}
	if t.chat == nil {
		return TranscribeResult{}, fmt.Errorf("transcribe: no chat provider configured")
	}

	instruction := t.prompt
	if v := strings.TrimSpace(req.Prompt); v != "" {
		instruction += "\n\n" + v
	}
	if v := strings.TrimSpace(req.Language); v != "" {
		instruction += "\n\nThe audio language is: " + v + "."
	}

	stream, err := t.chat.StreamChat(ctx, ChatRequest{
		Model: req.Model,
		Messages: []ChatMessage{{
			Role: "user",
			Content: []map[string]any{
				{"type": "text", "text": instruction},
				{"type": "input_audio", "input_audio": map[string]any{
					"data":   base64.StdEncoding.EncodeToString(req.Audio),
					"format": req.audioFormat(),
				}},
			},
		}},
		ExtraBody: req.ExtraBody,
	})
	if err != nil {
		return TranscribeResult{}, err
	}
	defer stream.Close()

	var text strings.Builder
	var usage *TokenUsage
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return TranscribeResult{}, err
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Content == "" {
			continue
		}
		text.WriteString(ev.Content)
		if onEvent != nil {
			if err := onEvent(TranscribeEvent{Delta: ev.Content}); err != nil {
				return TranscribeResult{}, err
			}
		}
	}

	result := TranscribeResult{Text: strings.TrimSpace(text.String())}
	if usage != nil {
		result.Usage = &TranscribeUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
		}
	}
	if onEvent != nil {
		if err := onEvent(TranscribeEvent{Result: &result}); err != nil {
			return TranscribeResult{}, err
		}
	}
	return result, nil
}
