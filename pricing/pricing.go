// Package pricing prices token usage and looks up context windows from the
// models.dev catalog (https://models.dev/api.json). It is meant for providers
// that do not report a per-request cost; OpenRouter already does (see
// provider.TokenUsage.Cost).
package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"velocitylabs.cc/sdk/provider"
)

// DefaultURL is the models.dev catalog endpoint.
const DefaultURL = "https://models.dev/api.json"

// Tier overrides the base rates once a request's input exceeds Size tokens.
type Tier struct {
	Size       int
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Model holds a catalog entry. Prices are USD per million tokens.
type Model struct {
	ID            string
	Provider      string
	ContextWindow int
	MaxOutput     int
	Input         float64
	Output        float64
	CacheRead     float64
	CacheWrite    float64
	Tiers         []Tier
}

// Priced reports whether the entry carries any price.
func (m Model) Priced() bool { return m.Input > 0 || m.Output > 0 }

// Cost prices one request's usage in USD. Cached and cache-write tokens are
// charged at their own rates, falling back to the input rate when the catalog
// lists none.
func (m Model) Cost(u provider.TokenUsage) float64 {
	in, out, cacheRead, cacheWrite := m.Input, m.Output, m.CacheRead, m.CacheWrite
	for _, t := range m.Tiers {
		if u.InputTokens > t.Size {
			in, out, cacheRead, cacheWrite = t.Input, t.Output, t.CacheRead, t.CacheWrite
		}
	}
	if cacheRead == 0 {
		cacheRead = in
	}
	if cacheWrite == 0 {
		cacheWrite = in
	}
	uncached := max(u.InputTokens-u.CachedTokens-u.CacheWriteTokens, 0)
	total := float64(uncached)*in +
		float64(u.CachedTokens)*cacheRead +
		float64(u.CacheWriteTokens)*cacheWrite +
		float64(u.OutputTokens)*out
	return total / 1e6
}

type rawCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Tiers      []struct {
		rawCost
		Tier struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
}

type rawModel struct {
	ID    string   `json:"id"`
	Cost  *rawCost `json:"cost"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

type rawProvider struct {
	Models map[string]rawModel `json:"models"`
}

// Catalog is an in-memory snapshot of the models.dev catalog.
type Catalog struct {
	byProvider map[string]map[string]Model
	// byModel indexes every provider's entry by lower-cased model ID; the
	// priced, largest-context entry wins.
	byModel map[string]Model
}

// Parse builds a Catalog from api.json bytes.
func Parse(data []byte) (*Catalog, error) {
	var raw map[string]rawProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse catalog: %w", err)
	}
	c := &Catalog{byProvider: map[string]map[string]Model{}, byModel: map[string]Model{}}
	for pid, p := range raw {
		models := make(map[string]Model, len(p.Models))
		for key, rm := range p.Models {
			id := rm.ID
			if id == "" {
				id = key
			}
			m := Model{ID: id, Provider: pid, ContextWindow: rm.Limit.Context, MaxOutput: rm.Limit.Output}
			if rm.Cost != nil {
				m.Input, m.Output, m.CacheRead, m.CacheWrite = rm.Cost.Input, rm.Cost.Output, rm.Cost.CacheRead, rm.Cost.CacheWrite
				for _, t := range rm.Cost.Tiers {
					if t.Tier.Type != "" && t.Tier.Type != "context" {
						continue
					}
					m.Tiers = append(m.Tiers, Tier{Size: t.Tier.Size, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite})
				}
			}
			models[strings.ToLower(id)] = m
			lower := strings.ToLower(id)
			if cur, ok := c.byModel[lower]; !ok || better(m, cur) {
				c.byModel[lower] = m
			}
		}
		c.byProvider[strings.ToLower(pid)] = models
	}
	return c, nil
}

func better(a, b Model) bool {
	if a.Priced() != b.Priced() {
		return a.Priced()
	}
	return a.ContextWindow > b.ContextWindow
}

// Lookup finds a model. providerID is a models.dev provider ID ("openai",
// "anthropic", "openrouter", ...) and may be empty or unknown, in which case
// the model ID is matched across all providers. Vendor-prefixed IDs
// ("openai/gpt-5") also match their bare form.
func (c *Catalog) Lookup(providerID, modelID string) (Model, bool) {
	if c == nil {
		return Model{}, false
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return Model{}, false
	}
	candidates := []string{id}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		candidates = append(candidates, id[i+1:])
	}
	if models, ok := c.byProvider[strings.ToLower(strings.TrimSpace(providerID))]; ok {
		for _, cand := range candidates {
			if m, ok := models[cand]; ok {
				return m, true
			}
		}
	}
	for _, cand := range candidates {
		if m, ok := c.byModel[cand]; ok {
			return m, true
		}
	}
	return Model{}, false
}

// Source fetches and caches the catalog. The zero value works.
type Source struct {
	URL    string        // defaults to DefaultURL
	Client *http.Client  // defaults to a 30s-timeout client
	TTL    time.Duration // defaults to 24h
	// CachePath, if set, persists the downloaded JSON so a restart or an
	// outage does not lose the catalog.
	CachePath string

	mu      sync.Mutex
	catalog *Catalog
	fetched time.Time
}

// Catalog returns the cached catalog, refreshing it when older than TTL. If a
// refresh fails and an older copy (memory or disk) exists, that copy is
// returned with a nil error.
func (s *Source) Catalog(ctx context.Context) (*Catalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if s.catalog != nil && time.Since(s.fetched) < ttl {
		return s.catalog, nil
	}
	data, err := s.download(ctx)
	if err == nil {
		var c *Catalog
		if c, err = Parse(data); err == nil {
			s.catalog, s.fetched = c, time.Now()
			if s.CachePath != "" {
				_ = os.WriteFile(s.CachePath, data, 0o600)
			}
			return c, nil
		}
	}
	if s.catalog != nil {
		return s.catalog, nil
	}
	if s.CachePath != "" {
		if data, rerr := os.ReadFile(s.CachePath); rerr == nil {
			if c, perr := Parse(data); perr == nil {
				s.catalog = c
				// Retry the network soon instead of waiting a full TTL.
				s.fetched = time.Now().Add(-ttl + time.Minute)
				return c, nil
			}
		}
	}
	return nil, err
}

func (s *Source) download(ctx context.Context) ([]byte, error) {
	url := s.URL
	if url == "" {
		url = DefaultURL
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: catalog returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// Lookup is Catalog followed by Catalog.Lookup.
func (s *Source) Lookup(ctx context.Context, providerID, modelID string) (Model, bool) {
	c, err := s.Catalog(ctx)
	if err != nil {
		return Model{}, false
	}
	return c.Lookup(providerID, modelID)
}
