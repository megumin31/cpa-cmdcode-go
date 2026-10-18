<div align="right">
  <strong>English</strong> | <a href="README_zh.md">简体中文</a>
</div>

# Command Code Go Plugin for CLIProxyAPI (CPA)

A high-performance C-shared plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) that integrates the Command Code Go ($1/mo plan) LLM ecosystem into standard OpenAI-compatible endpoints without requiring the Command Code CLI to run in the background.

---

## Features

- **Full Capability & Parity**: Comprehensive support for streaming (SSE), non-streaming completions, tool calls (function calling), reasoning content / thinking tokens, and multimodal image input.
- **Isolated Namespaced Models**: Models are registered as `cmdcode-go/<model_name>` (e.g., `cmdcode-go/deepseek-v4-flash`, `cmdcode-go/Kimi-K3`) without the manufacturer segment; the `cmdcode-go/` prefix keeps the public names separate from other providers.
- **Zero-Config Credential Detection**: Supports explicit `api_key` in CPA configuration, `COMMANDCODE_KEY` environment variable, or automatic discovery of local CLI login state (`~/.commandcode/auth.json` from `cmd login`).
- **Dynamic Model Catalog & Auto-Sync**: Bundles a compiled-in model fallback and automatically synchronizes with remote catalog updates (tracked via GitHub Actions), with optional local JSON file overrides.
- **Resilient Streaming Architecture**: Built on a bounded NDJSON state machine and safe host callback lifecycle, ensuring graceful shutdown without leaked connections or memory corruption.

---

## Featured Available Models

The plugin discovers and routes all Command Code Go entitled models. Call them using the `cmdcode-go/<model_name>` namespace (manufacturer omitted):

| Model | CPA Model ID | Reference context | Capabilities & Notes |
| :--- | :--- | :--- | :--- |
| **DeepSeek V4 Flash** | `cmdcode-go/deepseek-v4-flash` | 1M | Fast hybrid-attention reasoning, thinking tokens |
| **DeepSeek V4 Flash Fast** | `cmdcode-go/deepseek-v4-flash-fast` | 1M | Ultra-low latency inference |
| **Kimi K3** | `cmdcode-go/Kimi-K3` | 1,048,576 | High-throughput reasoning & agentic tasks |
| **MiMo V2.6 Pro** | `cmdcode-go/mimo-v2.6-pro` | 1,048,576 | Multimodal model; reasoning controls remain provider-specific |
| **GLM-5.3** | `cmdcode-go/GLM-5.3` | 1M | Reasoning and tool-use model |
| **GPT-6 Luna** | `cmdcode-go/gpt-6-luna` | 1.05M | Long-context multimodal model |

The public ID and name use only `cmdcode-go/<model_name>`, for example `cmdcode-go/Kimi-K3`. Use the same format in requests and `models` / `disable_models` configuration. Reasoning suffixes such as `cmdcode-go/Kimi-K3(high)` are supported. Old manufacturer-qualified IDs and bare model names are no longer accepted. Gateway requests still use the original upstream ID (`moonshotai/Kimi-K3`); catalogs with colliding short names are rejected.

> See [models.json](models.json) for the current Go roster. Membership comes from the official Go page; capabilities are models.dev references, not verified gateway limits. Missing limits use a labeled 262,144-context / 65,536-output fallback profile. See [catalog generation and matching](docs/MODELS.md).

---

## Installation & Deployment

### 1. Download Pre-built Binary

