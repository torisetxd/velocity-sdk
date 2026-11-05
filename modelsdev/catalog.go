// Package modelsdev is the models.dev catalog (https://models.dev/api.json)
// for this SDK: every provider the site lists, each with its models' display
// names, prices, context limits, modalities and capability flags.
//
// It is the single source of model metadata here. Pricing, context windows and
// human-readable labels all come from the same parsed catalog:
//
//	src := &modelsdev.Source{CachePath: "models.dev.json"}
//	m, ok := src.Lookup(ctx, modelsdev.ProviderID("openrouter", ""), "x-ai/grok-4.3")
//	cost := m.Cost(usage)
//	label := m.DisplayName()
package modelsdev

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

// DefaultURL is the models.dev catalog endpoint.
const DefaultURL = "https://models.dev/api.json"

// Rate is a model's price in USD per million tokens.
type Rate struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	// Tiers override the base rates once a request's input exceeds their size.
	Tiers []Tier
}

// Priced reports whether the rate carries any price.
func (r Rate) Priced() bool { return r.Input > 0 || r.Output > 0 }

// Tier is a context-size price break.
type Tier struct {
	Size       int
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Limit is a model's token limits.
type Limit struct {
	Context int
	Output  int
}

// Modalities lists the input and output kinds a model accepts, e.g. "text",
// "image", "audio", "video" and "pdf".
type Modalities struct {
	Input  []string
	Output []string
}

// Accepts reports whether the model takes the given input modality.
func (m Modalities) Accepts(modality string) bool {
	for _, in := range m.Input {
		if strings.EqualFold(in, modality) {
			return true
		}
	}
	return false
}

// ReasoningOption is one way a model's reasoning effort can be set.
type ReasoningOption struct {
	Type   string
	Values []string
}

// Model is one catalog entry.
type Model struct {
	// ID is the model ID the provider uses in requests; OpenRouter and friends
	// list vendor-prefixed IDs such as "openai/gpt-5".
	ID string
	// Name is the human-readable label ("Claude Sonnet 4.5"); it is empty when
	// the catalog publishes none.
	Name string
	// Provider is the models.dev provider ID that lists this entry.
	Provider string
	// ProviderName is that provider's human-readable name.
	ProviderName string

	Description string
	Family      string
	Knowledge   string
	ReleaseDate string
	LastUpdated string
	// Status is the catalog's lifecycle marker ("stable", "deprecated", ...).
	Status string

	Attachment       bool
	Reasoning        bool
	ToolCall         bool
	StructuredOutput bool
	Temperature      bool
	OpenWeights      bool

	ReasoningOptions []ReasoningOption
	Modalities       Modalities
	Limit            Limit
	Pricing          Rate
}

// DisplayName returns the catalog label, falling back to the model ID.
func (m Model) DisplayName() string {
	if strings.TrimSpace(m.Name) != "" {
		return m.Name
	}
	return m.ID
}

// Priced reports whether the entry carries any price.
func (m Model) Priced() bool { return m.Pricing.Priced() }

// Cost prices one request's usage in USD. Cached and cache-write tokens are
// charged at their own rates, falling back to the input rate when the catalog
// lists none.
func (m Model) Cost(u provider.TokenUsage) float64 {
	in, out, cacheRead, cacheWrite := m.Pricing.Input, m.Pricing.Output, m.Pricing.CacheRead, m.Pricing.CacheWrite
	for _, t := range m.Pricing.Tiers {
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

// AcceptsImages reports whether the model takes image input.
func (m Model) AcceptsImages() bool { return m.Modalities.Accepts("image") }

// Provider is one catalog provider and the models it lists.
type Provider struct {
	ID     string
	Name   string
	Env    []string
	NPM    string
	Doc    string
	API    string
	Models []Model
}

// Model finds one of the provider's models by ID. Vendor-prefixed IDs also
// match their bare form.
func (p Provider) Model(modelID string) (Model, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return Model{}, false
	}
	candidates := []string{id}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		candidates = append(candidates, id[i+1:])
	}
	for _, m := range p.Models {
		modelLower := strings.ToLower(m.ID)
		for _, cand := range candidates {
			if modelLower == cand {
				return m, true
			}
		}
	}
	return Model{}, false
}

// Catalog is an in-memory snapshot of the models.dev catalog.
type Catalog struct {
	providers map[string]Provider
	order     []string
	// byModel indexes every provider's entry by lower-cased model ID; the
	// priced, largest-context entry wins.
	byModel map[string]Model
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
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Family           string `json:"family"`
	Attachment       bool   `json:"attachment"`
	Reasoning        bool   `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	ToolCall         bool   `json:"tool_call"`
	StructuredOutput bool   `json:"structured_output"`
	Temperature      bool   `json:"temperature"`
	Knowledge        string `json:"knowledge"`
	ReleaseDate      string `json:"release_date"`
	LastUpdated      string `json:"last_updated"`
	OpenWeights      bool   `json:"open_weights"`
	Status           string `json:"status"`
	Modalities       struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost *rawCost `json:"cost"`
}

type rawProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Env    []string            `json:"env"`
	NPM    string              `json:"npm"`
	Doc    string              `json:"doc"`
	API    string              `json:"api"`
	Models map[string]rawModel `json:"models"`
}

// Parse builds a Catalog from api.json bytes.
func Parse(data []byte) (*Catalog, error) {
	var raw map[string]rawProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("modelsdev: parse catalog: %w", err)
	}
	c := &Catalog{
		providers: make(map[string]Provider, len(raw)),
		order:     make([]string, 0, len(raw)),
		byModel:   map[string]Model{},
	}
	for key, rp := range raw {
		providerID := strings.ToLower(strings.TrimSpace(rp.ID))
		if providerID == "" {
			providerID = strings.ToLower(strings.TrimSpace(key))
		}
		p := Provider{
			ID:     providerID,
			Name:   strings.TrimSpace(rp.Name),
			Env:    rp.Env,
			NPM:    rp.NPM,
			Doc:    rp.Doc,
			API:    rp.API,
			Models: make([]Model, 0, len(rp.Models)),
		}
		if p.Name == "" {
			p.Name = providerID
		}
		for modelKey, rm := range rp.Models {
			m := rawModelToModel(providerID, p.Name, modelKey, rm)
			p.Models = append(p.Models, m)
			lower := strings.ToLower(m.ID)
			if cur, ok := c.byModel[lower]; !ok || better(m, cur) {
				c.byModel[lower] = m
			}
		}
		sort.Slice(p.Models, func(i, j int) bool { return p.Models[i].ID < p.Models[j].ID })
		c.providers[providerID] = p
		c.order = append(c.order, providerID)
	}
	sort.Strings(c.order)
	return c, nil
}

func rawModelToModel(providerID, providerName, key string, rm rawModel) Model {
	id := strings.TrimSpace(rm.ID)
	if id == "" {
		id = key
	}
	m := Model{
		ID:               id,
		Name:             strings.TrimSpace(rm.Name),
		Provider:         providerID,
		ProviderName:     providerName,
		Description:      rm.Description,
		Family:           rm.Family,
		Knowledge:        rm.Knowledge,
		ReleaseDate:      rm.ReleaseDate,
		LastUpdated:      rm.LastUpdated,
		Status:           rm.Status,
		Attachment:       rm.Attachment,
		Reasoning:        rm.Reasoning,
		ToolCall:         rm.ToolCall,
		StructuredOutput: rm.StructuredOutput,
		Temperature:      rm.Temperature,
		OpenWeights:      rm.OpenWeights,
		Modalities:       Modalities{Input: rm.Modalities.Input, Output: rm.Modalities.Output},
		Limit:            Limit{Context: rm.Limit.Context, Output: rm.Limit.Output},
	}
	for _, opt := range rm.ReasoningOptions {
		m.ReasoningOptions = append(m.ReasoningOptions, ReasoningOption{Type: opt.Type, Values: opt.Values})
	}
	if rm.Cost != nil {
		m.Pricing = Rate{
			Input:      rm.Cost.Input,
			Output:     rm.Cost.Output,
			CacheRead:  rm.Cost.CacheRead,
			CacheWrite: rm.Cost.CacheWrite,
		}
		for _, t := range rm.Cost.Tiers {
			if t.Tier.Type != "" && t.Tier.Type != "context" {
				continue
			}
			m.Pricing.Tiers = append(m.Pricing.Tiers, Tier{
				Size:       t.Tier.Size,
				Input:      t.Input,
				Output:     t.Output,
				CacheRead:  t.CacheRead,
				CacheWrite: t.CacheWrite,
			})
		}
	}
	return m
}

func better(a, b Model) bool {
	if a.Priced() != b.Priced() {
		return a.Priced()
	}
	return a.Limit.Context > b.Limit.Context
}

// Providers returns every provider in the catalog, sorted by ID.
func (c *Catalog) Providers() []Provider {
	if c == nil {
		return nil
	}
	out := make([]Provider, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.providers[id])
	}
	return out
}

// Provider finds a provider by its models.dev ID.
func (c *Catalog) Provider(providerID string) (Provider, bool) {
	if c == nil {
		return Provider{}, false
	}
	p, ok := c.providers[strings.ToLower(strings.TrimSpace(providerID))]
	return p, ok
}

// Models returns a provider's models sorted by ID, or every known model when
// providerID is empty.
func (c *Catalog) Models(providerID string) []Model {
	if c == nil {
		return nil
	}
	if p, ok := c.Provider(providerID); ok {
		return p.Models
	}
	if strings.TrimSpace(providerID) != "" {
		return nil
	}
	out := make([]Model, 0, len(c.byModel))
	for _, m := range c.byModel {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Lookup finds a model. providerID is a models.dev provider ID ("openai",
// "anthropic", "openrouter", ...) and may be empty or unknown, in which case
// the model ID is matched across all providers. Vendor-prefixed IDs
// ("openai/gpt-5") also match their bare form.
func (c *Catalog) Lookup(providerID, modelID string) (Model, bool) {
	if c == nil {
		return Model{}, false
	}
	if p, ok := c.Provider(providerID); ok {
		if m, found := p.Model(modelID); found {
			return m, true
		}
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return Model{}, false
	}
	candidates := []string{id}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		candidates = append(candidates, id[i+1:])
	}
	for _, cand := range candidates {
		if m, ok := c.byModel[cand]; ok {
			return m, true
		}
	}
	return Model{}, false
}

// ProvidersFor lists every provider entry that offers the given model ID,
// sorted by provider ID.
func (c *Catalog) ProvidersFor(modelID string) []Model {
	if c == nil {
		return nil
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return nil
	}
	bare := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		bare = id[i+1:]
	}
	out := make([]Model, 0, 4)
	for _, pid := range c.order {
		p := c.providers[pid]
		for _, m := range p.Models {
			mid := strings.ToLower(m.ID)
			if mid == id || mid == bare || strings.HasSuffix(mid, "/"+bare) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// DisplayName returns a model's catalog label, or its ID when the catalog has
// none or does not know the model.
func (c *Catalog) DisplayName(providerID, modelID string) string {
	if m, ok := c.Lookup(providerID, modelID); ok {
		return m.DisplayName()
	}
	return modelID
}

// Names maps model IDs to their catalog labels, skipping models the catalog
// does not label. It is the batch form of DisplayName, for filling a picker.
func (c *Catalog) Names(providerID string, modelIDs []string) map[string]string {
	out := make(map[string]string, len(modelIDs))
	for _, id := range modelIDs {
		m, ok := c.Lookup(providerID, id)
		if !ok {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" || name == m.ID {
			continue
		}
		out[id] = name
	}
	return out
}
