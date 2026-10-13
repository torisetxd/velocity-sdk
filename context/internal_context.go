package context

import (
	"strings"

	"velocitylabs.cc/sdk/provider"
)

const (
	internalContextTag = "internal_context"
	projectTreeTag     = "project_tree"
)

func FlushInternalContext(history *[]Event, renderedHistory *[]provider.ChatMessage, caps provider.Capabilities, fn func(lastInternal string) string) {
	if fn == nil || history == nil {
		return
	}
	lastContent, hasSnapshot := InternalContextSnapshot(*history)
	if !hasSnapshot && LastBoundaryIndex(*history) < 0 {
		lastContent = LastInternalContent(*history)
	}
	content := strings.TrimSpace(fn(lastContent))
	if content == "" {
		return
	}
	if hasSnapshot {
		content = InternalContextDelta(lastContent, content)
		if content == "" {
			return
		}
	} else if lastContent == content {
		return
	}
	ev := InternalEvent(content)
	*history = append(*history, ev)
	if renderedHistory != nil {
		*renderedHistory = append(*renderedHistory, EventToProviderMessage(ev, caps))
	}
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

func InternalContextSnapshot(history []Event) (string, bool) {
	start := LastBoundaryIndex(history) + 1
	snapshot := ""
	for _, event := range history[start:] {
		if event.Kind != EventKindInternal {
			continue
		}
		content, ok := event.Content.(string)
		if !ok || !hasContextSection(content, "system") {
			continue
		}
		content = strings.TrimSpace(content)
		if isInternalContextUpdate(content) {
			if snapshot != "" {
				snapshot = mergeInternalContext(snapshot, content)
			}
			continue
		}
		snapshot = content
	}
	return snapshot, snapshot != ""
}

func InternalContextDelta(previous, current string) string {
	previousSystem, previousHasSystem := contextSection(previous, "system")
	currentSystem, currentHasSystem := contextSection(current, "system")
	if !previousHasSystem || !currentHasSystem {
		if previous == current {
			return ""
		}
		return current
	}

	sections := make([]string, 0, 3)
	if system, replace := changedSystemSection(previousSystem.body, currentSystem.body); system != "" {
		if replace {
			sections = append(sections, renderContextSection("system", " replace=\"true\"", system))
		} else {
			sections = append(sections, renderContextSection("system", "", system))
		}
	}

	previousSkills, previousHasSkills := contextSection(previous, "available_skills")
	currentSkills, currentHasSkills := contextSection(current, "available_skills")
	if previousHasSkills != currentHasSkills || (previousHasSkills && previousSkills.body != currentSkills.body) {
		sections = append(sections, contextSectionUpdate("available_skills", currentSkills, currentHasSkills))
	}

	if contextCWD(previous) != contextCWD(current) {
		previousTree, previousHasTree := contextSection(previous, projectTreeTag)
		currentTree, currentHasTree := contextSection(current, projectTreeTag)
		if previousHasTree != currentHasTree || (previousHasTree && previousTree.body != currentTree.body) {
			sections = append(sections, contextSectionUpdate(projectTreeTag, currentTree, currentHasTree))
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return "<" + internalContextTag + " update=\"true\">\n" + strings.Join(sections, "\n\n") + "\n</" + internalContextTag + ">"
}

func contextCWD(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Current Working Directory: ") {
			continue
		}
		val := strings.TrimPrefix(line, "Current Working Directory: ")
		if i := strings.Index(val, " ("); i >= 0 {
			val = val[:i]
		}
		return strings.TrimSpace(val)
	}
	return ""
}

func changedSystemSection(previous, current string) (string, bool) {
	previousLines := contextLines(previous)
	currentLines := contextLines(current)
	previousByField := make(map[string]string, len(previousLines))
	for _, line := range previousLines {
		previousByField[contextField(line)] = line
	}

	changed := make([]string, 0, len(currentLines))
	currentFields := make(map[string]struct{}, len(currentLines))
	for _, line := range currentLines {
		field := contextField(line)
		currentFields[field] = struct{}{}
		if previousByField[field] != line {
			changed = append(changed, line)
		}
	}
	for field := range previousByField {
		if _, ok := currentFields[field]; !ok {
			return strings.Join(currentLines, "\n"), true
		}
	}
	return strings.Join(changed, "\n"), false
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

func contextField(line string) string {
	if index := strings.Index(line, ":"); index >= 0 {
		return strings.TrimSpace(line[:index])
	}
	return line
}

type internalContextSection struct {
	raw  string
	body string
}

func contextSection(content, tag string) (internalContextSection, bool) {
	openStart := strings.Index(content, "<"+tag)
	if openStart < 0 {
		return internalContextSection{}, false
	}
	openEndOffset := strings.Index(content[openStart:], ">")
	if openEndOffset < 0 {
		return internalContextSection{}, false
	}
	openEnd := openStart + openEndOffset
	closeTag := "</" + tag + ">"
	closeStartOffset := strings.Index(content[openEnd+1:], closeTag)
	if closeStartOffset < 0 {
		return internalContextSection{}, false
	}
	closeStart := openEnd + 1 + closeStartOffset
	closeEnd := closeStart + len(closeTag)
	return internalContextSection{
		raw:  content[openStart:closeEnd],
		body: content[openEnd+1 : closeStart],
	}, true
}

func hasContextSection(content, tag string) bool {
	_, ok := contextSection(content, tag)
	return ok
}

func isInternalContextUpdate(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "<"+internalContextTag+" update=\"true\">")
}

func contextSectionUpdate(tag string, section internalContextSection, present bool) string {
	if present {
		return section.raw
	}
	return renderContextSection(tag, "", "")
}

func renderContextSection(tag, attributes, body string) string {
	if strings.TrimSpace(body) == "" {
		return "<" + tag + attributes + "></" + tag + ">"
	}
	return "<" + tag + attributes + ">\n" + strings.TrimSpace(body) + "\n</" + tag + ">"
}

func mergeInternalContext(snapshot, update string) string {
	if system, ok := contextSection(update, "system"); ok {
		if strings.Contains(system.raw, " replace=\"true\"") {
			snapshot = replaceContextSection(snapshot, "system", renderContextSection("system", "", system.body))
		} else if previous, ok := contextSection(snapshot, "system"); ok {
			snapshot = replaceContextSection(snapshot, "system", renderContextSection("system", "", mergeSystemLines(previous.body, system.body)))
		}
	}
	for _, tag := range []string{"available_skills", projectTreeTag} {
		section, ok := contextSection(update, tag)
		if !ok {
			continue
		}
		replacement := ""
		if strings.TrimSpace(section.body) != "" {
			replacement = section.raw
		}
		snapshot = replaceContextSection(snapshot, tag, replacement)
	}
	return snapshot
}

func mergeSystemLines(previous, update string) string {
	lines := contextLines(previous)
	positions := make(map[string]int, len(lines))
	for index, line := range lines {
		positions[contextField(line)] = index
	}
	for _, line := range contextLines(update) {
		field := contextField(line)
		if index, ok := positions[field]; ok {
			lines[index] = line
			continue
		}
		positions[field] = len(lines)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func replaceContextSection(content, tag, replacement string) string {
	section, ok := contextSection(content, tag)
	if ok {
		return strings.Replace(content, section.raw, replacement, 1)
	}
	if replacement == "" {
		return content
	}
	closingTag := "</" + internalContextTag + ">"
	return strings.Replace(content, closingTag, "\n"+replacement+"\n"+closingTag, 1)
}
