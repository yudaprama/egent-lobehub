# AGENTS.md — egent-lobehub

## What is this?

An **Eino-based LLM agent server** that provides an OpenAI-compatible `/v1/chat/completions` endpoint. It's designed to serve as the backend agent runtime for [LobeHub](https://github.com/lobehub/lobe-chat), executing tool calls through a middleware pipeline (permission → error classification → truncation). Tools are defined declaratively in YAML.

## Quick start

```bash
# Build
go build ./...

# Test all packages
go test ./...

# Run (uses embedded agent_config.yaml, serves on port 10531)
go run .

# Run with external config
go run . -config /path/to/agent_config.yaml -port 10531
```

## Architecture

```
main.go — HTTP server, routes, OpenAI-compatible request/response types
├── yamlconfig/   — YAML config loading (ToolDef, AgentConfig)
├── tool/         — APITool builder from YAML, HTTP tool executor
├── agent/        — Eino ChatModelAgent + Runner creation
├── runtime/      — Runtime lifecycle, AiAgentService, approval, context builder, tool resolver
├── middleware/   — Tool wrappers: truncation, error classification, permission gate
├── memory/       — User memory extraction (heuristic), in-memory store, memory tools
├── config/       — Layered agent config merging (default → server → user → agent)
```

### Control flow

```
HTTP POST /v1/chat/completions
  → handlers.go:chatCompletionsHandler
    → extractUserID (header priority: x-arch-actor-id > X-User-ID > Authorization:kratos > anonymous)
    → memory.WithUserID(ctx, userID)  // injects user ID into context
    → runtime.Query(ctx, query)
      → agent.NewAgent → adk.NewChatModelAgent (CloudWeGo Eino)
        → Each tool wrapped: permission gate → error classification + truncation
      → runner.Query → AsyncIterator[*adk.AgentEvent]
    → handleStreamingResponse or handleNonStreamingResponse
```

## Key packages

### `agent` (package `agent`)
- `NewAgent(ctx, *AgentConfig, *AgentOptions) -> adk.Agent` — creates ChatModelAgent with middleware-wrapped tools
- `NewRunner(ctx, adk.Agent) -> *adk.Runner`
- Default model: `custom/glm-5.1`, default base URL: `http://localhost:12000/v1`

### `runtime` (package `runtime`)
- `Runtime` — lifecycle: `New` → `RegisterTools` → `Start` → `Query` / `Close`
- `AiAgentService` — full agent pipeline: config merge → context injection → tool resolve → agent execute
- `ApprovalGate` — human-in-the-loop via Eino interrupts (modes: `headless`, `always`, `on_demand`)
- `ContextBuilder` — assembles system prompt with memory, persona, document, and skill blocks
- `ToolResolver` — multi-source tool registry with permission middleware wrapping

### `tool` (package `tool`)
- `BuildToolsFromConfig(cfg) -> []tool.BaseTool` — builds APITool instances from YAML ToolDefs
- `APITool` — HTTP tool caller with: URL template substitution, env var resolution, JSON-string fallback parsing, Cloudflare-style envelope unwrapping, empty query param cleanup
- Env var syntax in URLs: `$VAR_NAME` (all-caps, must start with letter)

### `middleware` (package `middleware`)
- `WrapWithMiddleware` — applies classification + truncation, then optional permission gate
- `ClassifiedToolMiddleware` — timing, error classification, truncation
- `TruncationMiddleware` — tool result truncation (default 25k chars)
- `PermissionGate` — blocks disabled tools with standardized response
- `ClassifyToolError` — classifies errors as `replan` / `retry` / `stop` via code, HTTP status, and keyword heuristics

### `memory` (package `memory`)
- `Manager` — `ExtractAndStore` (heuristic: name/location/preferences), `Recall` → injected into system prompt
- `InMemoryStore` — dev/test store; swap for PG-backed in production
- `Store` interface: `Set`, `Get`, `Delete`, `Search`, `List`
- Memory tools: `userMemory_set`, `userMemory_get`, `userMemory_search`, `userMemory_list`
- User ID scoped via context key (`memory.WithUserID` / `UserIDFromContext`)

### `config` (package `config`)
- 4-layer merge: `DefaultAgentConfig` → server → user → agent
- Workspace-scoped config skips the user layer
- `SERVER_DEFAULT_AGENT_CONFIG` env var for server defaults (JSON blob)

