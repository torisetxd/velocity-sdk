// Package webfetch fetches a URL and returns it as bounded, model-ready
// markdown. It prefers markdown endpoints (.md, .mdx, Accept: text/markdown),
// falls back to boilerplate-pruned HTML conversion, reports binary payloads as
// a short notice, and can retry as a crawler (Googlebot, GPTBot, ...) when a
// site serves a bot wall to the default client.
package webfetch

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	vcontext "velocitylabs.cc/sdk/context"
)

const (
	// DefaultMaxBodyBytes bounds how many bytes of a response are retained
	// before tokenization, so a hostile or accidental binary payload is never
	// read in full.
	DefaultMaxBodyBytes = 4 * 1024 * 1024
	// DefaultUserAgent is presented when Options.UserAgent is empty.
	DefaultUserAgent = "VelocitySDK/1.0"
)

// Options configures a fetch. The zero value is usable.
type Options struct {
	// Client performs the requests. Defaults to a client with a 30s timeout;
	// use NewSafeClient when the URL is untrusted.
	Client *http.Client
	// UserAgent is the identity presented first. Defaults to DefaultUserAgent.
	UserAgent string
	// Agent, when set, replaces UserAgent for every request (no spoof ladder).
	Agent *Agent
	// Spoof retries as each of SpoofAgents when the page looks blocked.
	Spoof bool
	// SpoofAgents overrides DefaultSpoofAgents.
	SpoofAgents []Agent
	// MaxBodyBytes defaults to DefaultMaxBodyBytes.
	MaxBodyBytes int
	// TokenBudget caps the model-facing output; defaults to
	// context.DefaultModelTokenBudget.
	TokenBudget int
	// EmailMode decides what happens to Cloudflare-obfuscated addresses.
	EmailMode EmailMode
	// Censor, if set, is applied to fetched text before it is returned
	// (e.g. secret redaction).
	Censor func(string) string
	// SkipMarkdownProbes disables the .md/.mdx/Accept probes and fetches the
	// URL once as HTML.
	SkipMarkdownProbes bool
}

func (o Options) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (o Options) maxBody() int {
	if o.MaxBodyBytes > 0 {
		return o.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

func (o Options) budget() int {
	if o.TokenBudget > 0 {
		return o.TokenBudget
	}
	return vcontext.DefaultModelTokenBudget
}

func (o Options) baseAgent() Agent {
	if o.Agent != nil {
		return *o.Agent
	}
	ua := strings.TrimSpace(o.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}
	return Agent{UserAgent: ua}
}

type response struct {
	status int
	body   string
	cut    bool
}

// Fetch returns the page at rawURL as markdown preceded by a source header.
func Fetch(ctx context.Context, rawURL string, opts Options) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("url must be an absolute http(s) URL")
	}
	agent := opts.baseAgent()

	if !opts.SkipMarkdownProbes {
		trimmed := strings.TrimSuffix(rawURL, "/")
		for _, probe := range []struct{ url, accept string }{
			{trimmed + ".md", "text/markdown"},
			{trimmed + ".mdx", "text/markdown"},
			{rawURL, "text/markdown"},
		} {
			resp, err := get(ctx, opts, probe.url, probe.accept, agent)
			if err != nil || resp.status < 200 || resp.status >= 300 || strings.TrimSpace(resp.body) == "" {
				continue
			}
			if isBinaryContent(resp.body) {
				return finalize(rawURL, resp.body, resp.cut, opts), nil
			}
			if contains404(resp.body) || looksLikeHTML(resp.body) {
				continue
			}
			return finalize(rawURL, resp.body, resp.cut, opts), nil
		}
	}

	ladder := []Agent{agent}
	if opts.Agent == nil && opts.Spoof {
		spoof := opts.SpoofAgents
		if len(spoof) == 0 {
			spoof = DefaultSpoofAgents()
		}
		ladder = append(ladder, spoof...)
	}

	var last response
	var lastErr error
	for _, a := range ladder {
		resp, err := get(ctx, opts, rawURL, "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8", a)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrBlockedAddress) {
				return "", err
			}
			lastErr = err
			continue
		}
		last, lastErr = resp, nil
		if resp.status >= 200 && resp.status < 300 && !blocked(resp.status, resp.body) {
			return render(rawURL, resp, opts)
		}
		if !blocked(resp.status, resp.body) {
			break
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	if last.status < 200 || last.status >= 300 {
		return "", fmt.Errorf("HTTP %d", last.status)
	}
	// Every identity hit a wall that still returned 2xx; hand over what we have.
	return render(rawURL, last, opts)
}

