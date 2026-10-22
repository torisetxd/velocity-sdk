package webfetch

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	//go:embed dom.js
	domPrelude string
	//go:embed engine.js
	enginePrelude string

	domProgram    = goja.MustCompile("dom.js", domPrelude, false)
	engineProgram = goja.MustCompile("engine.js", enginePrelude, false)
)

const (
	// maxScriptedPage bounds which responses are run as scripts at all:
	// interstitials are small, and real pages are left to the converter.
	maxScriptedPage = 512 * 1024
	// thinPageText is how much visible text a 2xx page may carry and still be
	// treated as an interstitial rather than content.
	thinPageText = 1500
	// scriptBudget is the wall-clock time a page's scripts get, timers included.
	scriptBudget   = 3 * time.Second
	maxScriptTicks = 512
	maxReports     = 64
	// maxRefreshDelay is the longest meta refresh still followed; longer ones are
	// pages that poll for updates, not interstitials.
	maxRefreshDelay = 10 * time.Second
)

var (
	errNavigated = errors.New("navigated")
	errBudget    = errors.New("script budget exhausted")
)

// navigation is the request a page's scripts (or meta refresh) asked for.
type navigation struct {
	method  string
	url     string
	body    string
	delay   time.Duration
	cookies []*http.Cookie
}

// scriptedPage parses body when it could be an interstitial: small, carrying
// a script or a refresh, and either blocked or nearly empty of visible text.
func scriptedPage(status int, body string) (*html.Node, bool) {
	if len(body) > maxScriptedPage {
		return nil, false
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "<script") && !strings.Contains(lower, "refresh") {
		return nil, false
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, false
	}
	if !blocked(status, body) && visibleText(doc, thinPageText) > thinPageText {
		return nil, false
	}
	return doc, true
}

// visibleText counts the text a reader would see, stopping once it passes limit.
func visibleText(n *html.Node, limit int) int {
	total := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if total > limit {
			return
		}
		switch n.Type {
		case html.TextNode:
			total += len(strings.TrimSpace(n.Data))
			return
		case html.ElementNode:
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Head:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return total
}

// runPage executes doc's inline scripts in a sandboxed JS engine over a
// minimal DOM, then its DOMContentLoaded and load handlers and its timers on a
// virtual clock, and reports the first navigation the page attempts. It is
// offline: external scripts, fetch and XHR are unavailable, so this clears
// interstitials that compute an answer and submit a form, set a cookie and
// reload, or redirect (see renderApp for pages that need the network).
func runPage(target string, doc *html.Node, userAgent string, cookies []*http.Cookie) (nav navigation, ok bool) {
	defer func() {
		if recover() != nil {
			nav, ok = navigation{}, false
		}
	}()
	page, err := url.Parse(target)
	if err != nil {
		return navigation{}, false
	}
	s := &pageSession{page: page, cookies: append([]*http.Cookie(nil), cookies...)}
	if !s.start(doc, target, userAgent) {
		return navigation{}, false
	}
	s.settle(scriptBudget, maxScriptTicks)
	if s.nav == nil {
		if nav, ok := metaRefresh(doc, page); ok {
			s.nav = &nav
		}
	}
	if s.nav == nil {
		return navigation{}, false
	}
	s.nav.cookies = s.written
	return *s.nav, true
}

// start loads doc into a fresh engine with the DOM prelude.
func (s *pageSession) start(doc *html.Node, target, userAgent string) bool {
	root := documentElement(doc)
	if root == nil {
		return false
	}
	tree, err := json.Marshal(jsTree(root))
	if err != nil {
		return false
	}
	s.vm = goja.New()
	s.vm.SetPromiseRejectionTracker(s.trackRejection)
	s.vm.Set("__host", s.host())
	s.vm.Set("__tree", string(tree))
	s.vm.Set("__url", target)
	s.vm.Set("__ua", userAgent)
	if _, err := s.vm.RunProgram(engineProgram); err != nil {
		return false
	}
	if _, err := s.vm.RunProgram(domProgram); err != nil {
		return false
	}
	s.hooks = s.vm.Get("__page").ToObject(s.vm)
	return true
}

// settle boots the page and pumps its event loop until it goes idle,
// navigates, or runs out of time or steps.
func (s *pageSession) settle(budget time.Duration, steps int) {
	timer := time.AfterFunc(budget, func() { s.vm.Interrupt(errBudget) })
	defer timer.Stop()
	if _, ok := s.call("boot"); !ok {
		return
	}
	for i := 0; i < steps && s.nav == nil; i++ {
		more, ok := s.call("pump")
		if !ok || !more.ToBoolean() {
			return
		}
	}
}

// call invokes a __page hook; ok is false once the engine was interrupted.
func (s *pageSession) call(name string) (goja.Value, bool) {
	fn, ok := goja.AssertFunction(s.hooks.Get(name))
	if !ok {
		return nil, false
	}
	v, err := fn(goja.Undefined())
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return nil, false
	}
	if err != nil {
		s.report(err.Error())
		return goja.Undefined(), true
	}
	return v, true
}

