package provider

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The Claude Agent provider keeps a Claude Code transcript on disk so a run can
// resume the earlier turns of a conversation, including the thinking blocks and
// their signatures. The file is a derived cache: it is seeded from the host's
// history whenever it does not match, and losing it only costs fidelity.
const claudeTranscriptMetaSuffix = ".meta.json"

type claudeTranscriptKey struct{}

// WithClaudeTranscript tells a provider that runs an agent harness where to keep
// the conversation transcript for this run. The host owns the path and its
// lifetime; the provider owns the file's contents and its validity.
func WithClaudeTranscript(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, claudeTranscriptKey{}, strings.TrimSpace(path))
}

func claudeTranscriptFromContext(ctx context.Context) string {
	path, _ := ctx.Value(claudeTranscriptKey{}).(string)
	return strings.TrimSpace(path)
}

type claudeTranscriptMeta struct {
	SessionID   string `json:"sessionId"`
	HistoryHash string `json:"historyHash"`
}

type claudeTranscriptRecord struct {
	ParentUUID  *string         `json:"parentUuid"`
	IsSidechain bool            `json:"isSidechain"`
	Type        string          `json:"type"`
	Message     json.RawMessage `json:"message"`
	UUID        string          `json:"uuid"`
	Timestamp   string          `json:"timestamp"`
	UserType    string          `json:"userType"`
	Cwd         string          `json:"cwd"`
	SessionID   string          `json:"sessionId"`
	Version     string          `json:"version"`
}

// claudeTranscript owns one transcript file for the lifetime of a run.
type claudeTranscript struct {
	path     string
	metaPath string
	records  []claudeTranscriptRecord
	uuids    map[string]bool
	lastUUID string
	session  string
	cwd      string
	version  string
	resumed  bool
	dirty    bool
}

// openClaudeTranscript reads any existing transcript, drops duplicate records
// (a Claude Code run may append its own copy), and decides whether it can be
// resumed for the given prior conversation.
func openClaudeTranscript(path string, prior []ChatMessage) (*claudeTranscript, error) {
	transcript := &claudeTranscript{
		path:     path,
		metaPath: path + claudeTranscriptMetaSuffix,
		uuids:    make(map[string]bool),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("claude agent: %w", err)
	}

	records, err := readClaudeTranscript(path)
	if err != nil {
		return nil, err
	}
	deduped := make([]claudeTranscriptRecord, 0, len(records))
	for _, record := range records {
		if record.UUID != "" {
			if transcript.uuids[record.UUID] {
				transcript.dirty = true
				continue
			}
			transcript.uuids[record.UUID] = true
		}
		deduped = append(deduped, record)
	}
	transcript.records = deduped
	if len(deduped) > 0 {
		if last := deduped[len(deduped)-1]; last.UUID != "" {
			transcript.lastUUID = last.UUID
		}
		if session := strings.TrimSpace(deduped[len(deduped)-1].SessionID); session != "" {
			transcript.session = session
		}
		if cwd := strings.TrimSpace(deduped[len(deduped)-1].Cwd); cwd != "" {
			transcript.cwd = cwd
		}
	}

	// A turn that failed after recording its prompt leaves a trailing user record
	// with no reply behind it; drop it so the history still lines up.
	for len(deduped) > 0 {
		last := deduped[len(deduped)-1]
		if last.Type != "user" || claudeMessageHasToolResult(last.Message) {
			break
		}
		deduped = deduped[:len(deduped)-1]
		transcript.dirty = true
	}
	transcript.records = deduped
	if len(deduped) > 0 {
		transcript.lastUUID = deduped[len(deduped)-1].UUID
	}

	if len(deduped) == 0 {
		return transcript, nil
	}

	meta, err := readClaudeTranscriptMeta(transcript.metaPath)
	if err != nil {
		return nil, err
	}
	if meta.SessionID != "" {
		transcript.session = meta.SessionID
	}
	transcript.resumed = meta.HistoryHash != "" &&
		meta.HistoryHash == claudeMessagesDigest(prior) &&
		transcript.session != ""
	return transcript, nil
}

func readClaudeTranscript(path string) ([]claudeTranscriptRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude agent: read transcript: %w", err)
	}
	defer file.Close()

	records := make([]claudeTranscriptRecord, 0, 64)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var record claudeTranscriptRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Type != "user" && record.Type != "assistant" {
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("claude agent: read transcript: %w", err)
	}
	return records, nil
}

func readClaudeTranscriptMeta(path string) (claudeTranscriptMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return claudeTranscriptMeta{}, nil
		}
		return claudeTranscriptMeta{}, fmt.Errorf("claude agent: read transcript meta: %w", err)
	}
	var meta claudeTranscriptMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return claudeTranscriptMeta{}, nil
	}
	return meta, nil
}

