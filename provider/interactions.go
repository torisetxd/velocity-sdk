package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"velocitylabs.cc/sdk/internal/debugtrace"
)

const (
	defaultGeminiInteractionsURL = "https://generativelanguage.googleapis.com/v1beta"
	defaultVertexLocation        = "global"

	// InteractionsThoughtsMetaKey holds the signed thought steps of an assistant
	// turn. Gemini 3+ validates them when the turn is replayed with tool calls.
	InteractionsThoughtsMetaKey = "interactions_thoughts"
	// InteractionsIDMetaKey holds the id of the stored interaction that produced
	// an assistant turn.
	InteractionsIDMetaKey = "interactions_id"

	// longDecodeThreshold is the largest output a single synchronous interaction
	// may produce; anything above it needs background execution.
	longDecodeThreshold = 65536
)

var interactionsFallbackModels = []ModelInfo{
	{ID: "gemini-4-argon", Name: "Gemini 4 Argon", ContextLength: 1048576},
	{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", ContextLength: 1048576},
	{ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash", ContextLength: 1048576},
	{ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro Preview", ContextLength: 1048576},
}

// InteractionsConfig configures a provider for Google's Interactions API.
//
// With Project set it targets Vertex AI, authenticating with an OAuth bearer
// token (APIKey or TokenSource). Otherwise it targets the Gemini Developer API
// and APIKey is sent as x-goog-api-key.
type InteractionsConfig struct {
	APIURL      string
	APIKey      string
	Project     string
	Location    string
	ExtraBody   map[string]any
	TokenSource TokenSource
}

type interactionsProvider struct {
	httpClient  *http.Client
	baseURL     string
	createURL   string
	vertex      bool
	apiKey      string
	tokenSource TokenSource
	extraBody   map[string]any
}

func NewInteractionsProvider(cfg InteractionsConfig) *interactionsProvider {
	p := &interactionsProvider{
		httpClient:  defaultHTTPClient(),
		apiKey:      strings.TrimSpace(cfg.APIKey),
		tokenSource: cfg.TokenSource,
		extraBody:   CopyMapAny(cfg.ExtraBody),
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	base = strings.TrimSuffix(base, "/interactions")
	project := strings.TrimSpace(cfg.Project)

	if project == "" {
		if base == "" {
			base = defaultGeminiInteractionsURL
		}
		p.baseURL = base
		p.createURL = base + "/interactions"
		return p
	}

	location := strings.TrimSpace(cfg.Location)
	if location == "" {
		location = defaultVertexLocation
	}
	if base == "" {
		host := location + "-aiplatform.googleapis.com"
		if location == defaultVertexLocation {
			host = "aiplatform.googleapis.com"
		}
		base = "https://" + host + "/v1beta1"
	}
	p.vertex = true
	p.baseURL = base
	p.createURL = fmt.Sprintf("%s/projects/%s/locations/%s/interactions", base, url.PathEscape(project), url.PathEscape(location))
	return p
}

func (p *interactionsProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func (p *interactionsProvider) interactionURL(id string) string {
	return p.baseURL + "/interactions/" + url.PathEscape(id)
}

func (p *interactionsProvider) setAuth(ctx context.Context, req *http.Request) error {
	token := p.apiKey
	if p.tokenSource != nil {
		fresh, err := p.tokenSource(ctx)
		if err != nil {
			return err
		}
		token = fresh
	}
	if token == "" {
		return nil
	}
	if p.vertex {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("x-goog-api-key", token)
	}
	return nil
}

func (p *interactionsProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	payload, err := buildInteractionsPayload(req, p.extraBody)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	background, _ := payload["background"].(bool)
	s := newInteractionsStream(ctx, p, background)
	if err := s.open(http.MethodPost, p.createURL, body); err != nil {
		return nil, err
	}
	return s, nil
}

// BuildInteractionsBody builds the create-interaction payload. Provider and
// request extra bodies are deep-merged over it, so generation_config,
// background, store and the like can be set from configuration.
//
// An output budget above 64K tokens ("long decode") only works through
// background execution, which in turn requires a stored interaction, so both
// are enabled automatically unless explicitly set.
func BuildInteractionsBody(req ChatRequest, providerExtra map[string]any) ([]byte, error) {
	payload, err := buildInteractionsPayload(req, providerExtra)
	if err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

func buildInteractionsPayload(req ChatRequest, providerExtra map[string]any) (map[string]any, error) {
	system, input := convertMessagesToInteractionsInput(req.Messages)
	if len(input) == 0 {
		return nil, fmt.Errorf("interactions request has no input")
	}

	body := map[string]any{
		"model":  req.Model,
		"input":  input,
		"stream": true,
		"generation_config": map[string]any{
			"thinking_summaries": "auto",
		},
	}
	if system != "" {
		body["system_instruction"] = system
	}
	if tools := convertToolsToInteractions(req.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := interactionsToolChoice(req.ToolChoice); choice != "" {
		body["generation_config"].(map[string]any)["tool_choice"] = choice
	}

	body = MergeMaps(body, MergeMaps(providerExtra, req.ExtraBody))
	body["model"] = req.Model
	body["input"] = input
	body["stream"] = true

	if interactionsMaxOutput(body) > longDecodeThreshold {
		if _, set := body["background"]; !set {
			body["background"] = true
		}
	}
	if background, _ := body["background"].(bool); background {
		if _, set := body["store"]; !set {
			body["store"] = true
		}
	}
	return body, nil
}

func interactionsMaxOutput(body map[string]any) int {
	config, _ := body["generation_config"].(map[string]any)
	switch n := config["max_output_tokens"].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		v, _ := n.Int64()
		return int(v)
	}
	return 0
}

func interactionsToolChoice(choice string) string {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "required", "any":
		return "any"
	}
	return ""
}

func convertToolsToInteractions(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Function.Name == "" {
			continue
		}
		decl := map[string]any{
			"type": "function",
			"name": tool.Function.Name,
		}
		if tool.Function.Description != "" {
			decl["description"] = tool.Function.Description
		}
		if tool.Function.Parameters != nil {
			decl["parameters"] = tool.Function.Parameters
		}
		out = append(out, decl)
	}
	return out
}

// open issues a create (POST) or resume (GET) request and attaches the
// response to the stream. Failures are classified so the agent loop can retry.
func (s *interactionsStream) open(method, target string, body []byte) error {
	connCtx, cancel := context.WithCancel(s.ctx)

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(connCtx, method, target, reader)
	if err != nil {
		cancel()
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if err := s.p.setAuth(s.ctx, httpReq); err != nil {
		cancel()
		return err
	}

	debugtrace.Log("provider.request", map[string]any{
		"protocol":    "interactions",
		"stream_id":   s.streamID,
		"method":      method,
		"body_bytes":  len(body),
		"body_sha256": debugtrace.Digest(string(body)),
	})

	resp, err := s.p.httpClient.Do(httpReq)
	if err != nil {
		cancel()
		return err
	}
	debugtrace.Log("provider.response", map[string]any{
		"protocol":  "interactions",
		"stream_id": s.streamID,
		"status":    resp.StatusCode,
	})

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer cancel()
		defer resp.Body.Close()
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return classifyInteractionsHTTPError(resp, body, rawBody)
	}

	idle := interactionsIdleTimeout
	if s.background {
		idle = interactionsBackgroundIdleTimeout
	}
	s.attach(resp.Body, cancel, idle)
	return nil
}

func classifyInteractionsHTTPError(resp *http.Response, requestBody, rawBody []byte) error {
	message := strings.TrimSpace(string(rawBody))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := 0
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			fmt.Sscanf(ra, "%d", &retryAfter)
		}
		return &RateLimitError{
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfter,
			Message:    fmt.Sprintf("rate limit exceeded (429): %s", message),
		}
	case resp.StatusCode >= 500:
		return &ServerError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("server error (%d): %s", resp.StatusCode, message),
		}
	}
	return formatStreamError(resp.Status, requestBody, rawBody)
}

func (p *interactionsProvider) getJSON(ctx context.Context, target string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := p.setAuth(ctx, req); err != nil {
		return err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return classifyInteractionsHTTPError(resp, nil, raw)
	}
	return json.Unmarshal(raw, out)
}

func (p *interactionsProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	var err error
	if p.vertex {
		models, err = p.fetchVertexModels(ctx)
	} else {
		models, err = p.fetchGeminiModels(ctx)
	}
	if err != nil || len(models) == 0 {
		return copyModelInfos(interactionsFallbackModels), nil
	}
	return models, nil
}

func (p *interactionsProvider) fetchGeminiModels(ctx context.Context) ([]ModelInfo, error) {
	var listing struct {
		Models []struct {
			Name             string   `json:"name"`
			DisplayName      string   `json:"displayName"`
			Description      string   `json:"description"`
			InputTokenLimit  int      `json:"inputTokenLimit"`
			SupportedMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := p.getJSON(ctx, p.baseURL+"/models?pageSize=1000", &listing); err != nil {
		return nil, err
	}
	models := make([]ModelInfo, 0, len(listing.Models))
	for _, m := range listing.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if !strings.HasPrefix(id, "gemini") {
			continue
		}
		models = append(models, ModelInfo{ID: id, Name: m.DisplayName, Description: m.Description, ContextLength: m.InputTokenLimit})
	}
	return models, nil
}

func (p *interactionsProvider) fetchVertexModels(ctx context.Context) ([]ModelInfo, error) {
	var listing struct {
		PublisherModels []struct {
			Name string `json:"name"`
		} `json:"publisherModels"`
	}
	if err := p.getJSON(ctx, p.baseURL+"/publishers/google/models?pageSize=1000", &listing); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	models := make([]ModelInfo, 0, len(listing.PublisherModels))
	for _, m := range listing.PublisherModels {
		id := m.Name[strings.LastIndex(m.Name, "/")+1:]
		if !strings.HasPrefix(id, "gemini") || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, ModelInfo{ID: id})
	}
	return models, nil
}
