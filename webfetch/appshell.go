package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	// appShellText is how much visible text a page may have and still be an
	// empty shell that a client-side app fills in.
	appShellText = 200
	// maxShellScript bounds the external scripts read for data URLs: config and
	// initializer files are small, framework bundles are not worth scanning.
	maxShellScript  = 128 * 1024
	maxShellScripts = 6
	maxDataAttempts = 3
	dataAccept      = "application/json, application/yaml;q=0.9, text/yaml;q=0.9, */*;q=0.5"
)

// dataURLRE finds what in a script names a document an app loads to render
// itself (a JSON or YAML file, or an OpenAPI/Swagger spec): an absolute URL
// anywhere, since configs pack several into one string, or a quoted relative
// path.
var (
	dataURLRE  = regexp.MustCompile("(https?://[^\"'`\\s,;|<>()]+|[\"'`][^\"'`\\s:]+[\"'`])")
	dataPathRE = regexp.MustCompile(`(?i)(\.json|\.ya?ml)$|openapi|swagger|api-docs`)
	yamlKeyRE  = regexp.MustCompile(`(?m)^[A-Za-z_][\w-]*:\s`)
)

// appShellData handles a page whose HTML is an empty mount point for a
// client-side app (Swagger UI, many docs viewers). Running the app would mean
// running its framework bundle, so instead the inline and small same-origin
// scripts are scanned for the data document the app loads, and that document
// is returned in place of the empty page.
func appShellData(ctx context.Context, client *http.Client, opts Options, target string, agent Agent, resp response) (string, bool) {
	if len(resp.body) > maxScriptedPage || looksLikeData(resp.body) {
		return "", false
	}
	doc, err := html.Parse(strings.NewReader(resp.body))
	if err != nil || visibleText(doc, appShellText) > appShellText {
		return "", false
	}
	page, err := url.Parse(target)
	if err != nil {
		return "", false
	}

	var inline []string
	var external []*url.URL
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Script {
			if src := htmlAttr(n, "src"); src != "" {
				if u, err := page.Parse(src); err == nil && u.Host == page.Host {
					external = append(external, u)
				}
			} else if n.FirstChild != nil {
				inline = append(inline, n.FirstChild.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	sources := inline
	scriptOpts := opts
	scriptOpts.MaxBodyBytes = maxShellScript
	for i, u := range external {
		if i == maxShellScripts {
			break
		}
		script, err := get(ctx, client, scriptOpts, u.String(), "*/*", agent)
		if err != nil || script.cut || script.status < 200 || script.status >= 300 {
			continue
		}
		sources = append(sources, script.body)
	}

	for i, candidate := range dataCandidates(page, sources) {
		if i == maxDataAttempts {
			break
		}
		data, err := get(ctx, client, opts, candidate, dataAccept, agent)
		if err != nil || data.status < 200 || data.status >= 300 || !looksLikeData(data.body) {
			continue
		}
		body := data.body
		var indented bytes.Buffer
		if json.Indent(&indented, []byte(body), "", "  ") == nil {
			body = indented.String()
		}
		note := fmt.Sprintf("[%s renders in the browser from the document below, loaded from %s]\n\n", target, candidate)
		return finalize(target, note+body, data.cut, opts), true
	}
	return "", false
}

// dataCandidates lists the data URLs the scripts mention, same-host ones
// first and otherwise in order of appearance.
func dataCandidates(page *url.URL, sources []string) []string {
	seen := map[string]bool{}
	var local, remote []string
	for _, source := range sources {
		for _, match := range dataURLRE.FindAllString(source, -1) {
			u, err := page.Parse(strings.Trim(match, "\"'`"))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !dataPathRE.MatchString(u.Path) || strings.Contains(strings.ToLower(u.Path), "manifest") {
				continue
			}
			u.Fragment = ""
			s := u.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			if u.Host == page.Host {
				local = append(local, s)
			} else {
				remote = append(remote, s)
			}
		}
	}
	return append(local, remote...)
}

// looksLikeData accepts a JSON document or a YAML one that is not HTML.
func looksLikeData(body string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || looksLikeHTML(trimmed) {
		return false
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed[0] == '{' || trimmed[0] == '['
	}
	head := trimmed
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.HasPrefix(head, "openapi:") || strings.HasPrefix(head, "swagger:") ||
		strings.HasPrefix(head, "---") || yamlKeyRE.MatchString(head) && !strings.Contains(head, "<")
}
