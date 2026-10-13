package inference

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Factory func(Config) (Client, error)

type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	aliases   map[string]string
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory), aliases: make(map[string]string)}
}

func (r *Registry) Register(providerType string, factory Factory, aliases ...string) error {
	if r == nil {
		return fmt.Errorf("register provider: nil registry")
	}
	if factory == nil {
		return fmt.Errorf("register provider %q: nil factory", providerType)
	}
	canonical := normalizeType(providerType)
	if canonical == "" {
		return fmt.Errorf("register provider: empty type")
	}
	normalizedAliases := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = normalizeType(alias)
		if alias != "" && alias != canonical {
			normalizedAliases = append(normalizedAliases, alias)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[canonical]; exists {
		return fmt.Errorf("register provider %q: already registered", canonical)
	}
	if target, exists := r.aliases[canonical]; exists && target != canonical {
		return fmt.Errorf("register provider %q: conflicts with alias for %q", canonical, target)
	}
	for _, alias := range normalizedAliases {
		if _, exists := r.factories[alias]; exists {
			return fmt.Errorf("register provider %q: alias %q conflicts with a provider type", canonical, alias)
		}
		if target, exists := r.aliases[alias]; exists && target != canonical {
			return fmt.Errorf("register provider %q: alias %q already belongs to %q", canonical, alias, target)
		}
	}

	r.factories[canonical] = factory
	r.aliases[canonical] = canonical
	for _, alias := range normalizedAliases {
		r.aliases[alias] = canonical
	}
	return nil
}

func (r *Registry) CanonicalType(raw string) string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.aliases[normalizeType(raw)]
}

func (r *Registry) New(config Config) (Client, error) {
	if r == nil {
		return nil, fmt.Errorf("build provider: nil registry")
	}
	rawType := config.Type
	if strings.TrimSpace(rawType) == "" || strings.EqualFold(strings.TrimSpace(rawType), "auto") {
		rawType = DetectType(rawType, config.APIURL)
	}
	r.mu.RLock()
	canonical := r.aliases[normalizeType(rawType)]
	factory := r.factories[canonical]
	r.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("unsupported provider type %q", strings.TrimSpace(rawType))
	}
	config = config.Normalized()
	config.Type = canonical
	return factory(config)
}

func (r *Registry) Types() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.factories))
	for providerType := range r.factories {
		types = append(types, providerType)
	}
	sort.Strings(types)
	return types
}

func normalizeType(raw string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "_", "-")
}
