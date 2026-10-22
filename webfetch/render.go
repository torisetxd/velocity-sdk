package webfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
	"golang.org/x/net/html"
)

const (
	// DefaultRenderTimeout is how long a client-rendered page gets to boot,
	// load its data and settle when Options.RenderTimeout is zero; module
	// loading and requests count against it.
	DefaultRenderTimeout = 30 * time.Second
	maxRenderSteps       = 50000
	// maxRenderRequests and maxRenderBytes bound the scripts, modules and
	// fetches one render may make.
	maxRenderRequests = 128
	maxRenderBytes    = 32 * 1024 * 1024
)

// renderBlockedHosts never carry content: analytics, ads and error beacons
// only cost time, so their scripts and requests fail fast.
var renderBlockedHosts = []string{
	"googletagmanager.com", "google-analytics.com", "analytics.google.com", "doubleclick.net",
	"googlesyndication.com", "googleadservices.com", "segment.com", "segment.io", "hotjar.com",
	"facebook.net", "clarity.ms", "sentry.io", "newrelic.com", "nr-data.net", "datadoghq.com",
	"fullstory.com", "mixpanel.com", "amplitude.com", "heap.io", "intercom.io", "optimizely.com",
}

// pageNetwork is how a rendering page reaches the network: through the fetch's
// own client and identity, so the SSRF guard and impersonation still apply.
type pageNetwork struct {
	ctx      context.Context
	client   *http.Client
	opts     Options
	agent    Agent
	referer  string
	requests int
	bytes    int
}

// renderApp runs a client-rendered page the way a browser would, without one:
// its classic and module scripts load and execute (modules are rewritten to
// CommonJS by esbuild, since the engine has no module loader), fetch and XHR
// go out over the fetch's client, and once the event loop goes idle the live
// DOM is serialized back to HTML for the usual conversion. It only runs on an
// app shell, a page with next to no text of its own, and reports false when
// the rendered page is still empty.
func renderApp(ctx context.Context, client *http.Client, opts Options, target string, agent Agent, resp response) (rendered response, ok bool) {
	defer func() {
		if recover() != nil {
			rendered, ok = response{}, false
		}
	}()
	if len(resp.body) > maxScriptedPage || !strings.Contains(strings.ToLower(resp.body), "<script") {
		return response{}, false
	}
	doc, err := html.Parse(strings.NewReader(resp.body))
	if err != nil {
		return response{}, false
	}
	shellText := visibleText(doc, appShellText)
	if shellText > appShellText {
		return response{}, false
	}
	pageURL, err := url.Parse(target)
	if err != nil {
		return response{}, false
	}
	budget := opts.renderTimeout()
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var cookies []*http.Cookie
	for _, c := range resp.cookies {
		if !c.HttpOnly {
			cookies = append(cookies, c)
		}
	}
	s := &pageSession{
		page:    pageURL,
		cookies: cookies,
		net:     &pageNetwork{ctx: rctx, client: client, opts: opts, agent: agent, referer: target},
	}
	if !s.start(doc, target, agent.UserAgent) {
		return response{}, false
	}
	s.settle(budget, maxRenderSteps)
	if s.nav != nil {
		return response{}, false
	}

	// Whatever rendered before the budget ran out is still worth reading.
	s.vm.ClearInterrupt()
	guard := time.AfterFunc(2*time.Second, func() { s.vm.Interrupt(errBudget) })
	page, called := s.call("html")
	guard.Stop()
	if !called || page == nil {
		return response{}, false
	}
	out := page.String()
	result, err := html.Parse(strings.NewReader(out))
	if err != nil || visibleText(result, appShellText) <= shellText {
		return response{}, false
	}
	return response{status: resp.status, body: out, cookies: resp.cookies}, true
}

func renderBlocked(host string) bool {
	host = strings.ToLower(host)
	for _, blocked := range renderBlockedHosts {
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return true
		}
	}
	return false
}

// pageRequest is what dom.js receives for a fetch or XHR, as JSON.
type pageRequest struct {
	Status     int         `json:"status"`
	StatusText string      `json:"statusText"`
	URL        string      `json:"url"`
	Headers    [][2]string `json:"headers"`
	Body       string      `json:"body"`
}

