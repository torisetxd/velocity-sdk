package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"velocitylabs.cc/sdk"
)

const (
	maxImageErrorBytes    = 4096
	maxGeneratedImageSize = 64 << 20
)

// ImageInput is a source image for editing or image-conditioned generation.
type ImageInput struct {
	Data     []byte
	MIMEType string
}

type ImageRequest struct {
	Model   string
	Prompt  string
	N       int
	Size    string // "1024x1024", ... (OpenAI-style endpoints only)
	Quality string
	// Images makes this an edit/variation request: OpenAI-style endpoints use
	// /images/edits, OpenRouter sends the images alongside the prompt.
	Images    []ImageInput
	ExtraBody map[string]any
}

type GeneratedImage struct {
	Data          []byte
	MIMEType      string
	RevisedPrompt string
}

type ImageUsage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	TotalTokens  int     `json:"total_tokens,omitempty"`
	Cost         float64 `json:"cost,omitempty"`
}

type ImageResult struct {
	Images []GeneratedImage
	Usage  *ImageUsage
}

type ImageGenerator interface {
	GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error)
}

func (r ImageRequest) validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("image: model is required")
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return fmt.Errorf("image: prompt is required")
	}
	return nil
}

func imageExt(mimeType string) string {
	switch strings.ToLower(mimeType) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "png"
	}
}

func sniffImageMIME(data []byte, fallback string) string {
	if t := http.DetectContentType(data); strings.HasPrefix(t, "image/") {
		return t
	}
	if fallback != "" {
		return fallback
	}
	return "image/png"
}

