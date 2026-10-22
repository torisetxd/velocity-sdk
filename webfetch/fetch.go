// Package webfetch fetches a URL and returns it as bounded, model-ready
// markdown. It prefers markdown endpoints (.md, .mdx, Accept: text/markdown),
// falls back to boilerplate-pruned HTML conversion, reports binary payloads as
// a short notice, and can retry as a crawler (Googlebot, GPTBot, ...) when a
// site serves a bot wall to the default client. Challenge walls score the TLS
// handshake rather than the user agent, so it can also replay the request over
// a real browser's TLS and HTTP/2 fingerprint (see ImpersonateClient). A
// JavaScript interstitial is cleared by running its inline scripts in an
// embedded engine over a minimal DOM and following the navigation they make.
package webfetch

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	// htmlAccept is what a browser sends for a document navigation.
	htmlAccept = "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8"
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
	// Impersonate drives the whole fetch over a real browser's TLS and HTTP/2
	// fingerprint instead of Go's, using the named profile from
	// profiles.MappedTLSClients ("chrome_150", "firefox_147", ...). Empty keeps
	// the stdlib transport. When it is set and the caller pinned no UserAgent or
	// Agent, the browser identity is presented so the header and the handshake
	// agree. Client is ignored in that case.
	Impersonate string
	// ImpersonateOnBlock retries once with DefaultImpersonateProfile after the
	// agent ladder has been refused. Bot walls fingerprint the handshake, not
	// the user-agent, so this is what turns a dozen refused ladder requests into
	// a successful pair. The attempt is skipped when it cannot help, and a
	// refusal to dial (a private address, which the impersonating client guards
	// against) never masks the ladder's own result.
	ImpersonateOnBlock bool
	// RenderApps runs a client-rendered page (an app shell with next to no
	// text of its own) the way a browser would, without one: its scripts and
	// modules load and execute in an embedded JS engine over a minimal DOM,
	// fetch and XHR go out over this fetch's client, and the DOM it settles on
	// is converted instead. It is tried after looking for the data document
	// the app loads, and costs the app's bundle download and seconds of CPU.
	RenderApps bool
	// RenderTimeout bounds one RenderApps render, scripts and requests
	// included; zero means DefaultRenderTimeout. A render that runs out
	// returns whatever the page had drawn by then, if anything.
	RenderTimeout time.Duration
}

func (o Options) renderTimeout() time.Duration {
	if o.RenderTimeout > 0 {
		return o.RenderTimeout
	}
	return DefaultRenderTimeout
}

func (o Options) client() *http.Client {
	if o.Client == nil {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Timeout: 30 * time.Second, Jar: jar}
	}
	if o.Client.Jar != nil {
		return o.Client
	}
	// Challenge walls (Cloudflare's __cf_bm, cf_clearance, ...) hand out their
	// clearance cookie on the first, refused response and only let the request
	// that echoes it through, so the jar has to survive across the agents in the
	// ladder. The caller's client is copied to keep this jar per-fetch.
	copied := *o.Client
	jar, _ := cookiejar.New(nil)
	copied.Jar = jar
	return &copied
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

// impersonationAgent is the identity to present alongside a browser
// fingerprint: a pinned caller choice wins, otherwise the browser profile.
func (o Options) impersonationAgent() Agent {
	if o.Agent == nil && strings.TrimSpace(o.UserAgent) == "" {
		return AgentBrowser
	}
	return o.baseAgent()
}

type response struct {
	status  int
	body    string
	cut     bool
	cookies []*http.Cookie
}

// maxInterstitialRounds bounds how many pages in a row may navigate before
// the chain is treated as a wall, and maxInterstitialWait caps how long a
// timer-delayed navigation is honoured in real time (some walls check that
// the delay they asked for actually elapsed).
const (
	maxInterstitialRounds = 3
	maxInterstitialWait   = 5 * time.Second
)

// passInterstitials runs a thin or blocked page's scripts and follows the
// navigation they make, with the client and identity that drew the page,
// until a page stops navigating. stuck reports a chain that never settled
// or whose next hop failed, which the caller treats as blocked.
func passInterstitials(ctx context.Context, client *http.Client, opts Options, target string, agent Agent, resp response) (response, bool) {
	var written []*http.Cookie
	for round := 0; ; round++ {
		doc, ok := scriptedPage(resp.status, resp.body)
		if !ok {
			return resp, false
		}
		visible := append([]*http.Cookie(nil), written...)
		for _, c := range resp.cookies {
			if !c.HttpOnly {
				visible = upsertCookie(visible, c, false)
			}
		}
		nav, ok := runPage(target, doc, agent.UserAgent, visible)
		if !ok {
			return resp, false
		}
		if round == maxInterstitialRounds {
			return resp, true
		}
		if wait := min(nav.delay, maxInterstitialWait); wait > 0 {
			select {
			case <-ctx.Done():
				return resp, true
			case <-time.After(wait):
			}
		}
		for _, c := range nav.cookies {
			written = upsertCookie(written, c, false)
		}
		next, err := send(ctx, client, opts, nav.method, nav.url, nav.body, htmlAccept, agent, written)
		if err != nil {
			return resp, true
		}
		target, resp = nav.url, next
	}
}

