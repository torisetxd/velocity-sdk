package webfetch

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
)

var (
	cfEmailRE      = regexp.MustCompile(`data-cfemail="([^"]+)"`)
	cfEmailLinkRE  = regexp.MustCompile(`/cdn-cgi/l/email-protection#([0-9a-fA-F]+)`)
	cfEmailClassRE = regexp.MustCompile(`\s*class="__cf_email__"`)
	cfEmailAttrRE  = regexp.MustCompile(`\s+data-cfemail="[^"]*"`)
)

// EmailMode controls how Cloudflare-obfuscated email addresses are handled.
type EmailMode int

const (
	// EmailDecode decodes obfuscated addresses back to plain text (default).
	EmailDecode EmailMode = iota
	// EmailStrip removes obfuscated addresses.
	EmailStrip
	// EmailIgnore leaves the markup untouched.
	EmailIgnore
)

func convertHTMLToMarkdown(input []byte, mode EmailMode) (string, error) {
	cleanedHTML := preCleanHTMLForMarkdown(input, mode)
	markdown, err := htmltomarkdown.ConvertReader(bytes.NewReader(cleanedHTML))
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(markdown)), nil
}

func preCleanHTMLForMarkdown(input []byte, mode EmailMode) []byte {
	if len(bytes.TrimSpace(input)) == 0 {
		return input
	}

	input = processCloudflareEmails(input, mode)

	doc, err := html.Parse(bytes.NewReader(input))
	if err != nil {
		return input
	}

	pruneHTMLBoilerplate(doc)

	var b bytes.Buffer
	b.Grow(len(input))
	if err := html.Render(&b, doc); err != nil {
		return input
	}

	return b.Bytes()
}

func pruneHTMLBoilerplate(root *html.Node) {
	if root == nil {
		return
	}

	stack := []*html.Node{root}
	for len(stack) > 0 {
		last := len(stack) - 1
		parent := stack[last]
		stack = stack[:last]

		for n := parent.FirstChild; n != nil; {
			next := n.NextSibling
			if shouldRemoveNode(n) {
				removeNode(n)
			} else {
				stack = append(stack, n)
			}
			n = next
		}
	}
}

func shouldRemoveNode(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}

	if isSkipToContentElement(n) {
		return true
	}
	if isAlwaysDroppedElement(n) {
		return true
	}
	if isLikelyBoilerplateElement(n) {
		return true
	}

	var attrs [6]string
	var seen [6]bool
	for _, attr := range n.Attr {
		idx := relevantAttrIndex(attr.Key)
		if idx == -1 || seen[idx] {
			continue
		}
		seen[idx] = true
		attrs[idx] = attr.Val
	}

	for _, value := range attrs {
		if value == "" {
			continue
		}
		lowerValue := strings.ToLower(value)
		if strings.Contains(lowerValue, "notloaded") {
			return true
		}
		if strings.Contains(lowerValue, "display:none") {
			return true
		}
		if containsBoilerplateMarker(lowerValue) {
			return true
		}
	}

	return false
}

func isSkipToContentElement(n *html.Node) bool {
	if !isAnchorOrButtonElement(n) {
		return false
	}

	text := normalizedNodeText(n)
	if looksLikeSkipToContentText(text) {
		return true
	}

	href := ""
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, "href") {
			href = strings.ToLower(strings.TrimSpace(attr.Val))
			continue
		}
		if !isSkipMarkerAttr(attr.Key) {
			continue
		}
		if containsSkipToContentMarker(attr.Val) {
			return true
		}
	}

	if strings.HasPrefix(href, "#") && strings.Contains(text, "skip") && isLikelyMainContentTarget(href) {
		return true
	}

	return false
}

func isAnchorOrButtonElement(n *html.Node) bool {
	switch n.DataAtom {
	case atom.A, atom.Button:
		return true
	}

	tag := strings.ToLower(strings.TrimSpace(n.Data))
	return tag == "a" || tag == "button"
}

func normalizedNodeText(n *html.Node) string {
	if n == nil {
		return ""
	}

	parts := make([]string, 0, 4)
	stack := []*html.Node{n}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]

		if current.Type == html.TextNode {
			trimmed := strings.TrimSpace(current.Data)
			if trimmed != "" {
				parts = append(parts, trimmed)
			}
		}

		for child := current.LastChild; child != nil; child = child.PrevSibling {
			stack = append(stack, child)
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.ToLower(strings.Join(parts, " "))
}

func looksLikeSkipToContentText(text string) bool {
	if text == "" || !strings.Contains(text, "skip") {
		return false
	}

	normalized := strings.Join(strings.Fields(text), " ")
	if strings.Contains(normalized, "skip to content") ||
		strings.Contains(normalized, "skip to main") ||
		strings.Contains(normalized, "skip to navigation") ||
		strings.Contains(normalized, "skip navigation") ||
		strings.Contains(normalized, "skip nav") {
		return true
	}

	return false
}

func isSkipMarkerAttr(key string) bool {
	return strings.EqualFold(key, "id") ||
		strings.EqualFold(key, "class") ||
		strings.EqualFold(key, "aria-label") ||
		strings.EqualFold(key, "title") ||
		strings.EqualFold(key, "data-testid") ||
		strings.EqualFold(key, "data-component")
}

func containsSkipToContentMarker(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}

	normalized := strings.NewReplacer("_", "-", " ", "-").Replace(strings.ToLower(value))
	return strings.Contains(normalized, "skip-to-content") ||
		strings.Contains(normalized, "skip-content") ||
		strings.Contains(normalized, "skip-to-main") ||
		strings.Contains(normalized, "skip-main") ||
		strings.Contains(normalized, "skip-nav") ||
		strings.Contains(normalized, "skip-navigation") ||
		strings.Contains(normalized, "skip-link")
}