// runScript runs a classic script at global scope, lowered to syntax the
// engine supports when esbuild can parse it.
func (s *pageSession) runScript(code, name string) (ran bool) {
	defer s.recoverEngine(name, func() { ran = false })
	if lowered, err := transformJS(code, name, false); err == nil {
		code = lowered
	}
	_, err := s.vm.RunScript(name, code)
	return s.nested(err)
}

// nested handles the error of a script run from inside a host call: an
// interrupt is re-raised so the outer run stops too, anything else is a
// script error the page carries on from.
func (s *pageSession) nested(err error) bool {
	if err == nil {
		return true
	}
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		s.vm.Interrupt(interrupted.Value())
		return false
	}
	s.report(err.Error())
	return false
}

// trackRejection reports promise rejections nothing handled, which is how
// most framework render failures surface.
func (s *pageSession) trackRejection(p *goja.Promise, op goja.PromiseRejectionOperation) {
	if op != goja.PromiseRejectionReject {
		return
	}
	reason := p.Result()
	if o, ok := reason.(*goja.Object); ok {
		if stack := o.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			s.report("unhandled rejection: " + stack.String())
			return
		}
	}
	s.report(fmt.Sprintf("unhandled rejection: %v", reason))
}

// recoverEngine turns a panic out of the engine's parser (goja has a few on
// unusual literals) into a failed script instead of a crashed fetch.
func (s *pageSession) recoverEngine(name string, failed func()) {
	if r := recover(); r != nil {
		s.report(fmt.Sprintf("%s: engine panic: %v", name, r))
		failed()
	}
}

func (s *pageSession) report(message string) {
	if len(s.reports) < maxReports {
		s.reports = append(s.reports, message)
	}
}

func documentElement(doc *html.Node) *html.Node {
	for c := doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

// jsTree is the shape dom.js builds its document from: an element is
// {t, a, c} plus n for a foreign namespace, a comment is {x}, and a text node
// is a bare string.
func jsTree(n *html.Node) any {
	switch n.Type {
	case html.TextNode:
		return n.Data
	case html.CommentNode:
		return map[string]any{"x": n.Data}
	}
	attrs := make([][2]string, 0, len(n.Attr))
	for _, a := range n.Attr {
		attrs = append(attrs, [2]string{a.Key, a.Val})
	}
	children := []any{}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode || c.Type == html.TextNode || c.Type == html.CommentNode {
			children = append(children, jsTree(c))
		}
	}
	node := map[string]any{"t": n.Data, "a": attrs, "c": children}
	if n.Namespace != "" {
		node["n"] = n.Namespace
	}
	return node
}

