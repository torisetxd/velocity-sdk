package context

import (
	"reflect"
	"time"

	"velocitylabs.cc/sdk/provider"
)

func LastRequestConfiguration(history []Event, model ModelRef) *provider.RequestConfiguration {
	for i := len(history) - 1; i >= 0; i-- {
		ev := history[i]
		if ev.Kind == EventKindConfiguration && ev.Model != nil && *ev.Model == model {
			return provider.CloneRequestConfiguration(ev.Configuration)
		}
	}
	return nil
}

func RecordRequestConfiguration(history []Event, model ModelRef, extra map[string]any, tools []provider.ToolDef) []Event {
	config := provider.CloneRequestConfiguration(&provider.RequestConfiguration{Model: model.Model, ExtraBody: extra, Tools: tools})
	last := LastRequestConfiguration(history, model)
	if reflect.DeepEqual(last, config) {
		return history
	}
	ev := Event{Kind: EventKindConfiguration, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Model: &model, Configuration: config}
	head, tail, ok := SplitUnansweredTail(history)
	if !ok {
		head, tail = nil, history
		for i, event := range history {
			if event.Kind == EventKindUser {
				head, tail = history[:i], history[i:]
				break
			}
		}
	}
	out := append(CloneEvents(head), ev)
	return append(out, CloneEvents(tail)...)
}

func configurationSnapshot(history []Event) []Event {
	latest := make(map[ModelRef]int)
	for i, ev := range history {
		if ev.Kind == EventKindConfiguration && ev.Model != nil {
			latest[*ev.Model] = i
		}
	}
	var out []Event
	for i, ev := range history {
		if ev.Kind == EventKindConfiguration && ev.Model != nil && latest[*ev.Model] == i {
			out = append(out, CloneEvents(history[i : i+1])[0])
		}
	}
	return out
}

func ConfigurationHistory(history []Event, model ModelRef) []Event {
	out := make([]Event, 0, len(history))
	for _, ev := range ActiveHistory(history) {
		if ev.Kind == EventKindConfiguration && (ev.Model == nil || *ev.Model != model) {
			continue
		}
		out = append(out, ev)
	}
	return out
}