func setBearerAuth(ctx context.Context, req *http.Request, apiKey string, ts TokenSource) error {
	token := apiKey
	if ts != nil {
		var err error
		if token, err = ts(ctx); err != nil {
			return err
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

func doImageRequest(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxImageErrorBytes))
		return nil, fmt.Errorf("image API error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*maxGeneratedImageSize))
	if err != nil {
		return nil, fmt.Errorf("read image response: %w", err)
	}
	return raw, nil
}

// --- OpenAI-style /images/generations and /images/edits ---

type openAIImageGenerator struct {
	httpClient  *http.Client
	apiURL      string
	apiKey      string
	tokenSource TokenSource
}

func NewOpenAIImageGenerator(apiURL, apiKey string) *openAIImageGenerator {
	return &openAIImageGenerator{httpClient: defaultHTTPClient(), apiURL: apiURL, apiKey: apiKey}
}

func (g *openAIImageGenerator) SetTokenSource(ts TokenSource) { g.tokenSource = ts }

func (g *openAIImageGenerator) GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error) {
	if err := req.validate(); err != nil {
		return ImageResult{}, err
	}
	var (
		httpReq *http.Request
		err     error
	)
	if len(req.Images) > 0 {
		httpReq, err = g.editRequest(ctx, req)
	} else {
		httpReq, err = g.generateRequest(ctx, req)
	}
	if err != nil {
		return ImageResult{}, err
	}
	if err := setBearerAuth(ctx, httpReq, g.apiKey, g.tokenSource); err != nil {
		return ImageResult{}, err
	}
	raw, err := doImageRequest(g.httpClient, httpReq)
	if err != nil {
		return ImageResult{}, err
	}
	return g.parse(ctx, raw)
}

func (g *openAIImageGenerator) endpoint(path string) string {
	base := strings.TrimRight(strings.TrimSpace(g.apiURL), "/")
	for _, suffix := range []string{"/chat/completions", "/images/generations", "/images/edits"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base + path
}

func (g *openAIImageGenerator) generateRequest(ctx context.Context, req ImageRequest) (*http.Request, error) {
	payload := map[string]any{"model": req.Model, "prompt": req.Prompt}
	if req.N > 1 {
		payload["n"] = req.N
	}
	if v := strings.TrimSpace(req.Size); v != "" {
		payload["size"] = v
	}
	if v := strings.TrimSpace(req.Quality); v != "" {
		payload["quality"] = v
	}
	for k, v := range req.ExtraBody {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint("/images/generations"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return httpReq, nil
}

func (g *openAIImageGenerator) editRequest(ctx context.Context, req ImageRequest) (*http.Request, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := map[string]string{"model": req.Model, "prompt": req.Prompt}
	if req.N > 1 {
		fields["n"] = fmt.Sprint(req.N)
	}
	if v := strings.TrimSpace(req.Size); v != "" {
		fields["size"] = v
	}
	if v := strings.TrimSpace(req.Quality); v != "" {
		fields["quality"] = v
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	for k, v := range req.ExtraBody {
		if s, ok := v.(string); ok {
			if err := w.WriteField(k, s); err != nil {
				return nil, err
			}
		}
	}
	for i, img := range req.Images {
		mimeType := sniffImageMIME(img.Data, img.MIMEType)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="image%d.%s"`, i, imageExt(mimeType)))
		h.Set("Content-Type", mimeType)
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(img.Data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint("/images/edits"), &buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	return httpReq, nil
}

func (g *openAIImageGenerator) parse(ctx context.Context, raw []byte) (ImageResult, error) {
	var parsed struct {
		Data []struct {
			B64           string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage *ImageUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ImageResult{}, fmt.Errorf("decode image response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return ImageResult{}, fmt.Errorf("image API error: %s", parsed.Error.Message)
	}
	result := ImageResult{Usage: parsed.Usage}
	for _, item := range parsed.Data {
		var data []byte
		switch {
		case item.B64 != "":
			decoded, err := base64.StdEncoding.DecodeString(item.B64)
			if err != nil {
				return ImageResult{}, fmt.Errorf("decode image data: %w", err)
			}
			data = decoded
		case item.URL != "":
			downloaded, err := g.download(ctx, item.URL)
			if err != nil {
				return ImageResult{}, err
			}
			data = downloaded
		default:
			continue
		}
		result.Images = append(result.Images, GeneratedImage{Data: data, MIMEType: sniffImageMIME(data, ""), RevisedPrompt: item.RevisedPrompt})
	}
	if len(result.Images) == 0 {
		return ImageResult{}, fmt.Errorf("image API returned no images")
	}
	return result, nil
}

func (g *openAIImageGenerator) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download generated image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download generated image: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxGeneratedImageSize {
		return nil, fmt.Errorf("generated image exceeds %d bytes", maxGeneratedImageSize)
	}
	return data, nil
}

// --- OpenRouter: image output through /chat/completions ---

type openRouterImageGenerator struct {
	httpClient  *http.Client
	apiURL      string
	apiKey      string
	tokenSource TokenSource
}

func NewOpenRouterImageGenerator(apiURL, apiKey string) *openRouterImageGenerator {
	return &openRouterImageGenerator{httpClient: defaultHTTPClient(), apiURL: apiURL, apiKey: apiKey}
}

func (g *openRouterImageGenerator) SetTokenSource(ts TokenSource) { g.tokenSource = ts }

func (g *openRouterImageGenerator) GenerateImage(ctx context.Context, req ImageRequest) (ImageResult, error) {
	if err := req.validate(); err != nil {
		return ImageResult{}, err
	}
	var content any = req.Prompt
	if len(req.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": req.Prompt}}
		for _, img := range req.Images {
			mimeType := sniffImageMIME(img.Data, img.MIMEType)
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)},
			})
		}
		content = parts
	}
	payload := map[string]any{
		"model":      req.Model,
		"messages":   []map[string]any{{"role": "user", "content": content}},
		"modalities": []string{"image", "text"},
	}
	if ratio := aspectRatio(req.Size); ratio != "" || req.Quality != "" {
		cfg := map[string]any{}
		if ratio != "" {
			cfg["aspect_ratio"] = ratio
		}
		if req.Quality != "" {
			cfg["image_size"] = req.Quality
		}
		payload["image_config"] = cfg
	}
	for k, v := range req.ExtraBody {
		payload[k] = v
	}

	n := max(req.N, 1)
	body, err := json.Marshal(payload)
	if err != nil {
		return ImageResult{}, err
	}
	var result ImageResult
	for i := 0; i < n; i++ {
		one, err := g.once(ctx, body)
		if err != nil {
			if len(result.Images) > 0 {
				break
			}
			return ImageResult{}, err
		}
		result.Images = append(result.Images, one.Images...)
		if one.Usage != nil {
			if result.Usage == nil {
				result.Usage = &ImageUsage{}
			}
			result.Usage.InputTokens += one.Usage.InputTokens
			result.Usage.OutputTokens += one.Usage.OutputTokens
			result.Usage.TotalTokens += one.Usage.TotalTokens
			result.Usage.Cost += one.Usage.Cost
		}
	}
	return result, nil
}

func (g *openRouterImageGenerator) once(ctx context.Context, body []byte) (ImageResult, error) {
	base := strings.TrimRight(strings.TrimSpace(g.apiURL), "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ImageResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	settings := sdk.Current()
	if settings.AppURL != "" {
		httpReq.Header.Set("HTTP-Referer", settings.AppURL)
	}
	if settings.AppName != "" {
		httpReq.Header.Set("X-Title", settings.AppName)
	}
	if err := setBearerAuth(ctx, httpReq, g.apiKey, g.tokenSource); err != nil {
		return ImageResult{}, err
	}
	raw, err := doImageRequest(g.httpClient, httpReq)
	if err != nil {
		return ImageResult{}, err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Images  []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
		Usage tokenUsagePayload `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ImageResult{}, fmt.Errorf("decode image response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return ImageResult{}, fmt.Errorf("image API error: %s", parsed.Error.Message)
	}
	var result ImageResult
	for _, choice := range parsed.Choices {
		for _, img := range choice.Message.Images {
			data, mimeType, err := decodeDataURI(img.ImageURL.URL)
			if err != nil {
				return ImageResult{}, err
			}
			result.Images = append(result.Images, GeneratedImage{Data: data, MIMEType: sniffImageMIME(data, mimeType), RevisedPrompt: strings.TrimSpace(choice.Message.Content)})
		}
	}
	if len(result.Images) == 0 {
		return ImageResult{}, fmt.Errorf("model returned no images (is it an image-output model?)")
	}
	if u := parsed.Usage.normalized(); u != nil {
		result.Usage = &ImageUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens, Cost: u.Cost}
	}
	return result, nil
}

// aspectRatio turns a size such as "1536x1024" into "3:2" for providers that
// take an aspect ratio; "16:9" passes through and anything else is dropped.
func aspectRatio(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	if strings.Contains(size, ":") {
		return size
	}
	ws, hs, ok := strings.Cut(size, "x")
	if !ok {
		return ""
	}
	w, errW := strconv.Atoi(strings.TrimSpace(ws))
	h, errH := strconv.Atoi(strings.TrimSpace(hs))
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return ""
	}
	a, b := w, h
	for b != 0 {
		a, b = b, a%b
	}
	return fmt.Sprintf("%d:%d", w/a, h/a)
}

func decodeDataURI(uri string) ([]byte, string, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return nil, "", fmt.Errorf("generated image is not a data URI")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return nil, "", fmt.Errorf("generated image data URI is not base64")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", fmt.Errorf("decode image data: %w", err)
	}
	return data, strings.TrimSuffix(meta, ";base64"), nil
}