func metaRefresh(doc *html.Node, page *url.URL) (navigation, bool) {
	var content string
	var find func(*html.Node) bool
	find = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.DataAtom == atom.Meta && strings.EqualFold(htmlAttr(n, "http-equiv"), "refresh") {
			content = htmlAttr(n, "content")
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if find(c) {
				return true
			}
		}
		return false
	}
	if !find(doc) {
		return navigation{}, false
	}
	delayText, rest, _ := strings.Cut(content, ";")
	seconds, err := strconv.ParseFloat(strings.TrimSpace(delayText), 64)
	if err != nil || seconds < 0 {
		return navigation{}, false
	}
	delay := time.Duration(seconds * float64(time.Second))
	if delay > maxRefreshDelay {
		return navigation{}, false
	}
	rest = strings.TrimSpace(rest)
	if key, value, ok := strings.Cut(rest, "="); ok && strings.EqualFold(strings.TrimSpace(key), "url") {
		rest = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	next := page
	if rest != "" {
		ref, err := page.Parse(rest)
		if err != nil || (ref.Scheme != "http" && ref.Scheme != "https") {
			return navigation{}, false
		}
		next = ref
	}
	clean := *next
	clean.Fragment = ""
	return navigation{method: http.MethodGet, url: clean.String(), delay: delay}, true
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// pageSession is the Go half of dom.js: it records the navigation and the
// cookies the page writes, and lends the engine the URL, HTML and hashing
// primitives it lacks.
type pageSession struct {
	vm      *goja.Runtime
	hooks   *goja.Object
	page    *url.URL
	nav     *navigation
	cookies []*http.Cookie
	written []*http.Cookie
	reports []string
	// net is set when the page may reach the network (render mode).
	net *pageNetwork
}

func (s *pageSession) host() map[string]any {
	host := map[string]any{
		"navigate":      s.navigate,
		"resolve":       s.resolve,
		"parseURL":      s.parseURL,
		"withPart":      s.withPart,
		"getCookie":     s.getCookie,
		"setCookie":     s.setCookie,
		"parseFragment": parseFragmentJSON,
		"parseDocument": parseDocumentJSON,
		"atob":          s.atob,
		"btoa":          s.btoa,
		"utf8Encode":    func(text string) goja.ArrayBuffer { return s.vm.NewArrayBuffer([]byte(text)) },
		"utf8Decode":    func(data goja.Value) string { return string(exportBytes(data)) },
		"digest":        s.digest,
		"runScript":     s.runScript,
		"report":        s.report,
	}
	if s.net != nil {
		host["request"] = s.request
		host["loadScript"] = s.loadScript
		host["loadModule"] = s.loadModule
		host["compileModule"] = s.compileModule
	}
	return host
}

func (s *pageSession) navigate(method, target, pairsJSON string, clock float64, reload bool) {
	if s.nav != nil {
		return
	}
	next, err := s.page.Parse(target)
	if err != nil || (next.Scheme != "http" && next.Scheme != "https") || next.Host == "" {
		return
	}
	if !reload && pairsJSON == "" && next.Fragment != "" {
		here, there := *s.page, *next
		here.Fragment, there.Fragment = "", ""
		if here.String() == there.String() {
			return
		}
	}
	next.Fragment = ""
	nav := navigation{method: http.MethodGet, delay: time.Duration(clock * float64(time.Millisecond))}
	if pairsJSON != "" {
		var pairs [][2]string
		if json.Unmarshal([]byte(pairsJSON), &pairs) != nil {
			return
		}
		encoded := encodePairs(pairs)
		if strings.EqualFold(method, http.MethodPost) {
			nav.method, nav.body = http.MethodPost, encoded
		} else {
			next.RawQuery = encoded
		}
	}
	nav.url = next.String()
	s.nav = &nav
	s.vm.Interrupt(errNavigated)
}

// encodePairs is form encoding in field order; url.Values would sort it.
func encodePairs(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	return strings.Join(parts, "&")
}

func (s *pageSession) resolve(ref, base string) string {
	b, err := url.Parse(base)
	if err != nil {
		b = s.page
	}
	u, err := b.Parse(ref)
	if err != nil {
		return ""
	}
	return u.String()
}

func (s *pageSession) parseURL(href, base string) any {
	var u *url.URL
	var err error
	if base == "" {
		u, err = url.Parse(href)
	} else if b, berr := url.Parse(base); berr == nil {
		u, err = b.Parse(href)
	} else {
		err = berr
	}
	if err != nil || u.Scheme == "" {
		return nil
	}
	return urlParts(u)
}

func urlParts(u *url.URL) map[string]any {
	path := u.EscapedPath()
	if path == "" && u.Host != "" {
		path = "/"
	}
	search, hash := "", ""
	if u.RawQuery != "" {
		search = "?" + u.RawQuery
	}
	if u.Fragment != "" {
		hash = "#" + u.EscapedFragment()
	}
	password, _ := u.User.Password()
	return map[string]any{
		"href":     u.String(),
		"protocol": u.Scheme + ":",
		"username": u.User.Username(),
		"password": password,
		"host":     u.Host,
		"hostname": u.Hostname(),
		"port":     u.Port(),
		"pathname": path,
		"search":   search,
		"hash":     hash,
		"origin":   u.Scheme + "://" + u.Host,
	}
}

func (s *pageSession) withPart(href, part, value string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	switch part {
	case "protocol":
		u.Scheme = strings.TrimSuffix(value, ":")
	case "host":
		u.Host = value
	case "hostname":
		if port := u.Port(); port != "" {
			u.Host = value + ":" + port
		} else {
			u.Host = value
		}
	case "port":
		u.Host = u.Hostname()
		if value != "" {
			u.Host += ":" + value
		}
	case "pathname":
		u.Path, u.RawPath = "", ""
		if p, err := url.PathUnescape(value); err == nil {
			u.Path = p
		}
	case "search":
		u.RawQuery = strings.TrimPrefix(value, "?")
	case "hash":
		u.Fragment = strings.TrimPrefix(value, "#")
	}
	return u.String()
}

func (s *pageSession) getCookie() string {
	parts := make([]string, 0, len(s.cookies))
	for _, c := range s.cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func (s *pageSession) setCookie(line string) {
	c, err := http.ParseSetCookie(line)
	if err != nil {
		return
	}
	expired := c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now()))
	s.cookies = upsertCookie(s.cookies, c, expired)
	s.written = upsertCookie(s.written, c, expired)
}