// Fetch returns the page at rawURL as markdown preceded by a source header.
func Fetch(ctx context.Context, rawURL string, opts Options) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("url must be an absolute http(s) URL")
	}
	agent := opts.baseAgent()
	client := opts.client()
	impersonating := opts.Impersonate != ""
	if impersonating {
		impersonated, err := ImpersonateClient(opts.Impersonate, opts.client().Timeout)
		if err != nil {
			return "", err
		}
		client = impersonated
		agent = opts.impersonationAgent()
	}

	if !opts.SkipMarkdownProbes {
		trimmed := strings.TrimSuffix(rawURL, "/")
		for _, probe := range []struct{ url, accept string }{
			{trimmed + ".md", "text/markdown"},
			{trimmed + ".mdx", "text/markdown"},
			{rawURL, "text/markdown"},
		} {
			resp, err := get(ctx, client, opts, probe.url, probe.accept, agent)
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

	// Attempts run in increasing order of effort: the pinned identity, then a
	// browser handshake (the dimension a user-agent string cannot reach), and
	// only then the crawler rungs, which challenge walls tend to refuse outright.
	type attempt struct {
		client *http.Client
		agent  Agent
	}
	attempts := []attempt{{client: client, agent: agent}}
	if opts.ImpersonateOnBlock && !impersonating {
		if impersonated, err := ImpersonateClient(DefaultImpersonateProfile, opts.client().Timeout); err == nil {
			attempts = append(attempts, attempt{client: impersonated, agent: opts.impersonationAgent()})
		}
	}
	if opts.Agent == nil && opts.Spoof {
		spoof := opts.SpoofAgents
		if len(spoof) == 0 {
			spoof = DefaultSpoofAgents()
		}
		for _, a := range spoof {
			attempts = append(attempts, attempt{client: client, agent: a})
		}
	}

	var last response
	var lastErr error
	for index, at := range attempts {
		resp, err := get(ctx, at.client, opts, rawURL, htmlAccept, at.agent)
		if err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			// A refusal from an escalated attempt must not mask what the first one
			// saw: impersonation dials through the SSRF guard, so escalating a
			// localhost fetch is expected to be refused and the ladder's own
			// answer should stand.
			if index == 0 {
				if errors.Is(err, ErrBlockedAddress) {
					return "", err
				}
				lastErr = err
			}
			continue
		}
		resp, stuck := passInterstitials(ctx, at.client, opts, rawURL, at.agent, resp)
		last, lastErr = resp, nil
		walled := stuck || blocked(resp.status, resp.body)
		// An app shell's <noscript> fallback ("enable JavaScript to run this
		// app") reads as a wall, but running the app is exactly what clears it,
		// so client rendering gets the first look at any 2xx page.
		if resp.status >= 200 && resp.status < 300 && !stuck {
			if out, ok := appShellData(ctx, at.client, opts, rawURL, at.agent, resp); ok {
				return out, nil
			}
			if opts.RenderApps {
				if rendered, ok := renderApp(ctx, at.client, opts, rawURL, at.agent, resp); ok {
					return render(rawURL, rendered, opts)
				}
			}
		}
		if resp.status >= 200 && resp.status < 300 && !walled {
			return render(rawURL, resp, opts)
		}
		if !walled {
			break
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	if last.status < 200 || last.status >= 300 {
		return "", fmt.Errorf("HTTP %d", last.status)
	} // Every identity hit a wall that still returned 2xx; hand over what we have.
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

func get(ctx context.Context, client *http.Client, opts Options, target, accept string, agent Agent) (response, error) {
	return send(ctx, client, opts, http.MethodGet, target, "", accept, agent, nil)
}

// send performs one request. A non-empty form is sent as a urlencoded body,
// and cookies (ones a page's scripts set) go to the client's jar when it has
// one, or on the request otherwise, since the impersonating client keeps its
// jar out of reach.
func send(ctx context.Context, client *http.Client, opts Options, method, target, form, accept string, agent Agent, cookies []*http.Cookie) (response, error) {
	var body io.Reader
	if form != "" {
		body = strings.NewReader(form)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", agent.UserAgent)
	for k, v := range agent.Headers {
		req.Header.Set(k, v)
	}
	if form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if len(cookies) > 0 {
		if client.Jar != nil {
			client.Jar.SetCookies(req.URL, cookies)
		} else {
			for _, c := range cookies {
				req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
		}
	}
	resp, err := client.Do(req)
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
	return response{status: resp.StatusCode, body: string(data), cut: cut, cookies: resp.Cookies()}, nil
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
	head := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\xef\xbb\xbf")))
	if len(head) > 4096 {
		head = head[:4096]
	}
	// Static bundles often open with a build comment before the doctype.
	for strings.HasPrefix(head, "<!--") {
		end := strings.Index(head, "-->")
		if end < 0 {
			break
		}
		head = strings.TrimSpace(head[end+3:])
	}
	for _, prefix := range []string{"<!doctype html", "<html", "<head", "<body"} {
		if strings.HasPrefix(head, prefix) {
			return true
		}
	}
	return false
}

func contains404(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "404") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "does not exist")
}
