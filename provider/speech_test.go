package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAISpeakerSpeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/audio/speech" || r.Header.Get("Authorization") != "Bearer k" ||
			!strings.Contains(string(body), `"input":"hello"`) || !strings.Contains(string(body), `"response_format":"wav"`) {
			http.Error(w, string(body), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFFfake"))
	}))
	defer srv.Close()

	res, err := NewOpenAISpeaker(srv.URL+"/v1", "k").Speak(context.Background(), SpeechRequest{Model: "tts", Input: "hello", Format: "wav"})
	if err != nil || string(res.Audio) != "RIFFfake" || res.MIMEType != "audio/wav" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestOpenAISpeakerStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abc"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	res, err := NewOpenAISpeaker(srv.URL, "").StreamSpeech(context.Background(), SpeechRequest{Model: "tts", Input: "x"}, &buf)
	if err != nil || buf.String() != "abc" || res.Format != "mp3" || res.MIMEType != "audio/mpeg" {
		t.Fatalf("buf=%q res=%+v err=%v", buf.String(), res, err)
	}
}

func TestOpenAISpeakerAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such voice", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := NewOpenAISpeaker(srv.URL, "").Speak(context.Background(), SpeechRequest{Model: "tts", Input: "x"})
	if err == nil || !strings.Contains(err.Error(), "no such voice") {
		t.Fatalf("err=%v", err)
	}
}
