package provider

import "testing"

func TestParseModelListJSONContextLength(t *testing.T) {
	raw := []byte(`{"data":[
		{"id":"a","context_length":128000},
		{"id":"b","max_model_len":32768},
		{"id":"c","top_provider":{"context_length":1000000}},
		{"id":"d"}]}`)
	got := parseModelListJSON(raw)
	want := map[string]int{"a": 128000, "b": 32768, "c": 1000000, "d": 0}
	for _, m := range got {
		if m.ContextLength != want[m.ID] {
			t.Errorf("%s: context length %d, want %d", m.ID, m.ContextLength, want[m.ID])
		}
	}
}

func TestTokenUsageCost(t *testing.T) {
	u := tokenUsagePayload{PromptTokens: 10, CompletionTokens: 5, Cost: 0.0042}
	n := u.normalized()
	if n == nil || n.Cost != 0.0042 {
		t.Fatalf("usage %+v", n)
	}
	meta := TokenUsageMetadata(n)[UsageMetadataKey].(map[string]any)
	if meta["cost"] != 0.0042 {
		t.Fatalf("meta %+v", meta)
	}
}
