package webfetch

import "strings"

// Agent is a User-Agent profile a fetch can present itself as. Many sites
// serve crawlers the full server-rendered page and skip the JavaScript or
// CAPTCHA challenge they show to unknown clients.
type Agent struct {
	Name      string
	UserAgent string
	Headers   map[string]string
}

// Built-in profiles. Googlebot and friends are real crawler identities, so a
// site that verifies crawlers by reverse DNS will still reject them.
var (
	AgentGooglebot = Agent{
		Name:      "googlebot",
		UserAgent: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		Headers:   map[string]string{"From": "googlebot(at)googlebot.com"},
	}
	AgentBingbot = Agent{
		Name:      "bingbot",
		UserAgent: "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
	}
	AgentGPTBot = Agent{
		Name:      "gptbot",
		UserAgent: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.2; +https://openai.com/gptbot",
	}
	AgentChatGPTUser = Agent{
		Name:      "chatgpt-user",
		UserAgent: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot",
	}
	AgentClaudeBot = Agent{
		Name:      "claudebot",
		UserAgent: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
	}
	AgentPerplexityBot = Agent{
		Name:      "perplexitybot",
		UserAgent: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)",
	}
	AgentFacebookBot = Agent{
		Name:      "facebookbot",
		UserAgent: "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
	}
	AgentTwitterBot = Agent{
		Name:      "twitterbot",
		UserAgent: "Twitterbot/1.0",
	}
	AgentBrowser = Agent{
		Name:      "browser",
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		Headers:   map[string]string{"Accept-Language": "en-US,en;q=0.9"},
	}
)

// DefaultSpoofAgents is the order crawler identities are tried in once a
// fetch looks blocked: search engines first, then AI crawlers, then a plain
// browser.
func DefaultSpoofAgents() []Agent {
	return []Agent{AgentGooglebot, AgentBingbot, AgentGPTBot, AgentChatGPTUser, AgentClaudeBot, AgentPerplexityBot, AgentFacebookBot, AgentBrowser}
}

// AgentByName resolves a profile by its Name, case-insensitively.
func AgentByName(name string) (Agent, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, a := range DefaultSpoofAgents() {
		if a.Name == name {
			return a, true
		}
	}
	if name == "twitterbot" {
		return AgentTwitterBot, true
	}
	return Agent{}, false
}

// blocked reports whether a response looks like a bot wall rather than the
// requested page: a denial status, or a challenge/CAPTCHA/JS-required stub.
func blocked(status int, body string) bool {
	switch status {
	case 401, 403, 406, 429, 451, 503:
		return true
	}
	if status < 200 || status >= 300 {
		return false
	}
	sample := body
	if len(sample) > 16*1024 {
		sample = sample[:16*1024]
	}
	lower := strings.ToLower(sample)
	for _, marker := range challengeMarkers {
		if strings.Contains(lower, marker) {
			// A real article that merely mentions a CAPTCHA is long; walls are short.
			return len(body) < 24*1024
		}
	}
	return false
}

var challengeMarkers = []string{
	"just a moment...",
	"checking your browser",
	"cf-browser-verification",
	"cf-challenge",
	"challenge-platform",
	"attention required! | cloudflare",
	"enable javascript and cookies to continue",
	"please enable javascript",
	"requires javascript",
	"you need to enable javascript",
	"verify you are human",
	"are you a robot",
	"g-recaptcha",
	"h-captcha",
	"cf-turnstile",
	"px-captcha",
	"datadome",
	"access denied",
	"unusual traffic from your computer",
}
