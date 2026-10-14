package context

import (
	"strings"
	"testing"
	"velocitylabs.cc/sdk/provider"
)

func flush(history []Event, sections []ContextSection) ([]Event, string) {
	before := len(history)
	FlushInternalContext(&history, nil, provider.Capabilities{}, sections)
	if len(history) == before {
		return history, ""
	}
	return history, history[len(history)-1].Content.(string)
}

func TestInternalContextSendsOnlyChanges(t *testing.T) {
	date, files, tree := "Date: 1", "a.go", "src/"
	treeOK := true
	sections := []ContextSection{
		{Tag: "system", Diff: DiffLines, Build: func(string) (string, bool) { return date + "\nOS: linux", true }},
		{Tag: "files", Build: func(string) (string, bool) { return files, true }},
		{Tag: "tree", Build: func(string) (string, bool) { return tree, treeOK }},
	}

	history, first := flush(nil, sections)
	want := "<internal_context>\n<system>\nDate: 1\nOS: linux\n</system>\n\n<files>\na.go\n</files>\n\n<tree>\nsrc/\n</tree>\n</internal_context>"
	if first != want {
		t.Fatalf("first =\n%s", first)
	}
	if history, got := flush(history, sections); got != "" || len(history) != 1 {
		t.Fatalf("unchanged context appended %q", got)
	}

	date, treeOK = "Date: 2", false
	history, got := flush(history, sections)
	if got != "<internal_context update=\"true\">\n<system>\nDate: 2\n</system>\n</internal_context>" {
		t.Fatalf("line update =\n%s", got)
	}

	files = ""
	history, got = flush(history, sections)
	if got != "<internal_context update=\"true\">\n<files></files>\n</internal_context>" {
		t.Fatalf("removal =\n%s", got)
	}

	snapshot, _ := InternalContextSnapshot(history, sections)
	if snapshot != "<internal_context>\n<system>\nDate: 2\nOS: linux\n</system>\n\n<tree>\nsrc/\n</tree>\n</internal_context>" {
		t.Fatalf("snapshot =\n%s", snapshot)
	}
	if body, ok := InternalContextSection(snapshot, "tree"); !ok || body != "src/" {
		t.Fatalf("section = %q %v", body, ok)
	}
}

func TestInternalContextRemovedLineReplacesSection(t *testing.T) {
	body := "A: 1\nB: 2"
	sections := []ContextSection{{Tag: "system", Diff: DiffLines, Build: func(string) (string, bool) { return body, true }}}
	history, _ := flush(nil, sections)
	body = "A: 1"
	history, got := flush(history, sections)
	if !strings.Contains(got, "<system replace=\"true\">\nA: 1\n</system>") {
		t.Fatalf("got %s", got)
	}
	if snapshot, _ := InternalContextSnapshot(history, sections); strings.Contains(snapshot, "B: 2") {
		t.Fatalf("snapshot kept removed line: %s", snapshot)
	}
}

func TestInternalContextIgnoresFreeFormAndRestartsAfterBoundary(t *testing.T) {
	sections := []ContextSection{{Tag: "system", Build: func(string) (string, bool) { return "x", true }}}
	history := []Event{InternalEvent("<internal_context>\nA background task finished.\n</internal_context>")}
	history, got := flush(history, sections)
	if !strings.HasPrefix(got, "<internal_context>\n") {
		t.Fatalf("free-form message taken as snapshot: %q", got)
	}
	history = append(history, CompressionBoundaryEvent("summary"))
	if _, got = flush(history, sections); !strings.HasPrefix(got, "<internal_context>\n") {
		t.Fatalf("context not re-sent after boundary: %q", got)
	}
}