func render(source string, resp response, opts Options) (string, error) {
	if isBinaryContent(resp.body) {
		return finalize(source, resp.body, resp.cut, opts), nil
	}
	md, err := convertHTMLToMarkdown([]byte(resp.body), opts.EmailMode)
	if err != nil {
		return "", fmt.Errorf("failed to convert HTML to markdown: %w", err)
	}
	return finalize(source, md, resp.cut, opts), nil
}

func get(ctx context.Context, opts Options, target, accept string, agent Agent) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", agent.UserAgent)
	for k, v := range agent.Headers {
		req.Header.Set(k, v)
	}
	resp, err := opts.client().Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	limit := opts.maxBody()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return response{}, err
	}
	cut := len(data) > limit
	if cut {
		data = data[:limit]
	}
	return response{status: resp.StatusCode, body: string(data), cut: cut}, nil
}

// finalize is the single exit point for fetched content: binary payloads
// become a bounded notice and everything else is truncated to the token budget.
func finalize(source, content string, cut bool, opts Options) string {
	if isBinaryContent(content) {
		return formatBinary(source, content, cut, opts)
	}
	out := formatMarkdown(source, content, opts)
	if cut {
		out += fmt.Sprintf("\n[response body capped at %d bytes; content above is incomplete]\n", opts.maxBody())
	}
	limited, _ := vcontext.TruncateText(out, opts.budget())
	return limited
}

func formatMarkdown(source, markdown string, opts Options) string {
	markdown = strings.TrimSpace(markdown)
	if markdown == "" {
		return fmt.Sprintf("---\nsource: %s\n---\n", source)
	}
	if opts.Censor != nil {
		markdown = opts.Censor(markdown)
	}
	return fmt.Sprintf("---\nsource: %s\n---\n\n%s\n", source, markdown)
}

func formatBinary(source, content string, cut bool, opts Options) string {
	suffix := ""
	if cut {
		suffix = fmt.Sprintf(" (at least %d bytes; body capped)", opts.maxBody())
	}
	preview := content
	if len(preview) > 256 {
		preview = preview[:256]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nsource: %s\n---\n\n", source)
	fmt.Fprintf(&b, "[binary content: %d bytes%s; not shown as text]\n", len(content), suffix)
	if len(preview) > 0 {
		fmt.Fprintf(&b, "\nfirst %d bytes (hex):\n%s\n", len(preview), hex.Dump([]byte(preview)))
	}
	limited, _ := vcontext.TruncateText(b.String(), opts.budget())
	return limited
}

// isBinaryContent reports whether content looks like a non-text payload, using
// the same sniffing net/http does.
func isBinaryContent(content string) bool {
	sample := content
	if len(sample) > 512 {
		sample = sample[:512]
	}
	if strings.TrimSpace(sample) == "" {
		return false
	}
	ct := http.DetectContentType([]byte(sample))
	if strings.HasPrefix(ct, "text/") {
		return false
	}
	switch {
	case strings.Contains(ct, "json"),
		strings.Contains(ct, "xml"),
		strings.Contains(ct, "javascript"),
		strings.HasPrefix(ct, "message/"):
		return false
	default:
		return true
	}
}

// looksLikeHTML catches servers that ignore Accept and answer a markdown probe
// with the HTML page, which should go through the HTML cleaner instead.
func looksLikeHTML(s string) bool {
	head := strings.ToLower(strings.TrimSpace(s))
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

func contains404(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "404") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "does not exist")
}