func isLikelyMainContentTarget(href string) bool {
	target := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(href), "#"))
	if target == "" {
		return false
	}

	return strings.Contains(target, "content") ||
		strings.Contains(target, "main") ||
		strings.Contains(target, "primary") ||
		strings.Contains(target, "article")
}

func isAlwaysDroppedElement(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Iframe, atom.Svg, atom.Canvas, atom.Dialog, atom.Embed, atom.Object, atom.Form, atom.Button:
		return true
	}

	if n.DataAtom != 0 {
		return false
	}

	tag := strings.ToLower(strings.TrimSpace(n.Data))
	if tag == "" {
		return false
	}

	return isAlwaysDroppedTag(tag)
}

func isLikelyBoilerplateElement(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Nav, atom.Header, atom.Footer, atom.Aside:
		return true
	}

	if n.DataAtom != 0 {
		return false
	}

	tag := strings.ToLower(strings.TrimSpace(n.Data))
	if tag == "" {
		return false
	}

	return isLikelyBoilerplateByTag(tag)
}

func isAlwaysDroppedTag(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "template", "iframe", "svg", "canvas", "dialog", "embed", "object", "form", "button":
		return true
	default:
		return false
	}
}

func isLikelyBoilerplateByTag(tag string) bool {
	switch tag {
	case "nav", "header", "footer", "aside":
		return true
	default:
		return false
	}
}

func relevantAttrIndex(key string) int {
	switch key {
	case "id":
		return 0
	case "class":
		return 1
	case "role":
		return 2
	case "aria-label":
		return 3
	case "data-testid":
		return 4
	case "data-component":
		return 5
	}

	switch {
	case strings.EqualFold(key, "id"):
		return 0
	case strings.EqualFold(key, "class"):
		return 1
	case strings.EqualFold(key, "role"):
		return 2
	case strings.EqualFold(key, "aria-label"):
		return 3
	case strings.EqualFold(key, "data-testid"):
		return 4
	case strings.EqualFold(key, "data-component"):
		return 5
	default:
		return -1
	}
}

func containsBoilerplateMarker(input string) bool {
	for _, marker := range trafilaturaBoilerplateMarkers {
		if strings.Contains(input, marker) {
			return true
		}
	}
	return false
}

func removeNode(n *html.Node) {
	if n == nil || n.Parent == nil {
		return
	}
	n.Parent.RemoveChild(n)
}

var trafilaturaBoilerplateMarkers = []string{
	"footer",
	"related",
	"viral",
	"share",
	"social",
	"sociable",
	"syndication",
	"newsletter",
	"subnav",
	"cookie",
	"tags",
	"tag-list",
	"sidebar",
	"banner",
	"menu",
	"navbar",
	"navbox",
	"breadcrumb",
	"bread-crumb",
	"author",
	"button",
	"byline",
	"rating",
	"widget",
	"attachment",
	"timestamp",
	"user-info",
	"user-profile",
	"article-infos",
	"nfoline",
	"mostpopularstories",
	"outbrain",
	"taboola",
	"criteo",
	"consent",
	"modal-content",
	"permission",
	"next-",
	"-stories",
	"most-popular",
	"message-container",
	"overlay",
	"paid-content",
	"paidcontent",
	"obfuscated",
	"blurred",
	"comments-title",
	"nocomments",
	"reply-",
	"akismet",
	"hide-",
	"-hide-",
	"hide-print",
	"hidden",
	"noprint",
	"navigation",
	"contentinfo",
	"search",
	"tablist",
}

func processCloudflareEmails(input []byte, mode EmailMode) []byte {
	if mode == EmailIgnore {
		return input
	}

	inputStr := string(input)

	matches := cfEmailRE.FindAllStringSubmatch(inputStr, -1)
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		hexString := match[1]
		var replacement string
		switch mode {
		case EmailStrip:
			replacement = ""
		case EmailDecode:
			decoded := decodeCloudflareEmail(hexString)
			replacement = decoded
		default:
			continue
		}
		if replacement == "" {
			inputStr = strings.Replace(inputStr, match[0], "", 1)
		} else {
			inputStr = strings.Replace(inputStr, match[0], replacement, 1)
		}
	}

	linkMatches := cfEmailLinkRE.FindAllStringSubmatch(inputStr, -1)
	for _, match := range linkMatches {
		if len(match) < 2 {
			continue
		}
		hexString := match[1]
		switch mode {
		case EmailStrip:
			inputStr = strings.Replace(inputStr, match[0], "#", 1)
		case EmailDecode:
			decoded := decodeCloudflareEmail(hexString)
			inputStr = strings.Replace(inputStr, match[0], decoded, 1)
		default:
			continue
		}
	}

	if mode == EmailStrip {
		inputStr = cfEmailClassRE.ReplaceAllString(inputStr, "")
		inputStr = cfEmailAttrRE.ReplaceAllString(inputStr, "")
	}

	return []byte(inputStr)
}

func decodeCloudflareEmail(hexString string) string {
	if len(hexString) < 4 {
		return ""
	}

	key, err := strconv.ParseInt(hexString[:2], 16, 64)
	if err != nil {
		return ""
	}

	var email strings.Builder
	email.Grow(len(hexString) / 2)

	for i := 2; i < len(hexString); i += 2 {
		charCode, err := strconv.ParseInt(hexString[i:i+2], 16, 64)
		if err != nil {
			continue
		}

		email.WriteByte(byte(charCode ^ key))
	}

	return email.String()
}
