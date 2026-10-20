package webfetch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"velocitylabs.cc/sdk/provider"
)

// ToolName is the name of the tool returned by ToolDef.
const ToolName = "web_fetch"

// ToolDef is a ready-made function-tool definition for Execute.
func ToolDef() provider.ToolDef {
	return provider.ToolDef{
		Type: "function",
		Function: provider.FunctionDef{
			Name:        ToolName,
			Description: "Fetch a URL and return its content as markdown. Tries .md, .mdx extensions first, then falls back to HTML conversion. Pages that block automated clients are retried as search-engine and AI crawlers. Output is truncated to a token budget before the model sees it; binary payloads (e.g. images, parquet, archives) are reported as a bounded notice instead of raw bytes.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "The URL to fetch."},
				},
				"required": []string{"url"},
			},
		},
	}
}

// Execute runs the tool for a JSON arguments string and returns text for the
// model. Errors are returned as "tool error: ..." text, matching other tools.
func Execute(ctx context.Context, arguments string, opts Options) string {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "tool error: " + err.Error()
	}
	if strings.TrimSpace(args.URL) == "" {
		return "tool error: url is required"
	}
	out, err := Fetch(ctx, args.URL, opts)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return "tool error: refusing to fetch a private or internal address"
		}
		return "tool error: failed to fetch URL: " + err.Error()
	}
	return out
}
