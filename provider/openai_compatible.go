package provider

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type openAICompatibleProvider struct {
	httpClient             *http.Client
	apiURL                 string
	apiKey                 string
	tokenSource            TokenSource
	extraBody              map[string]any
	reasoningField         string
	extraHeaders           map[string]string
	toolResultImagesAsUser bool
}

func NewOpenAICompatibleProvider(apiURL, apiKey string, extraBody map[string]any, reasoningField string) *openAICompatibleProvider {
	return NewOpenAICompatibleProviderWithHeaders(apiURL, apiKey, extraBody, reasoningField, nil)
}

func NewOpenAICompatibleProviderWithHeaders(apiURL, apiKey string, extraBody map[string]any, reasoningField string, extraHeaders map[string]string) *openAICompatibleProvider {
	httpClient := defaultHTTPClient()
	preconnectHTTPClient(httpClient, apiURL, apiKey)

	return &openAICompatibleProvider{
		httpClient:     httpClient,
		apiURL:         apiURL,
		apiKey:         apiKey,
		extraBody:      CopyMapAny(extraBody),
		reasoningField: reasoningField,
		extraHeaders:   copyHeaders(extraHeaders),
	}
}

func (p *openAICompatibleProvider) SetTokenSource(ts TokenSource) {
	p.tokenSource = ts
}

func (p *openAICompatibleProvider) resolveToken(ctx context.Context) (string, error) {
	if p.tokenSource != nil {
		return p.tokenSource(ctx)
	}
	return p.apiKey, nil
}

func (p *openAICompatibleProvider) SetToolResultImagesAsUser(enabled bool) {
	p.toolResultImagesAsUser = enabled
}

var _ ToolResultImagesConverter = (*openAICompatibleProvider)(nil)

func (p *openAICompatibleProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	token, err := p.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	field := strings.TrimSpace(p.reasoningField)
	if field == "" {
		field = "reasoning_content"
	}
	reasoning := OpenAICompatibleReasoningPersistence(field)

	body, err := BuildOpenAIChatBody(req, OpenAIRequestOptions{
		ForceStream:            true,
		IncludeUsage:           true,
		ProviderExtraBody:      p.extraBody,
		RequestExtraBody:       req.ExtraBody,
		ReasoningPersistence:   reasoning,
		ToolResultImagesAsUser: p.toolResultImagesAsUser,
	})
	if err != nil {
		return nil, err
	}

	return streamChatCompletions(
		ctx,
		p.httpClient,
		p.apiURL,
		token,
		body,
		p.extraHeaders,
		streamOpts{ReasoningFields: ReasoningReadFields(firstReasoningField(reasoning))},
	)
}

func (p *openAICompatibleProvider) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	token, err := p.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	return FetchModelsFromEndpoint(ctx, token, p.apiURL, p.extraHeaders)
}

func (p *openAICompatibleProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsReasoning:        true,
		SupportsReasoningDetails: false,
		SupportsImages:           true,
		SupportsToolCalls:        true,
	}
}

func defaultHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   0,
		Transport: transport,
	}
}

func copyHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = v
	}
	return out
}

func preconnectHTTPClient(httpClient *http.Client, apiURL, apiKey string) {
	if httpClient == nil {
		return
	}

	parsed, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodHead, parsed.String(), nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "*/*")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
}
