# Velocity SDK (`velocitylabs.cc/sdk`)

Standalone Go SDK for multi-provider LLM streaming, context management, and agentic tool execution.

## Packages

- **`velocitylabs.cc/sdk/provider`** — Streaming LLM providers (`openai-compatible`, `openrouter`, `responses`, `copilot`, `copilot-responses`, `chatgpt`, `xai`, `crof`, `zen`, `cline`), capability reporting, reasoning & `reasoning_details` persistence, PNG CRC repair, image compression, model discovery, and non-SSE JSON completion fallback. Credentials/tokens are injected by the host application (`APIKey` or `TokenSource`).
- **`velocitylabs.cc/sdk/inference`** — Provider `Registry`, `Router`, `Config`, and automatic provider type detection.
- **`velocitylabs.cc/sdk/context`** — Append-only `Event` log, capability-aware `RenderHistory`, token ceiling `ModelLimit`, registrable `ContextSection` internal context (only changed sections/lines are appended, so volatile context never breaks the prompt cache), and prompt-cache-preserving `CompressConversation` with `<files_to_retain>` via pluggable `FileReader`.
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
})
```