// request performs one fetch or XHR for the page and returns the response as
// JSON, or "" when it was refused or failed (a network error, to the page).
func (s *pageSession) request(method, target, headersJSON, body string) string {
	resp, err := s.do(method, target, headersJSON, body)
	if err != nil {
		return ""
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *pageSession) do(method, target, headersJSON, body string) (pageRequest, error) {
	n := s.net
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return pageRequest{}, errors.New("unsupported url")
	}
	if renderBlocked(u.Hostname()) {
		return pageRequest{}, errors.New("blocked host")
	}
	if n.requests >= maxRenderRequests || n.bytes >= maxRenderBytes {
		return pageRequest{}, errors.New("request budget exhausted")
	}
	n.requests++

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(n.ctx, strings.ToUpper(method), u.String(), reader)
	if err != nil {
		return pageRequest{}, err
	}
	req.Header.Set("User-Agent", n.agent.UserAgent)
	for k, v := range n.agent.Headers {
		req.Header.Set(k, v)
	}
	var headers [][2]string
	if headersJSON != "" {
		_ = json.Unmarshal([]byte(headersJSON), &headers)
	}
	for _, h := range headers {
		switch strings.ToLower(h[0]) {
		case "host", "content-length", "cookie", "cookie2", "connection", "user-agent", "referer", "origin", "keep-alive", "transfer-encoding", "te", "upgrade":
			continue
		}
		req.Header.Add(h[0], h[1])
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	req.Header.Set("Referer", n.referer)
	if len(s.written) > 0 {
		if n.client.Jar != nil {
			n.client.Jar.SetCookies(req.URL, s.written)
		} else {
			for _, c := range s.written {
				req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
		}
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return pageRequest{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(min(n.opts.maxBody(), maxRenderBytes-n.bytes))))
	if err != nil {
		return pageRequest{}, err
	}
	n.bytes += len(data)
	out := pageRequest{Status: resp.StatusCode, StatusText: http.StatusText(resp.StatusCode), URL: u.String(), Body: string(data)}
	if resp.Request != nil && resp.Request.URL != nil {
		out.URL = resp.Request.URL.String()
	}
	for k, values := range resp.Header {
		for _, v := range values {
			out.Headers = append(out.Headers, [2]string{k, v})
		}
	}
	return out, nil
}

func (s *pageSession) fetchSource(target string) (string, bool) {
	resp, err := s.do(http.MethodGet, target, "", "")
	if err != nil || resp.Status < 200 || resp.Status >= 300 {
		s.report("failed to load " + target)
		return "", false
	}
	return resp.Body, true
}

// loadScript fetches and runs an external classic script.
func (s *pageSession) loadScript(target string) bool {
	code, ok := s.fetchSource(target)
	if !ok {
		return false
	}
	return s.runScript(code, target)
}

// loadModule fetches an ES module and compiles it to a CommonJS wrapper for
// dom.js's loader, or returns null.
func (s *pageSession) loadModule(target string) goja.Value {
	code, ok := s.fetchSource(target)
	if !ok {
		return goja.Null()
	}
	return s.compileModule(code, target)
}

func (s *pageSession) compileModule(code, name string) (fn goja.Value) {
	defer s.recoverEngine(name, func() { fn = goja.Null() })
	lowered, err := transformJS(code, name, true)
	if err != nil {
		s.report(name + ": " + err.Error())
		return goja.Null()
	}
	fn, err = s.vm.RunScript(name, "(function (exports, require, module, __import_meta) {"+lowered+"\n})")
	if !s.nested(err) {
		return goja.Null()
	}
	return fn
}

// transformJS lowers modern syntax to what goja runs and, for a module,
// rewrites imports and exports to CommonJS (import() becomes a lazy require)
// with import.meta supplied by the loader.
func transformJS(code, name string, module bool) (string, error) {
	options := api.TransformOptions{
		Loader:     api.LoaderJS,
		Target:     api.ES2017,
		Sourcefile: name,
		LogLevel:   api.LogLevelSilent,
		Supported:  map[string]bool{"bigint": true, "dynamic-import": false},
	}
	if module {
		options.Format = api.FormatCommonJS
		options.Define = map[string]string{"import.meta": "__import_meta"}
	}
	result := api.Transform(code, options)
	if len(result.Errors) > 0 {
		return "", errors.New(result.Errors[0].Text)
	}
	return splitBraceEscapes(string(result.Code)), nil
}

// splitBraceEscapes rewrites every \u{...} escape whose backslash is not
// itself escaped as \uXXXX, or a surrogate pair above the BMP. goja's lexer
// sizes a string before decoding it and panics when one escape decodes to two
// UTF-16 units; the pair means the same in a string, a template and a
// u-flagged regexp.
func splitBraceEscapes(code string) string {
	if !strings.Contains(code, `\u{`) {
		return code
	}
	var b strings.Builder
	b.Grow(len(code))
	last := 0
	for i := 0; i+2 < len(code); i++ {
		if code[i] != '\\' || code[i+1] != 'u' || code[i+2] != '{' {
			continue
		}
		run := 0
		for j := i - 1; j >= 0 && code[j] == '\\'; j-- {
			run++
		}
		end := strings.IndexByte(code[i:], '}')
		if run%2 == 1 || end < 0 {
			continue
		}
		value, err := strconv.ParseUint(code[i+3:i+end], 16, 32)
		if err != nil || value > unicode.MaxRune {
			continue
		}
		b.WriteString(code[last:i])
		if value <= 0xFFFF {
			fmt.Fprintf(&b, `\u%04X`, value)
		} else {
			hi, lo := utf16.EncodeRune(rune(value))
			fmt.Fprintf(&b, `\u%04X\u%04X`, hi, lo)
		}
		last = i + end + 1
		i = last - 1
	}
	b.WriteString(code[last:])
	return b.String()
}