Published [Releases](https://github.com/megumin31/cpa-cmdcode-go/releases) provide native Linux `amd64` and `arm64` archives after passing the native quality gate. Download the archive for your architecture, verify its checksum, and install `cmdcode-go-v0.1.0.so` into your CPA `plugins/` directory:

```bash
cp cmdcode-go-v0.1.0.so /path/to/cliproxyapi/plugins/
chmod 700 /path/to/cliproxyapi/plugins/cmdcode-go-v0.1.0.so
```

> **Note**: Do not rename the `.so` file, as CPA requires the versioned library name. See [release instructions](docs/RELEASE.md) for details.

### 2. Build from Source

Requires Go 1.26.9 and a C compiler (GCC or Clang):

```bash
git clone https://github.com/megumin31/cpa-cmdcode-go.git
cd cpa-cmdcode-go
mkdir -p build
LIBRARY="$(cd go && go run ./cmd/buildmeta --field filename)"
(cd go && CGO_ENABLED=1 go build -buildmode=c-shared -o "../build/$LIBRARY" .)
cp "build/$LIBRARY" /path/to/cliproxyapi/plugins/
chmod 700 "/path/to/cliproxyapi/plugins/$LIBRARY"
```

Source builds default to unreleased `0.0.0` (`build_tag=dev`). To publish a tested commit, push a new version tag; CI injects that version and creates the Release automatically. See [release instructions](docs/RELEASE.md).

### 3. Configure CPA

In your CPA `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cmdcode-go:
      enabled: true
      priority: 1
      api_key: "YOUR_COMMAND_CODE_KEY" # Optional if logged in via `cmd login`
```

### 4. Restart & Verify CPA

```bash
systemctl --user restart cliproxyapi
# or restart your CPA binary directly
```

Verify that models are registered:
```bash
curl -s http://localhost:8317/v1/models -H "Authorization: Bearer <your-cpa-api-key>" | jq '.data[].id'
```

---

## Configuration Options

Supported options under `plugins.configs.cmdcode-go`:

| Option | Type | Description | Default |
| :--- | :--- | :--- | :--- |
| `api_key` | string | Command Code account key (`user_...`) | Reads env vars / CLI auth file if omitted |
| `models` | list | Allowlist of models to expose | All models |
| `disable_models` | list | Denylist of models to exclude | None |
| `models_refresh_interval` | string | Interval to check for remote catalog updates | `"6h"` (`"0"` to use compiled-in roster only) |
| `models_file` | string | Path to a local model catalog JSON file | Unset (overrides remote URL if set) |
| `models_url` | string | URL to fetch model catalog JSON from | `models.json` on `main` branch |

---

## Usage Examples

### 1. Chat Completions (`/v1/chat/completions`)

```bash
curl -N http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cmdcode-go/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "Write a quicksort in Go."}],
    "stream": true
  }'
```

### 2. Tool Calling (Function Calling)

Downstream client tools are fully parsed, mapped, and forwarded upstream:

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cmdcode-go/Kimi-K3",
    "messages": [{"role": "user", "content": "What is 123 * 456?"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "multiply",
        "description": "Multiplies two numbers",
        "parameters": {
          "type": "object",
          "properties": {
            "a": {"type": "number"},
            "b": {"type": "number"}
          },
          "required": ["a", "b"]
        }
      }
    }],
    "tool_choice": "auto"
  }'
```

### 3. Reasoning & Thinking Tokens

For reasoning models (such as DeepSeek and Kimi), thinking output is emitted as `reasoning_content` deltas in streaming mode:

```bash
curl -N http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cmdcode-go/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "Solve: if x^2 + 5x + 6 = 0, what is x?"}],
    "stream": true
  }'
```

---

## Architecture & CI Verification

The repository maintains strict ownership boundaries across core components:

- **C ABI Adapter (`go/main.go`, `go/abi_runtime.go`)**: Owns C memory allocations, host callback bridges, safe instance reloads across `dlclose`, and exported plugin ABI symbols (`cliproxy_plugin_init`, `cliproxyPluginCall`, `cliproxyPluginFree`, `cliproxyPluginShutdown`).
- **Plugin Engine (`go/plugin.go`, `go/dispatch.go`, `go/executor.go`)**: Composition root managing RPC dispatch, task registration, coherent turn snapshots, and graceful shutdown.
- **ConfigStore (`go/config.go`)**: Thread-safe YAML decoding, validation, atomic updates, and defensive copies.
- **ModelRegistry (`go/models_remote.go`, `go/models.go`)**: Immutable model indexes with single-flight background refresh and compiled-in fallback.
- **GatewayClient (`go/gateway_client.go`)**: Reusable HTTP transport, authentication headers, timeouts, and connection pool management.
- **EventDecoder (`go/event_decoder.go`)**: Bounded NDJSON event parser shared between streaming and buffered responses.

Every branch push and pull request runs the native Linux amd64/arm64 quality gate: formatting, vet, unit tests, race detector, Python fixtures, workflow validation, and the C ABI reload harness. See [ARCHITECTURE.md](ARCHITECTURE.md) and [CI documentation](docs/CI.md).

---

## License

[AGPL-3.0](LICENSE)

### Conversation reuse

Explicit conversation IDs are reused only within the same CPA caller scope and
selected account. See [session identity and lifecycle](docs/SESSIONS.md) for
supported fields, stateless fallback, bounded cache lifetime, and concurrent-turn
HTTP 409 behavior.
