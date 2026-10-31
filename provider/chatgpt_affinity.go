package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"velocitylabs.cc/sdk/internal/debugtrace"
)

// cacheDebugEnv enables one debug line per request describing the affinity the
// subscription route was sent and what it echoed back.
const cacheDebugEnv = "VELOCITY_CACHE_DEBUG"

const (
	chatGPTSessionIDHeader     = "session-id"
	chatGPTSessionIDUnderscore = "session_id"
	chatGPTThreadIDHeader      = "thread-id"
	chatGPTClientRequestHeader = "x-client-request-id"
	chatGPTTurnStateHeader     = "x-codex-turn-state"
	chatGPTWindowIDHeader      = "x-codex-window-id"
	chatGPTTurnMetadataHeader  = "x-codex-turn-metadata"
	chatGPTParentThreadHeader  = "x-codex-parent-thread-id"
)

// chatGPTAffinityKey derives the affinity id of one logical prefix from the
// conversation key plus a digest over everything the cache matches on: model,
// leading instructions, the first tool schema set and the first input item.
// Callers resending the same prefix share the id; a different prefix
// (compaction, a title, another model) gets its own.
func chatGPTAffinityKey(conversationKey string, payload map[string]any) (string, error) {
	prefix := struct {
		Model        any   `json:"model"`
		Instructions []any `json:"instructions"`
		Tools        any   `json:"tools"`
		FirstInput   any   `json:"first_input"`
	}{Model: payload["model"], Tools: payload["tools"]}
	if input, ok := payload["input"].([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				break
			}
			role, _ := item["role"].(string)
			if role != "developer" && role != "system" {
				break
			}
			prefix.Instructions = append(prefix.Instructions, item)
		}
		if len(input) > 0 {
			prefix.FirstInput = input[0]
		}
	}
	encoded, err := json.Marshal(prefix)
	if err != nil {
		return "", fmt.Errorf("encode cache affinity prefix: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(conversationKey) + "\x00" + string(encoded)))
	id, err := uuid.FromBytes(sum[:16])
	if err != nil {
		return "", fmt.Errorf("encode cache affinity id: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String(), nil
}

// chatGPTRequestHeaders spells the affinity id in every spelling the route is
// known to read, plus the sticky-routing token of the current turn and any
// x-codex-* values the caller supplied.
func chatGPTRequestHeaders(req ChatRequest, affinity, turnState string) map[string]string {
	if affinity == "" {
		return nil
	}
	headers := map[string]string{
		chatGPTSessionIDHeader:     affinity,
		chatGPTSessionIDUnderscore: affinity,
		chatGPTThreadIDHeader:      affinity,
		chatGPTClientRequestHeader: affinity,
	}
	if turnState != "" {
		headers[chatGPTTurnStateHeader] = turnState
	}
	optional := map[string]string{
		chatGPTWindowIDHeader:     req.CodexWindowID,
		chatGPTTurnMetadataHeader: req.CodexTurnMetadata,
		chatGPTParentThreadHeader: req.CodexParentThreadID,
	}
	for name, value := range optional {
		if value = strings.TrimSpace(value); value != "" {
			headers[name] = value
		}
	}
	return headers
}

// chatGPTTurnCache holds the sticky-routing token the backend returns inside a
// turn. It lives in memory only and is dropped when the turn id or the bearer
// token changes, so it is never replayed across turns or sessions.
type chatGPTTurnCache struct {
	mu          sync.Mutex
	turnID      string
	tokenDigest string
	state       string
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

func (c *chatGPTTurnCache) get(turnID, token string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	digest := tokenDigest(token)
	if turnID == "" || c.turnID != turnID || c.tokenDigest != digest {
		c.turnID, c.tokenDigest, c.state = turnID, digest, ""
		return ""
	}
	return c.state
}

func (c *chatGPTTurnCache) put(turnID, token, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if turnID == "" || state == "" {
		return
	}
	if c.turnID != turnID || c.tokenDigest != tokenDigest(token) {
		return
	}
	c.state = state
}

// chatGPTRequestTrace collects what one request sent and what the response
// reported, so a single line can say whether the affinity stuck.
type chatGPTRequestTrace struct {
	sentSessionID   string
	sentCacheKey    string
	turnStateSent   bool
	turnStateLen    int
	echoedCacheKey  string
	turnStateSeen   bool
	turnStateNewLen int
	cachedTokens    int
	inputTokens     int
}

func (t chatGPTRequestTrace) log() {
	fields := map[string]any{
		"sent_session_id":          t.sentSessionID,
		"sent_prompt_cache_key":    t.sentCacheKey,
		"echoed_prompt_cache_key":  t.echoedCacheKey,
		"turn_state_present":       t.turnStateSeen,
		"turn_state_len":           t.turnStateNewLen,
		"turn_state_replayed":      t.turnStateSent,
		"turn_state_replayed_len":  t.turnStateLen,
		"cached_tokens":            t.cachedTokens,
		"input_tokens":             t.inputTokens,
		"prompt_cache_key_matched": t.echoedCacheKey != "" && t.echoedCacheKey == t.sentCacheKey,
	}
	debugtrace.Log("provider.cache_affinity", fields)
	if !cacheDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr,
		"cache-affinity sent_session_id=%s sent_prompt_cache_key=%s echoed_prompt_cache_key=%s turn_state_present=%t turn_state_len=%d turn_state_replayed=%t cached_tokens=%d input_tokens=%d\n",
		t.sentSessionID, t.sentCacheKey, t.echoedCacheKey, t.turnStateSeen, t.turnStateNewLen, t.turnStateSent, t.cachedTokens, t.inputTokens)
}

func cacheDebugEnabled() bool {
	value := strings.TrimSpace(os.Getenv(cacheDebugEnv))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// chatGPTCacheHeaders lists the response headers that can carry cache or turn
// state, so instrumentation can show what the route returns.
func chatGPTCacheHeaders(header http.Header) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "cache") || strings.Contains(lower, "turn") ||
			strings.Contains(lower, "session") || strings.Contains(lower, "thread") ||
			strings.Contains(lower, "codex") || strings.Contains(lower, "affinity") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func logChatGPTResponseCacheHeaders(streamID string, header http.Header) {
	for _, name := range chatGPTCacheHeaders(header) {
		debugtrace.Log("provider.cache_response_header", map[string]any{
			"protocol":  "responses",
			"stream_id": streamID,
			"name":      strings.ToLower(name),
			"value":     header.Get(name),
		})
		if cacheDebugEnabled() {
			fmt.Fprintf(os.Stderr, "cache-affinity response_header %s=%s\n", strings.ToLower(name), header.Get(name))
		}
	}
}
