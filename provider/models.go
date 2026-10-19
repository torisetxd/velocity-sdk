package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func ProviderEndpoint(baseURL, endpoint string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint = strings.TrimLeft(strings.TrimSpace(endpoint), "/")
	for _, suffix := range []string{"/chat/completions", "/responses"} {
		if strings.HasSuffix(baseURL, suffix) {
			if "/" + endpoint == suffix {
				return baseURL
			}
			baseURL = strings.TrimSuffix(baseURL, suffix)
			break
		}
	}
	if strings.HasSuffix(baseURL, "/v1") || strings.HasSuffix(baseURL, "/zen/v1") || strings.HasSuffix(baseURL, "/api/v1") {
		return baseURL + "/" + endpoint
	}
	return baseURL + "/v1/" + endpoint
}

func FetchModelsFromEndpoint(ctx context.Context, apiKey, baseURL string, extraHeaders map[string]string) ([]ModelInfo, error) {
	return fetchModelList(ctx, apiKey, baseURL, extraHeaders, "")
}

// fetchModelList GETs the models listing, appending query (without "?") when set.
func fetchModelList(ctx context.Context, apiKey, baseURL string, extraHeaders map[string]string, query string) ([]ModelInfo, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error
	urls := []string{
		ProviderEndpoint(baseURL, "models"),
		strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models",
	}
	if query != "" {
		for i := range urls {
			urls[i] += "?" + query
		}
	}
	seenURLs := map[string]bool{}
	for _, u := range urls {
		if seenURLs[u] {
			continue
		}
		seenURLs[u] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Accept", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		for key, value := range extraHeaders {
			if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
				req.Header.Set(key, value)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 404 {
			lastErr = fmt.Errorf("GET %s -> 404", u)
			resp.Body.Close()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("GET %s -> %d", u, resp.StatusCode)
			resp.Body.Close()
			continue
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		processed := parseModelListJSON(raw)
		if len(processed) == 0 {
			lastErr = fmt.Errorf("no models found at %s", u)
			continue
		}
		sort.Slice(processed, func(i, j int) bool {
			return processed[i].ID < processed[j].ID
		})
		return processed, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no models found")
	}
	return nil, lastErr
}

func parseModelListJSON(raw []byte) []ModelInfo {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}

	var modelsList []any
	if m, ok := parsed.(map[string]any); ok {
		if data, has := m["data"]; has {
			if arr, ok := data.([]any); ok {
				modelsList = arr
			}
		}
	}
	if modelsList == nil {
		if arr, ok := parsed.([]any); ok {
			modelsList = arr
		}
	}

	seen := map[string]bool{}
	processed := make([]ModelInfo, 0, len(modelsList))
	for _, item := range modelsList {
		if mi, ok := item.(map[string]any); ok {
			id, _ := mi["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name, _ := mi["name"].(string)
			info := ModelInfo{ID: id, Name: strings.TrimSpace(name)}
			if arch, ok := mi["architecture"].(map[string]any); ok {
				info.InputModalities = stringList(arch["input_modalities"])
				info.OutputModalities = stringList(arch["output_modalities"])
			}
			processed = append(processed, info)
		}
	}
	return processed
}

func stringList(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func copyModelInfos(models []ModelInfo) []ModelInfo {
	return append([]ModelInfo(nil), models...)
}
