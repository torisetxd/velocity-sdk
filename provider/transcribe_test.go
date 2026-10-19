package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAITranscriberMultipart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("model") != "gpt-4o-transcribe" || r.FormValue("language") != "en" {
			t.Errorf("form %v", r.MultipartForm.Value)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(f)
		if string(data) != "abc" {
			t.Errorf("audio %q", data)
		}
		_, _ = w.Write([]byte(`{"text":"hello","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`))
	}))
	defer srv.Close()

	res, err := NewOpenAITranscriber(srv.URL, "k", nil).Transcribe(context.Background(), TranscribeRequest{
		Model: "gpt-4o-transcribe", Audio: []byte("abc"), Format: "mp3", Language: "en",
	})
	if err != nil || res.Text != "hello" || res.Usage == nil || res.Usage.TotalTokens != 4 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestOpenAITranscriberStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"transcript.text.delta\",\"delta\":\"he\"}\n\ndata: {\"type\":\"transcript.text.delta\",\"delta\":\"llo\"}\n\ndata: {\"type\":\"transcript.text.done\",\"text\":\"hello\"}\n\n")
	}))
	defer srv.Close()

	var deltas []string
	res, err := NewOpenAITranscriber(srv.URL, "k", nil).StreamTranscribe(context.Background(),
		TranscribeRequest{Model: "m", Audio: []byte("a")},
		func(ev TranscribeEvent) error {
			if ev.Delta != "" {
				deltas = append(deltas, ev.Delta)
			}
			return nil
		})
	if err != nil || res.Text != "hello" || strings.Join(deltas, "|") != "he|llo" {
		t.Fatalf("res %+v deltas %v err %v", res, deltas, err)
	}
}

func TestOpenRouterTranscriberJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audio/transcriptions" {
			t.Errorf("path %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ia := body["input_audio"].(map[string]any)
		if ia["data"] != "YWJj" || ia["format"] != "wav" || body["model"] != "openai/whisper-large-v3" {
			t.Errorf("body %v", body)
		}
		_, _ = w.Write([]byte(`{"text":"hi","usage":{"seconds":1.5,"cost":0.001}}`))
	}))
	defer srv.Close()

	res, err := NewOpenRouterTranscriber(srv.URL+"/api/v1/chat/completions", "k").Transcribe(context.Background(),
		TranscribeRequest{Model: "openai/whisper-large-v3", Audio: []byte("abc")})
	if err != nil || res.Text != "hi" || res.Usage.Seconds != 1.5 || res.Usage.Cost == nil {
		t.Fatalf("res %+v err %v", res, err)
	}
}

type fakeChat struct{ got ChatRequest }

type fakeStream struct{ events []StreamEvent }

func (s *fakeStream) Recv() (StreamEvent, error) {
	if len(s.events) == 0 {
		return StreamEvent{}, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}
func (s *fakeStream) Close() error { return nil }

func (f *fakeChat) StreamChat(_ context.Context, req ChatRequest) (ChatStream, error) {
	f.got = req
	return &fakeStream{events: []StreamEvent{{Content: "hel"}, {Content: "lo "}, {Usage: &TokenUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}}}}, nil
}

func TestChatTranscriber(t *testing.T) {
	chat := &fakeChat{}
	res, err := NewChatTranscriber(chat, "").Transcribe(context.Background(),
		TranscribeRequest{Model: "google/gemini", Audio: []byte("abc"), Format: "wav", Prompt: "names: Toriset"})
	if err != nil || res.Text != "hello" || res.Usage.TotalTokens != 12 {
		t.Fatalf("res %+v err %v", res, err)
	}
	parts := chat.got.Messages[0].Content.([]map[string]any)
	if parts[1]["type"] != "input_audio" || !strings.Contains(parts[0]["text"].(string), "Toriset") {
		t.Fatalf("parts %v", parts)
	}
}
