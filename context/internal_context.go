package context

import (
	"strings"

	"velocitylabs.cc/sdk/provider"
)

const internalContextTag = "internal_context"

// DiffMode controls how a changed ContextSection is re-sent to the model.
type DiffMode int

const (
	// DiffWhole re-sends the whole section whenever its body changes.
	DiffWhole DiffMode = iota
	// DiffLines treats the body as "Key: value" lines and re-sends only the
	// lines that changed. Removing a key re-sends the whole section.
	DiffLines
)

// ContextSection is one tagged block of internal context. The runner calls
// every section's Build before each model step, diffs the result against what
// the model has already seen, and appends only the changes to the history, so
// volatile context never invalidates the prompt-cache prefix.
type ContextSection struct {
	Tag  string
	Diff DiffMode
	// Build returns the section's current body. last is the full internal
	// context the model has seen so far ("" when none). Returning ok=false
	// leaves the section as it was last sent, or omits it if it never was. An
	// empty body removes the section.
	Build func(last string) (body string, ok bool)
}

// InternalContextPrompt explains internal context messages to the model, for
// inclusion in the system prompt.
func InternalContextPrompt(sections []ContextSection) string {
	var lineTags []string
	for _, section := range sections {
		if section.Diff == DiffLines {
			lineTags = append(lineTags, "<"+section.Tag+">")
		}
	}
	prompt := "Messages wrapped in <" + internalContextTag + "> tags are injected automatically by the system, not sent by the user. Treat them as part of the system prompt and never mention them explicitly. Messages with <" + internalContextTag + ` update="true"> are deltas from the most recent full internal context: an included section replaces its prior version in full, omitted sections retain their prior values, and an empty section clears its prior value.`
	if len(lineTags) > 0 {
		prompt += " Exception: in an update, " + strings.Join(lineTags, ", ") + ` list only changed "Key: value" fields unless marked replace="true", which replaces the full section.`
	}
	return prompt
}

type contextBlock struct {
	tag   string
	attrs string
	body  string
}

// FlushInternalContext builds sections and appends whatever the model has not
// seen yet: the full context on first use (or after a compression boundary),
// otherwise an update holding only the sections that changed.
func FlushInternalContext(history *[]Event, renderedHistory *[]provider.ChatMessage, caps provider.Capabilities, sections []ContextSection) {
	if len(sections) == 0 || history == nil {
		return
	}
	last, _ := InternalContextSnapshot(*history, sections)
	content := InternalContextUpdate(last, sections)
	if content == "" {
		return
	}
	ev := InternalEvent(content)
	*history = append(*history, ev)
	if renderedHistory != nil {
		*renderedHistory = append(*renderedHistory, EventToProviderMessage(ev, caps))
	}
}

// InternalContextUpdate builds sections against last (a snapshot as returned
// by InternalContextSnapshot) and returns the internal context message to
// append, or "" when nothing changed.
func InternalContextUpdate(last string, sections []ContextSection) string {
	previous, hasPrevious := parseInternalContext(last)
	var out []contextBlock
	for _, section := range sections {
		if section.Build == nil || section.Tag == "" {
			continue
		}
		body, ok := section.Build(last)
		if !ok {
			continue
		}
		body = strings.TrimSpace(body)
		prev, had := findBlock(previous.blocks, section.Tag)
		if !hasPrevious {
			if body != "" {
				out = append(out, contextBlock{tag: section.Tag, body: body})
			}
			continue
		}
		switch {
		case body == "":
			if had {
				out = append(out, contextBlock{tag: section.Tag})
			}
		case !had:
			out = append(out, contextBlock{tag: section.Tag, body: body})
		case section.Diff == DiffLines:
			if changed, replace := changedLines(prev.body, body); changed != "" {
				block := contextBlock{tag: section.Tag, body: changed}
				if replace {
					block.attrs = ` replace="true"`
				}
				out = append(out, block)
			}
		case prev.body != body:
			out = append(out, contextBlock{tag: section.Tag, body: body})
		}
	}
	if len(out) == 0 {
		return ""
	}
	return renderInternalContext(out, hasPrevious)
}

// InternalContextSnapshot returns everything the model has been told through
// internal context since the last compression boundary, with updates merged
// in, rendered as one full internal context message.
func InternalContextSnapshot(history []Event, sections []ContextSection) (string, bool) {
	var snapshot []contextBlock
	has := false
	for _, event := range history[LastBoundaryIndex(history)+1:] {
		if event.Kind != EventKindInternal {
			continue
		}
		content, ok := event.Content.(string)
		if !ok {
			continue
		}
		parsed, ok := parseInternalContext(content)
		if !ok || !anyRegistered(parsed.blocks, sections) {
			continue
		}
		if !parsed.update {
			snapshot, has = parsed.blocks, true
			continue
		}
		if has {
			snapshot = mergeBlocks(snapshot, parsed.blocks, sections)
		}
	}
	if !has {
		return "", false
	}
	return renderInternalContext(snapshot, false), true
}

// InternalContextSection returns the body of tag inside an internal context
// message, such as the snapshot handed to ContextSection.Build.
func InternalContextSection(content, tag string) (string, bool) {
	parsed, ok := parseInternalContext(content)
	if !ok {
		return "", false
	}
	block, ok := findBlock(parsed.blocks, tag)
	return block.body, ok
}

