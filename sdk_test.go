package sdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"velocitylabs.cc/sdk/agent"
	vcontext "velocitylabs.cc/sdk/context"
	"velocitylabs.cc/sdk/inference"
	"velocitylabs.cc/sdk/provider"
)

func TestInferenceDetectAndRouter(t *testing.T) {
	if got := inference.DetectType("", "https://openrouter.ai/api/v1"); got != inference.TypeOpenRouter {
		t.Fatalf("DetectType openrouter = %q", got)
	}
	if got := inference.DetectType("", "https://api.openai.com/v1/responses"); got != inference.TypeResponses {
		t.Fatalf("DetectType responses = %q", got)
	}
	if got := inference.DetectType("", "https://crof.ai"); got != inference.TypeCrof {
		t.Fatalf("DetectType crof = %q", got)
	}
	if got := inference.DetectType("", "https://api.x.ai/v1"); got != inference.TypeXAI {
		t.Fatalf("DetectType xai = %q", got)
	}

	router := inference.NewRouter(nil)
	if err := router.Set("local", inference.Config{APIURL: "http://localhost:8080/v1"}); err != nil {
		t.Fatalf("router.Set: %v", err)
	}
	client, err := router.Select("local")
	if err != nil || client == nil {
		t.Fatalf("router.Select: %v", err)
	}
}

func TestAgentRunnerPipelinedToolsAndAutoCompress(t *testing.T) {
	var turnCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := turnCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			// Compression summary call triggered by AutoCompress
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Compressed summary of prior work.\\n<files_to_retain>\\nmain.go:1-2\\n</files_to_retain>\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			// Tool call turn
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Thinking...\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"echo\",\"arguments\":\"{\\\"msg\\\":\\\"hi\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			// Final answer turn
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"All done!\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":42}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	defer srv.Close()

	client, err := inference.NewProvider(inference.Config{
		Type:   inference.TypeOpenAICompatible,
		APIURL: srv.URL,
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	var compressed bool
	var toolExecuted bool
	runner := agent.Runner{
		Provider: client,
		Model:    agent.ModelRef{Provider: "test", Model: "gpt-test"},
		AutoCompress: agent.AutoCompress{
			Enabled:       true,
			ContextWindow: 1000,
			Threshold:     220,
		},
		FileReader: func(_ context.Context, relPath string) ([]byte, error) {
			if relPath == "main.go" {
				return []byte("package main\nfunc main() {}\nline3\n"), nil
			}
			return nil, fmt.Errorf("not found")
		},
		Tools: agent.FuncToolExecutor{
			DefinitionsFn: func(_ context.Context) []provider.ToolDef {
				return []provider.ToolDef{{
					Type: "function",
					Function: provider.FunctionDef{
						Name:        "echo",
						Description: "Echoes message",
						Parameters:  map[string]any{"type": "object"},
					},
				}}
			},
			ExecuteFn: func(_ context.Context, call agent.ToolCall, onOutput func(agent.ToolOutput)) any {
				toolExecuted = true
				onOutput(agent.ToolOutput{Text: "echoed: " + call.Arguments, Done: true})
				return "echoed: " + call.Arguments
			},
		},
		Observer: agent.Observer{
			OnCompression: func(status agent.CompressionStatus) {
				if status.Done && status.Err == nil {
					compressed = true
				}
			},
		},
	}

	sess := agent.Session{
		ID:    "sess-1",
		Model: runner.Model,
		History: []agent.Event{
			agent.SystemEvent("You are a test assistant."),
			agent.UserEvent(strings.Repeat("Prior conversation text ", 10)),
			agent.AssistantEvent("Prior assistant reply.", "", nil, nil, 250),
			agent.UserEvent("Now run echo."),
		},
	}

	res, err := runner.Run(context.Background(), sess)
	if err != nil {
		t.Fatalf("runner.Run: %v", err)
	}
	if !compressed {
		t.Fatal("expected AutoCompress to trigger")
	}
	if !toolExecuted {
		t.Fatal("expected tool to execute")
	}
	if res.Reply != "All done!" {
		t.Fatalf("Reply = %q, want %q", res.Reply, "All done!")
	}
	boundaryIdx := vcontext.LastBoundaryIndex(res.History)
	if boundaryIdx < 0 {
		t.Fatal("expected compression boundary event in history")
	}
	boundary := res.History[boundaryIdx]
	if boundary.CompressionData == nil || len(boundary.CompressionData.RetainedFiles) != 1 {
		t.Fatalf("expected 1 retained file, got %#v", boundary.CompressionData)
	}
	if boundary.CompressionData.RetainedFiles[0].Path != "main.go" || boundary.CompressionData.RetainedFiles[0].Range != "1-2" {
		t.Fatalf("retained file = %#v", boundary.CompressionData.RetainedFiles[0])
	}

	// Verify RenderHistory after compression includes <compressed_context> and <active_files>
	rendered, _ := vcontext.RenderHistory(res.History, vcontext.ProviderCapabilities(client))
	renderedText := fmt.Sprint(rendered)
	if !strings.Contains(renderedText, "<compressed_context>") || !strings.Contains(renderedText, "package main") {
		t.Fatalf("rendered history missing compressed context or retained file: %s", renderedText)
	}
}
