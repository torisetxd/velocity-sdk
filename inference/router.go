package inference

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Router struct {
	mu       sync.RWMutex
	registry *Registry
	routes   map[string]Config
	current  string
	client   Client
}

func NewRouter(registry *Registry) *Router {
	if registry == nil {
		registry = NewDefaultRegistry()
	}
	return &Router{registry: registry, routes: make(map[string]Config)}
}

func (r *Router) Set(name string, config Config) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("set provider: empty name")
	}
	if r == nil {
		return fmt.Errorf("set provider %q: nil router", name)
	}
	config = config.Normalized()
	if r.registry.CanonicalType(config.Type) == "" {
		return fmt.Errorf("set provider %q: unsupported provider type %q", name, config.Type)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var activeClient Client
	if name == r.current {
		client, err := r.registry.New(config)
		if err != nil {
			return fmt.Errorf("rebuild provider %q: %w", name, err)
		}
		activeClient = client
	}
	r.routes[name] = config
	if activeClient != nil {
		r.client = activeClient
	}
	return nil
}

func (r *Router) Select(name string) (Client, error) {
	if r == nil {
		return nil, fmt.Errorf("select provider: nil router")
	}
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	config, ok := r.routes[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	client, err := r.registry.New(config)
	if err != nil {
		return nil, fmt.Errorf("build provider %q: %w", name, err)
	}
	r.current = name
	r.client = client
	return client, nil
}

func (r *Router) Current() (name string, client Client, ok bool) {
	if r == nil {
		return "", nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.current == "" || r.client == nil {
		return "", nil, false
	}
	return r.current, r.client, true
}

func (r *Router) Config(name string) (Config, bool) {
	if r == nil {
		return Config{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	config, ok := r.routes[strings.TrimSpace(name)]
	return config.Normalized(), ok
}

func (r *Router) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.routes))
	for name := range r.routes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
