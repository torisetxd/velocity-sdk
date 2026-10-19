# Velocity SDK (`velocitylabs.cc/sdk`)

Standalone Go SDK for multi-provider LLM streaming, context management, and agentic tool execution.

## Packages

- **`velocitylabs.cc/sdk/provider`** — Streaming LLM providers (`openai-compatible`, `openrouter`, `responses`, `copilot`, `copilot-responses`, `chatgpt`, `xai`, `crof`, `zen`, `cline`), capability reporting, reasoning & `reasoning_details` persistence, PNG CRC repair, image compression, model discovery, non-SSE JSON completion fallback, and speech-to-text `Transcriber`s (OpenAI-style multipart `/audio/transcriptions` with optional SSE deltas, OpenRouter's JSON transcription endpoint, and chat-model audio input via `input_audio` with a customizable prompt). Credentials/tokens are injected by the host application (`APIKey` or `TokenSource`).
- **`velocitylabs.cc/sdk/inference`** — Provider `Registry`, `Router`, `Config`, automatic provider type detection, and `NewTranscriber` (`openai`, `openrouter`, `chat`).
- **`velocitylabs.cc/sdk/context`** — Append-only `Event` log, capability-aware `RenderHistory`, token ceiling `ModelLimit`, registrable `ContextSection` internal context (only changed sections/lines are appended, so volatile context never breaks the prompt cache), and and prompt-cache-preserving `CompressConversation`, with optional `<files_to_retain>` file carry-over enabled by supplying a `FileReader` (`DirFileReader` for local disk).
- **`velocitylabs.cc/sdk/agent`** — Streamed `Runner` with `ToolExecutor` interface, pipelined tool execution, 429/5xx/empty-response retries, optional tool-round safety ceiling, and mid-run `AutoCompress` / `Steer` hooks.

## Install

```sh
go get velocitylabs.cc/sdk@latest
```

## Configuration

Process-wide settings live in the root package. Call `Configure` once at startup; empty fields keep their defaults.

```go
import "velocitylabs.cc/sdk"

sdk.Configure(sdk.Settings{
	AppName:        "MyApp",              // OpenRouter X-Title (none by default)
	AppURL:         "https://myapp.dev",  // OpenRouter HTTP-Referer (none by default)
	CacheKeyPrefix: "myapp-session-",     // default "velocity-session-"
	TempPrefix:     "myapp-",             // default "velocity-"
	DebugEnv:       "MYAPP_STREAM_DEBUG", // default "VELOCITY_STREAM_DEBUG" (+ "_RAW")
	// DisableDebugEnv: true,             // ignore the debug env var entirely
	// DisableDebugFiles: true,           // never save failed request/response payloads to temp files
})
```

## Transcription

```go
t, _ := inference.NewTranscriber(inference.TranscribeConfig{Type: "openrouter", APIKey: key})
res, err := t.Transcribe(ctx, inference.TranscribeRequest{Model: "openai/whisper-large-v3", Audio: wav, Format: "wav"})

t, _ = inference.NewTranscriber(inference.TranscribeConfig{
	Type:   "chat",
	Prompt: "Transcribe verbatim, keep speaker changes on new lines.",
	Chat:   &inference.Config{Type: "openrouter", APIKey: key},
})
res, err = t.Transcribe(ctx, inference.TranscribeRequest{Model: "google/gemini-2.5-flash", Audio: wav, Format: "wav"})
```

`openai` and `chat` transcribers also implement `provider.TranscribeStreamer` for incremental text deltas over a completed recording. True realtime (WebSocket) transcription is not included.
