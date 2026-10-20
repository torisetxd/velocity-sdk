package pricing

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"velocitylabs.cc/sdk/provider"
)

const sample = `{
 "openai":{"models":{"gpt-x":{"id":"gpt-x","cost":{"input":2,"output":8,"cache_read":0.5},"limit":{"context":200000,"output":1000}}}},
 "openrouter":{"models":{"openai/gpt-x":{"id":"openai/gpt-x","cost":{"input":2,"output":8},"limit":{"context":100000}},
  "big/model":{"id":"big/model","cost":{"input":1,"output":2,"tiers":[{"input":4,"output":6,"tier":{"type":"context","size":200000}}]},"limit":{"context":1000000}}}}
}`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLookupAndCost(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := c.Lookup("openai", "gpt-x")
	if !ok || m.ContextWindow != 200000 {
		t.Fatalf("%+v %v", m, ok)
	}
	// 1000 uncached @2 + 500 cached @0.5 + 100 out @8 = 2000+250+800 per M
	got := m.Cost(provider.TokenUsage{InputTokens: 1500, CachedTokens: 500, OutputTokens: 100})
	if !near(got, 0.00305) {
		t.Fatalf("cost %v", got)
	}
	if v, ok := c.Lookup("", "openai/gpt-x"); !ok || v.Provider == "" {
		t.Fatalf("vendor-prefixed lookup: %+v %v", v, ok)
	}
	if _, ok := c.Lookup("openai", "missing"); ok {
		t.Fatal("unexpected hit")
	}
}

func TestTierCost(t *testing.T) {
	c, _ := Parse([]byte(sample))
	m, _ := c.Lookup("openrouter", "big/model")
	if got := m.Cost(provider.TokenUsage{InputTokens: 300000, OutputTokens: 1000}); !near(got, 1.2+0.006) {
		t.Fatalf("tier cost %v", got)
	}
}

func TestSourceFallsBackToDiskCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	ok := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	if _, found := (&Source{URL: srv.URL, CachePath: path}).Lookup(context.Background(), "openai", "gpt-x"); !found {
		t.Fatal("first fetch failed")
	}
	ok = false
	if _, found := (&Source{URL: srv.URL, CachePath: path}).Lookup(context.Background(), "openai", "gpt-x"); !found {
		t.Fatal("disk fallback failed")
	}
}
