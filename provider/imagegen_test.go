package provider

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

func TestOpenAIImageGenerateB64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request "+r.URL.Path, http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"prompt":"a cat"`) {
			http.Error(w, string(body), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(tinyPNG) + `","revised_prompt":"a fluffy cat"}]}`))
	}))
	defer srv.Close()

	res, err := NewOpenAIImageGenerator(srv.URL+"/v1", "k").GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "a cat"})
	if err != nil || len(res.Images) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Images[0].MIMEType != "image/png" || res.Images[0].RevisedPrompt != "a fluffy cat" {
		t.Fatalf("%+v", res.Images[0])
	}
}

func TestOpenAIImageDownloadsURL(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img.png" {
			_, _ = w.Write(tinyPNG)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"url":"` + srv.URL + `/img.png"}]}`))
	}))
	defer srv.Close()

	res, err := NewOpenAIImageGenerator(srv.URL, "").GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "p"})
	if err != nil || len(res.Images) != 1 || len(res.Images[0].Data) != len(tinyPNG) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestOpenAIImageEditUsesMultipart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil || len(r.MultipartForm.File["image[]"]) != 1 {
			http.Error(w, "no image", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(tinyPNG) + `"}]}`))
	}))
	defer srv.Close()

	_, err := NewOpenAIImageGenerator(srv.URL, "").GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "p", Images: []ImageInput{{Data: tinyPNG}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenRouterImageGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/chat/completions" || !strings.Contains(string(body), `"modalities":["image","text"]`) {
			http.Error(w, string(body), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"here","images":[{"image_url":{"url":"data:image/png;base64,` +
			base64.StdEncoding.EncodeToString(tinyPNG) + `"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"cost":0.04}}`))
	}))
	defer srv.Close()

	res, err := NewOpenRouterImageGenerator(srv.URL, "k").GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "p", N: 2})
	if err != nil || len(res.Images) != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Usage == nil || res.Usage.Cost < 0.0799 || res.Usage.Cost > 0.0801 {
		t.Fatalf("usage %+v", res.Usage)
	}
}

func TestOpenRouterImageNoImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sorry"}}]}`))
	}))
	defer srv.Close()
	if _, err := NewOpenRouterImageGenerator(srv.URL, "").GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "p"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestAspectRatio(t *testing.T) {
	for in, want := range map[string]string{"1536x1024": "3:2", "1024x1024": "1:1", "16:9": "16:9", "large": "", "": ""} {
		if got := aspectRatio(in); got != want {
			t.Errorf("aspectRatio(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenRouterImagesEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/api/v1/images" || !strings.Contains(string(body), `"size":"1024x1024"`) || !strings.Contains(string(body), `"input_references"`) {
			http.Error(w, r.URL.Path+" "+string(body), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(tinyPNG) + `","media_type":"image/png"}],"usage":{"prompt_tokens":0,"completion_tokens":4175,"total_tokens":4175,"cost":0.04}}`))
	}))
	defer srv.Close()

	res, err := NewOpenRouterImageGenerator(srv.URL+"/api/v1", "k").GenerateImage(context.Background(),
		ImageRequest{Model: "openai/gpt-image-1", Prompt: "p", Size: "1024x1024", Images: []ImageInput{{Data: tinyPNG}}})
	if err != nil || len(res.Images) != 1 || res.Usage == nil || res.Usage.Cost != 0.04 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestOpenRouterImagesNoFallbackOnAuthError(t *testing.T) {
	chat := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			chat = true
		}
		http.Error(w, "no credits", http.StatusPaymentRequired)
	}))
	defer srv.Close()

	if _, err := NewOpenRouterImageGenerator(srv.URL, "k").GenerateImage(context.Background(), ImageRequest{Model: "m", Prompt: "p"}); err == nil || chat {
		t.Fatalf("err=%v chat=%v", err, chat)
	}
}