func upsertCookie(list []*http.Cookie, c *http.Cookie, remove bool) []*http.Cookie {
	out := list[:0]
	for _, existing := range list {
		if existing.Name != c.Name {
			out = append(out, existing)
		}
	}
	if !remove {
		out = append(out, c)
	}
	return out
}

func (s *pageSession) atob(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r' {
			return -1
		}
		return r
	}, text)
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(text, "="))
	if err != nil {
		panic(s.vm.NewGoError(errors.New("InvalidCharacterError: atob input is not valid base64")))
	}
	return latin1(data)
}

func (s *pageSession) btoa(text string) string {
	data := make([]byte, 0, len(text))
	for _, r := range text {
		if r > 0xff {
			panic(s.vm.NewGoError(errors.New("InvalidCharacterError: btoa input is outside Latin-1")))
		}
		data = append(data, byte(r))
	}
	return base64.StdEncoding.EncodeToString(data)
}

// latin1 maps each byte to the code point of the same value, which is what
// a JS "binary string" is.
func latin1(data []byte) string {
	buf := make([]byte, 0, len(data)*2)
	for _, b := range data {
		buf = utf8.AppendRune(buf, rune(b))
	}
	return string(buf)
}

func (s *pageSession) digest(algorithm string, data goja.Value) goja.ArrayBuffer {
	var h hash.Hash
	switch strings.ToUpper(strings.ReplaceAll(algorithm, "-", "")) {
	case "SHA1":
		h = sha1.New()
	case "SHA256":
		h = sha256.New()
	case "SHA384":
		h = sha512.New384()
	case "SHA512":
		h = sha512.New()
	default:
		panic(s.vm.NewGoError(errors.New("NotSupportedError: unknown digest " + algorithm)))
	}
	h.Write(exportBytes(data))
	return s.vm.NewArrayBuffer(h.Sum(nil))
}

func exportBytes(v goja.Value) []byte {
	if v == nil {
		return nil
	}
	switch data := v.Export().(type) {
	case []byte:
		return data
	case goja.ArrayBuffer:
		return data.Bytes()
	}
	return nil
}

func parseDocumentJSON(markup string) string {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return `{"t":"html","a":[],"c":[]}`
	}
	data, err := json.Marshal(jsTree(documentElement(doc)))
	if err != nil {
		return `{"t":"html","a":[],"c":[]}`
	}
	return string(data)
}

func parseFragmentJSON(markup string) string {
	body := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := html.ParseFragment(strings.NewReader(markup), body)
	if err != nil {
		return "[]"
	}
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		if n.Type == html.ElementNode || n.Type == html.TextNode || n.Type == html.CommentNode {
			out = append(out, jsTree(n))
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(data)
}