func LastInternalContent(history []Event) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Kind != EventKindInternal {
			continue
		}
		if content, ok := history[i].Content.(string); ok {
			return strings.TrimSpace(content)
		}
	}
	return ""
}

type parsedContext struct {
	blocks []contextBlock
	update bool
}

// parseInternalContext reads a message made only of top-level tagged blocks
// inside <internal_context>. Free-form internal messages fail to parse, so
// they are never mistaken for snapshots.
func parseInternalContext(content string) (parsedContext, bool) {
	s := strings.TrimSpace(content)
	var parsed parsedContext
	switch {
	case strings.HasPrefix(s, "<"+internalContextTag+` update="true">`):
		parsed.update = true
		s = s[len("<"+internalContextTag+` update="true">`):]
	case strings.HasPrefix(s, "<"+internalContextTag+">"):
		s = s[len("<"+internalContextTag+">"):]
	default:
		return parsedContext{}, false
	}
	closing := "</" + internalContextTag + ">"
	if !strings.HasSuffix(s, closing) {
		return parsedContext{}, false
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, closing))
	for s != "" {
		if s[0] != '<' {
			return parsedContext{}, false
		}
		end := strings.IndexByte(s, '>')
		if end < 0 {
			return parsedContext{}, false
		}
		tag, attrs, _ := strings.Cut(s[1:end], " ")
		if tag == "" || strings.HasPrefix(tag, "/") {
			return parsedContext{}, false
		}
		rest := s[end+1:]
		closeTag := "</" + tag + ">"
		i := strings.Index(rest, closeTag)
		if i < 0 {
			return parsedContext{}, false
		}
		block := contextBlock{tag: tag, body: strings.TrimSpace(rest[:i])}
		if attrs != "" {
			block.attrs = " " + attrs
		}
		parsed.blocks = append(parsed.blocks, block)
		s = strings.TrimSpace(rest[i+len(closeTag):])
	}
	return parsed, len(parsed.blocks) > 0
}

func renderInternalContext(blocks []contextBlock, update bool) string {
	open := "<" + internalContextTag + ">"
	if update {
		open = "<" + internalContextTag + ` update="true">`
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.body == "" {
			parts = append(parts, "<"+block.tag+block.attrs+"></"+block.tag+">")
			continue
		}
		parts = append(parts, "<"+block.tag+block.attrs+">\n"+block.body+"\n</"+block.tag+">")
	}
	return open + "\n" + strings.Join(parts, "\n\n") + "\n</" + internalContextTag + ">"
}

func mergeBlocks(snapshot, update []contextBlock, sections []ContextSection) []contextBlock {
	snapshot = append([]contextBlock(nil), snapshot...)
	for _, block := range update {
		index := -1
		for i := range snapshot {
			if snapshot[i].tag == block.tag {
				index = i
				break
			}
		}
		if block.body == "" {
			if index >= 0 {
				snapshot = append(snapshot[:index], snapshot[index+1:]...)
			}
			continue
		}
		body := block.body
		if index >= 0 && diffMode(sections, block.tag) == DiffLines && !strings.Contains(block.attrs, `replace="true"`) {
			body = mergeLines(snapshot[index].body, block.body)
		}
		if index >= 0 {
			snapshot[index] = contextBlock{tag: block.tag, body: body}
		} else {
			snapshot = append(snapshot, contextBlock{tag: block.tag, body: body})
		}
	}
	return snapshot
}

func findBlock(blocks []contextBlock, tag string) (contextBlock, bool) {
	for _, block := range blocks {
		if block.tag == tag {
			return block, true
		}
	}
	return contextBlock{}, false
}

func anyRegistered(blocks []contextBlock, sections []ContextSection) bool {
	for _, block := range blocks {
		for _, section := range sections {
			if section.Tag == block.tag {
				return true
			}
		}
	}
	return false
}

func diffMode(sections []ContextSection, tag string) DiffMode {
	for _, section := range sections {
		if section.Tag == tag {
			return section.Diff
		}
	}
	return DiffWhole
}

// changedLines returns the lines of current whose key differs from previous.
// When a key was removed it returns all of current and replace=true.
func changedLines(previous, current string) (string, bool) {
	previousLines := contextLines(previous)
	currentLines := contextLines(current)
	previousByKey := make(map[string]string, len(previousLines))
	for _, line := range previousLines {
		previousByKey[lineKey(line)] = line
	}
	changed := make([]string, 0, len(currentLines))
	currentKeys := make(map[string]struct{}, len(currentLines))
	for _, line := range currentLines {
		key := lineKey(line)
		currentKeys[key] = struct{}{}
		if previousByKey[key] != line {
			changed = append(changed, line)
		}
	}
	for key := range previousByKey {
		if _, ok := currentKeys[key]; !ok {
			return strings.Join(currentLines, "\n"), true
		}
	}
	return strings.Join(changed, "\n"), false
}

func mergeLines(previous, update string) string {
	lines := contextLines(previous)
	positions := make(map[string]int, len(lines))
	for index, line := range lines {
		positions[lineKey(line)] = index
	}
	for _, line := range contextLines(update) {
		key := lineKey(line)
		if index, ok := positions[key]; ok {
			lines[index] = line
			continue
		}
		positions[key] = len(lines)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func contextLines(content string) []string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func lineKey(line string) string {
	if index := strings.Index(line, ":"); index >= 0 {
		return strings.TrimSpace(line[:index])
	}
	return line
}
