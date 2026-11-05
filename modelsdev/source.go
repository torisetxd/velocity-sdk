package modelsdev

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

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

// Lookup is Catalog followed by Catalog.Lookup.
func (s *Source) Lookup(ctx context.Context, providerID, modelID string) (Model, bool) {
	c, err := s.Catalog(ctx)
	if err != nil {
		return Model{}, false
	}
	return c.Lookup(providerID, modelID)
}

// DisplayName returns a model's catalog label, or its ID when the catalog is
// unavailable or does not know the model.
func (s *Source) DisplayName(ctx context.Context, providerID, modelID string) string {
	if m, ok := s.Lookup(ctx, providerID, modelID); ok {
		return m.DisplayName()
	}
	return modelID
}

// Names is Catalog followed by Catalog.Names: model ID -> catalog label.
func (s *Source) Names(ctx context.Context, providerID string, modelIDs []string) map[string]string {
	c, err := s.Catalog(ctx)
	if err != nil {
		return nil
	}
	return c.Names(providerID, modelIDs)
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
		return nil, fmt.Errorf("modelsdev: catalog returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
