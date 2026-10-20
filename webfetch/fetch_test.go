package webfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchHTMLToMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") || strings.HasSuffix(r.URL.Path, ".mdx") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><nav>menu</nav><h1>Title</h1><p>Hello world</p></body></html>`))
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/page", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Title") || !strings.Contains(out, "Hello world") || strings.Contains(out, "menu") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestFetchPrefersMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/doc.md") {
			_, _ = w.Write([]byte("# From markdown\n"))
			return
		}
		_, _ = w.Write([]byte("<html><body>html</body></html>"))
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/doc", Options{})
	if err != nil || !strings.Contains(out, "From markdown") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestFetchSpoofsCrawlerWhenBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") || strings.HasSuffix(r.URL.Path, ".mdx") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.UserAgent(), "Googlebot") {
			_, _ = w.Write([]byte("<html><body><p>real content</p></body></html>"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><title>Just a moment...</title></html>"))
	}))
	defer srv.Close()

	if _, err := Fetch(context.Background(), srv.URL+"/x", Options{}); err == nil {
		t.Fatal("expected a failure without spoofing")
	}
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{Spoof: true})
	if err != nil || !strings.Contains(out, "real content") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestFetchSpoofsOnChallengeStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") || strings.HasSuffix(r.URL.Path, ".mdx") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.UserAgent(), "bingbot") {
			_, _ = w.Write([]byte("<html><body>bing sees this</body></html>"))
			return
		}
		_, _ = w.Write([]byte("<html><body>Please enable JavaScript to continue</body></html>"))
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL, Options{Spoof: true, SpoofAgents: []Agent{AgentBingbot}})
	if err != nil || !strings.Contains(out, "bing sees this") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestFetchBinaryNotice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") || strings.HasSuffix(r.URL.Path, ".mdx") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL, Options{})
	if err != nil || !strings.Contains(out, "[binary content") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestFetchRejectsBadURL(t *testing.T) {
	if _, err := Fetch(context.Background(), "file:///etc/passwd", Options{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSafeClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, Options{Client: NewSafeClient(5 * time.Second)})
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err=%v", err)
	}
}

func TestCensorHook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") {
			_, _ = w.Write([]byte("key=SECRET"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/a", Options{Censor: func(s string) string { return strings.ReplaceAll(s, "SECRET", "[redacted]") }})
	if err != nil || strings.Contains(out, "SECRET") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