### `yamlconfig` (package `yamlconfig`)
- YAML schema: `version`, `system_prompt`, `tools[].{name, description, url, method, parameters, http_headers}`

## API endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/v1/chat/completions` | POST | OpenAI-compatible chat completions (stream + non-stream) |
| `/v1/tools` | GET | List registered tool names |
| `/health` | GET | Health check (`{"status":"ok"}`) |

## Agent config (YAML)

```yaml
version: v1
system_prompt: |
  You are a helpful AI assistant.
tools:
  - name: my_tool
    description: Tool description.
    url: https://api.example.com/endpoint/{param}?key=$API_KEY
    method: GET
    parameters:
      - name: param
        description: Path parameter
        type: str
        required: true
        in_path: true
      - name: limit
        description: Result count
        type: int
        default: 10
    http_headers:
      Authorization: Bearer $TOKEN
```

## Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `PLANO_LLM_GATEWAY` | LLM API base URL | `http://localhost:12000/v1` |
| `MODEL_NAME` | Model identifier | `custom/glm-5.1` |
| `SERVER_DEFAULT_AGENT_CONFIG` | JSON blob for server-layer config overrides | (none) |
| `.env` | Auto-loaded from CWD and parent dir of config | — |

## User ID extraction priority

1. `x-arch-actor-id` header (Plano brightstaff after Talos verify)
2. `X-User-ID` header (dev/auth-proxied)
3. `Authorization: kratos:<session_token>` (prod)
4. Default: `"anonymous"`

## Testing

```bash
# All packages
go test ./...

# Single package
go test ./middleware/

# Verbose
go test -v ./runtime/
```

Test files: `handlers_test.go`, `config/config_test.go`, `memory/memory_test.go`, `middleware/middleware_test.go`, `runtime/*_test.go` (agent, approval).

## Gotchas & non-obvious patterns

1. **Tools require at least one named tool** — `BuildToolsFromConfig` returns an error if the tool list is empty (no tools built from config).
2. **API key is hardcoded `"EMPTY"`** in `agent/agent.go:73` — the gateway (`PLANO_LLM_GATEWAY`) is expected to handle auth transparently or use a proxy.
3. **Non-GET tools get a 90s timeout** vs 15s for GET, in `tool/api_tool.go:43-44`.
4. **JSON-string fallback** — if the LLM serializes an array/object arg as a JSON string, `APITool.InvokableRun` parses it back (`tool/api_tool.go:80-88`).
5. **Cloudflare-style envelope unwrapping** — POST tool responses with `{"success":true,"result":"..."}` get unwrapped to just the result field (`tool/api_tool.go:146-163`).
6. **Unused params are harmless** — remaining `{param}` placeholders and empty query params are silently removed.
7. **Memory is heuristic-only** — `extractHeuristic` in `memory/manager.go` does simple pattern matching (`"my name is X"`, `"I live in X"`, `"I like X"`). Production should use LLM extraction.
8. **User ID flows through context** — `memory.WithUserID(ctx, id)` sets it; memory tools retrieve it via `UserIDFromContext`. Forgetting to inject it means tools get an empty user ID and error.
9. **Config layers** — 4-layer merge with workspace scope: workspace-scoped agents skip the user config layer to prevent personal defaults from leaking.
10. **`cleanNil` in `config/config.go`** strips null/empty values during merge, preventing empty YAML from overriding defaults.
11. **Two parallel approaches to tool resolution** — `runtime/runtime.go` uses `RegisterTools` + middleware-wrapping in `agent.NewAgent`; `runtime/aiAgent_tools.go` has a newer `ToolResolver` with multi-source resolution. Check which one is active before adding new tool sources.
12. **Approval interrupts** use Eino's `tool.Interrupt`/`tool.GetInterruptState`/`tool.GetResumeContext` dance. See `runtime/approval.go`.
13. **`IsInterruptEvent` uses JSON roundtrip** to detect approval interrupts from agent events (reflection-free).
14. **Two unused `ctx` params** — `runtime/aiAgent.go:188` and `:232` are unused (known, `unusedparams` lint complaint).
15. **No CI/CD configs** — no `.github/`, `.cursor/rules/`, or Makefile. Build/test is ad-hoc.
