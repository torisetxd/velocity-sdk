# Velocity SDK (`velocitylabs.cc/sdk`)

Modular Go library for multi-provider streaming LLM inference, context management & compression, and pipelined agent execution.

## Packages

- **`velocitylabs.cc/sdk/provider`** — Credential-agnostic streaming clients (`openai-compatible`, `openrouter`, `responses`, `copilot`, `copilot-responses`, `chatgpt`, `xai`, `crof`, `zen`, `cline`), capability reporting, reasoning persistence, PNG CRC repair, image compression, and model discovery.
- **`velocitylabs.cc/sdk/inference`** — Provider `Registry`, runtime `Router`, and automatic provider type detection.
- **`velocitylabs.cc/sdk/context`** — Append-only `Event` log, capability-aware `RenderHistory`, token ceiling `ModelLimit`, prompt-cache-preserving `CompressConversation` with `<files_to_retain>` via pluggable `FileReader`, and `GenerateHandoff`.
- **`velocitylabs.cc/sdk/agent`** — Streamed `Runner` with `ToolExecutor`, pipelined tool execution, rate-limit/5xx/empty-turn retries, tool-round safety ceiling, and mid-run `AutoCompress` / `Steer` hooks.

## Installation

```bash
go get velocitylabs.cc/sdk@latest
```
