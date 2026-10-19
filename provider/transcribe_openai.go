package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

const maxTranscribeErrorBytes = 4096

type openAITranscriber struct {
	httpClient   *http.Client
	apiURL       string
	apiKey       string
	tokenSource  TokenSource
	extraHeaders map[string]string
}

func NewOpenAITranscriber(apiURL, apiKey string, extraHeaders map[string]string) *openAITranscriber {
	return &openAITranscriber{
		httpClient:   defaultHTTPClient(),
		apiURL:       apiURL,
		apiKey:       apiKey,
		extraHeaders: copyHeaders(extraHeaders),
	}
}

func (t *openAITranscriber) SetTokenSource(ts TokenSource) {
	t.tokenSource = ts
}

func (t *openAITranscriber) token(ctx context.Context) (string, error) {
	if t.tokenSource != nil {
		return t.tokenSource(ctx)
	}
	return t.apiKey, nil
}

func (t *openAITranscriber) Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	resp, err := t.do(ctx, req, false)
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

func (t *openAITranscriber) StreamTranscribe(ctx context.Context, req TranscribeRequest, onEvent func(TranscribeEvent) error) (TranscribeResult, error) {
	resp, err := t.do(ctx, req, true)
	if err != nil {
		return TranscribeResult{}, err
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return TranscribeResult{}, fmt.Errorf("read transcription response: %w", err)
		}
		result, err := parseOpenAITranscription(raw)
		if err != nil {
			return TranscribeResult{}, err
		}
		if onEvent != nil {
			if err := onEvent(TranscribeEvent{Delta: result.Text, Result: &result}); err != nil {
				return TranscribeResult{}, err
			}
		}
		return result, nil
	}

	var text strings.Builder
	var final *TranscribeResult
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string          `json:"type"`
			Delta string          `json:"delta"`
			Text  string          `json:"text"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "transcript.text.delta":
			text.WriteString(event.Delta)
			if onEvent != nil {
				if err := onEvent(TranscribeEvent{Delta: event.Delta}); err != nil {
					return TranscribeResult{}, err
				}
			}
		case "transcript.text.done":
			result := TranscribeResult{Text: event.Text, Usage: parseTranscribeUsage(event.Usage)}
			final = &result
		}
	}
	if err := scanner.Err(); err != nil {
		return TranscribeResult{}, fmt.Errorf("read transcription stream: %w", err)
	}
	if final == nil {
		final = &TranscribeResult{Text: text.String()}
	}
	if onEvent != nil {
		if err := onEvent(TranscribeEvent{Result: final}); err != nil {
			return TranscribeResult{}, err
		}
	}
	return *final, nil
}

func (t *openAITranscriber) do(ctx context.Context, req TranscribeRequest, stream bool) (*http.Response, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	token, err := t.token(ctx)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, req.filename()))
	header.Set("Content-Type", req.contentType())
	part, err := form.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(req.Audio); err != nil {
		return nil, err
	}
	fields := map[string]string{"model": req.Model}
	if v := strings.TrimSpace(req.Language); v != "" {
		fields["language"] = v
	}
	if v := strings.TrimSpace(req.Prompt); v != "" {
		fields["prompt"] = v
	}
	if req.Temperature != nil {
		fields["temperature"] = strconv.FormatFloat(*req.Temperature, 'f', -1, 64)
	}
	if stream {
		fields["stream"] = "true"
	}
	for key, value := range req.ExtraBody {
		fields[key] = fmt.Sprint(value)
	}
	for key, value := range fields {
		if err := form.WriteField(key, value); err != nil {
			return nil, err
		}
	}
	if err := form.Close(); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ProviderEndpoint(t.apiURL, "audio/transcriptions"), &body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", form.FormDataContentType())
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range t.extraHeaders {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			httpReq.Header.Set(key, value)
		}
	}
	return doTranscribeRequest(t.httpClient, httpReq)
}

func doTranscribeRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transcription request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxTranscribeErrorBytes))
		return nil, fmt.Errorf("transcription API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp, nil
}

func parseOpenAITranscription(raw []byte) (TranscribeResult, error) {
	var parsed struct {
		Text     string          `json:"text"`
		Language string          `json:"language"`
		Duration float64         `json:"duration"`
		Usage    json.RawMessage `json:"usage"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		text := strings.TrimSpace(string(raw))
		if text == "" {
			return TranscribeResult{}, fmt.Errorf("decode transcription response: %w", err)
		}
		return TranscribeResult{Text: text}, nil
	}
	result := TranscribeResult{
		Text:     parsed.Text,
		Language: parsed.Language,
		Duration: parsed.Duration,
		Usage:    parseTranscribeUsage(parsed.Usage),
	}
	for _, seg := range parsed.Segments {
		result.Segments = append(result.Segments, TranscribeSegment{Start: seg.Start, End: seg.End, Text: seg.Text})
	}
	return result, nil
}

func parseTranscribeUsage(raw json.RawMessage) *TranscribeUsage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var parsed struct {
		Seconds      float64  `json:"seconds"`
		InputTokens  int      `json:"input_tokens"`
		OutputTokens int      `json:"output_tokens"`
		TotalTokens  int      `json:"total_tokens"`
		Cost         *float64 `json:"cost"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	return &TranscribeUsage{
		Seconds:      parsed.Seconds,
		InputTokens:  parsed.InputTokens,
		OutputTokens: parsed.OutputTokens,
		TotalTokens:  parsed.TotalTokens,
		Cost:         parsed.Cost,
	}
}
