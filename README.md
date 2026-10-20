# Velocity SDK (`velocitylabs.cc/sdk`)

Standalone Go SDK for multi-provider LLM streaming, context management, and agentic tool execution.

## Packages

- **`velocitylabs.cc/sdk/provider`** — Streaming LLM providers (`openai-compatible`, `openrouter`, `responses`, `copilot`, `copilot-responses`, `chatgpt`, `xai`, `crof`, `zen`, `cline`), capability reporting, reasoning & `reasoning_details` persistence, PNG CRC repair, image compression, model discovery, non-SSE JSON completion fallback, and speech-to-text `Transcriber`s (OpenAI-style multipart `/audio/transcriptions` with optional SSE deltas, OpenRouter's JSON transcription endpoint, and chat-model audio input via `input_audio` with a customizable prompt). Credentials/tokens are injected by the host application (`APIKey` or `TokenSource`).
- **`velocitylabs.cc/sdk/inference`** — Provider `Registry`, `Router`, `Config`, automatic provider type detection, and `NewTranscriber` (`openai`, `openrouter`, `chat`).
- **`velocitylabs.cc/sdk/context`** — Append-only `Event` log, capability-aware `RenderHistory`, token ceiling `ModelLimit`, registrable `ContextSection` internal context (only changed sections/lines are appended, so volatile context never breaks the prompt cache), and and prompt-cache-preserving `CompressConversation`, with optional `<files_to_retain>` file carry-over enabled by supplying a `FileReader` (`DirFileReader` for local disk).
- **`velocitylabs.cc/sdk/webfetch`** — `web_fetch` tool: markdown-first URL fetching (`.md`, `.mdx`, `Accept: text/markdown`, then boilerplate-pruned HTML conversion), binary-payload notices, token-budgeted output, Cloudflare email decoding, an SSRF-safe `NewSafeClient`, and an opt-in crawler-spoof ladder (Googlebot, Bingbot, GPTBot, ChatGPT-User, ClaudeBot, PerplexityBot, ...) for pages that serve a bot wall to unknown clients.
- **`velocitylabs.cc/sdk/pricing`** — models.dev catalog (`Source` with TTL, disk fallback) for per-million-token prices, tiered context pricing and context windows; `Model.Cost(usage)` prices requests for providers that do not report `usage.cost` themselves (OpenRouter does, as `provider.TokenUsage.Cost`).
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

## Web fetch

```go
out, err := webfetch.Fetch(ctx, "https://example.com/docs", webfetch.Options{
	Client: webfetch.NewSafeClient(30 * time.Second), // refuse private/loopback targets
	Spoof:  true,                                     // retry as crawlers when blocked
})
// or register the ready-made tool: webfetch.ToolDef() + webfetch.Execute(ctx, args, opts)
```

Spoofing only kicks in when the first response is a denial status (401/403/406/429/503...) or a short challenge/CAPTCHA/JS-required page. Sites that verify crawlers by reverse DNS will still refuse the spoofed identities.

## Image generation

```go
g, _ := inference.NewImageGenerator(inference.ImageConfig{Type: "openrouter", APIKey: key})
res, err := g.GenerateImage(ctx, inference.ImageRequest{Model: "google/gemini-2.5-flash-image", Prompt: "a lighthouse at dusk"})
// res.Images[i].Data / .MIMEType; res.Usage.Cost is the provider-reported USD charge
```

`openai` targets `/images/generations` (and `/images/edits` when `Images` are supplied, e.g. `gpt-image-1`, local servers); `openrouter` uses image output through `/chat/completions`. Both accept `b64_json`, data-URI and URL results.

## Speech synthesis

```go
sp, _ := inference.NewSpeaker(inference.SpeechConfig{APIKey: key}) // or APIURL of a local OpenAI-style TTS server
res, err := sp.Speak(ctx, inference.SpeechRequest{Model: "gpt-4o-mini-tts", Voice: "alloy", Input: "Hello", Format: "mp3"})
// res.Audio, res.MIMEType; provider.SpeechStreamer.StreamSpeech writes audio to an io.Writer as it arrives
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