// appendPrompt records the user turn being answered by this run.
func (t *claudeTranscript) appendPrompt(prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}
	content, err := json.Marshal([]map[string]any{{"type": "text", "text": prompt}})
	if err != nil {
		return fmt.Errorf("claude agent: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"role": "user", "content": json.RawMessage(content)})
	if err != nil {
		return fmt.Errorf("claude agent: %w", err)
	}
	line, err := t.append("user", payload, "", "")
	if err != nil {
		return err
	}
	return t.write(line)
}

// seed rewrites the transcript from the host's history, keeping only the text of
// each turn: nothing captured earlier survives, so neither do tool pairs.
func (t *claudeTranscript) seed(prior []ChatMessage) error {
	t.session = uuid.NewString()
	t.records = nil
	t.uuids = map[string]bool{}
	t.lastUUID = ""
	t.dirty = true
	t.resumed = false
	for _, message := range prior {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(claudeAgentContent(message.Content))
		if text == "" {
			continue
		}
		content, err := json.Marshal([]map[string]any{{"type": "text", "text": text}})
		if err != nil {
			return fmt.Errorf("claude agent: %w", err)
		}
		payload, err := json.Marshal(map[string]any{"role": role, "content": json.RawMessage(content)})
		if err != nil {
			return fmt.Errorf("claude agent: %w", err)
		}
		if _, err := t.append(role, payload, "", ""); err != nil {
			return err
		}
	}
	return t.flush()
}

// append records one stream message, filling in the fields Claude Code expects
// but does not send on the stream. A uuid from the stream is kept so a run that
// also appends its own copy stays deduplicable.
func (t *claudeTranscript) append(kind string, message json.RawMessage, id, timestamp string) ([]byte, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = uuid.NewString()
	}
	timestamp = strings.TrimSpace(timestamp)
	if timestamp == "" {
		timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	record := claudeTranscriptRecord{
		IsSidechain: false,
		Type:        kind,
		Message:     message,
		UUID:        id,
		Timestamp:   timestamp,
		UserType:    "external",
		Cwd:         t.cwd,
		SessionID:   t.session,
		Version:     t.version,
	}
	if t.lastUUID != "" {
		parent := t.lastUUID
		record.ParentUUID = &parent
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("claude agent: encode transcript record: %w", err)
	}
	t.records = append(t.records, record)
	t.uuids[record.UUID] = true
	t.lastUUID = record.UUID
	t.dirty = true
	return encoded, nil
}

// write appends a record that was already encoded by append.
func (t *claudeTranscript) write(line []byte) error {
	file, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("claude agent: write transcript: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("claude agent: write transcript: %w", err)
	}
	return nil
}

func (t *claudeTranscript) seen(id string) bool {
	return id != "" && t.uuids[id]
}

// flush rewrites the transcript from the records it holds and records the
// history fingerprint the next run will resume against.
func (t *claudeTranscript) flush() error {
	if t.dirty {
		var builder strings.Builder
		for _, record := range t.records {
			encoded, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("claude agent: encode transcript: %w", err)
			}
			builder.Write(encoded)
			builder.WriteByte('\n')
		}
		if err := os.WriteFile(t.path, []byte(builder.String()), 0o600); err != nil {
			return fmt.Errorf("claude agent: write transcript: %w", err)
		}
		t.dirty = false
	}
	return t.writeMeta()
}

func (t *claudeTranscript) writeMeta() error {
	if t.session == "" {
		return nil
	}
	meta := claudeTranscriptMeta{
		SessionID:   t.session,
		HistoryHash: claudeRecordsDigest(t.records),
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("claude agent: %w", err)
	}
	if err := os.WriteFile(t.metaPath, encoded, 0o600); err != nil {
		return fmt.Errorf("claude agent: write transcript meta: %w", err)
	}
	return nil
}

// claudeMessagesDigest fingerprints the conversation the host replayed, so a
// transcript is only resumed when it still describes the same history.
func claudeMessagesDigest(messages []ChatMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		text := claudeDigestText(claudeAgentContent(message.Content))
		if text == "" {
			continue
		}
		parts = append(parts, role+":"+text)
	}
	return claudeDigest(parts)
}

func claudeRecordsDigest(records []claudeTranscriptRecord) string {
	parts := make([]string, 0, len(records))
	for _, record := range records {
		if record.Type != "user" && record.Type != "assistant" {
			continue
		}
		var message struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(record.Message, &message); err != nil {
			continue
		}
		text := claudeTranscriptText(message.Content)
		if text == "" {
			continue
		}
		parts = append(parts, record.Type+":"+text)
	}
	return claudeDigest(parts)
}

// claudeTranscriptText keeps only the readable text of a recorded message:
// thinking blocks carry no text for comparison and tool blocks never do.
func claudeTranscriptText(raw json.RawMessage) string {
	content := decodeClaudeContent(raw)
	blocks, ok := content.([]any)
	if !ok {
		return claudeDigestText(claudeAgentContent(content))
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		fields, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := fields["type"].(string); kind != "text" && kind != "input_text" && kind != "output_text" {
			continue
		}
		text, _ := fields["text"].(string)
		if trimmed := claudeDigestText(text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return claudeDigestText(strings.Join(parts, " "))
}

func decodeClaudeContent(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return nil
}

// claudeMessageHasToolResult reports whether a user message carries tool output
// rather than a typed prompt, which decides whether it is recorded.
func claudeMessageHasToolResult(message json.RawMessage) bool {
	var payload struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(message, &payload); err != nil {
		return false
	}
	blocks, ok := decodeClaudeContent(payload.Content).([]any)
	if !ok {
		return false
	}
	for _, block := range blocks {
		fields, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := fields["type"].(string); kind == "tool_result" {
			return true
		}
	}
	return false
}

func claudeDigestText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func claudeDigest(parts []string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}
