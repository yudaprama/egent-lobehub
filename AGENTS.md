# AGENTS.md — egent-lobehub

## What is this?

A **Go port of LobeHub's agent runtime** (originally TypeScript/Node.js) that provides an OpenAI-compatible `/v1/chat/completions` endpoint. Built on [CloudWeGo Eino](https://github.com/cloudwego/eino) as a higher-performance replacement for LobeHub's JS backend.

Designed to run behind **Plano** (auth proxy) — tools are defined declaratively in YAML and executed through a middleware pipeline (permission → error classification → truncation).

### LobeHub TypeScript → Go mapping

| LobeHub (TypeScript) | egent-lobehub (Go) |
|---|---|
| `AiAgentService` | `runtime/aiAgent.go` |
| `ToolExecutionService` + error classification | `middleware/error_classify.go` |
| `truncateToolResult.ts` | `middleware/truncate.go` |
| `connectorPermissionCheck.ts` | `middleware/permission.go` |
| `UserMemory` service | `memory/` package |
| Agent config merge (4-layer) | `config/config.go` |
| `UserInterventionConfig` | `runtime/approval.go` |
| MCP/plugin tool resolution | `runtime/aiAgent_tools.go` (ToolResolver) |

### Target architecture

```
Client (LobeChat frontend)
  → Plano brightstaff (auth via Talos verify, sets x-arch-actor-id)
    → egent-lobehub (port 10531, OpenAI-compatible API)
      → LLM gateway (PLANO_LLM_GATEWAY, port 12000)
        → Tool execution (YAML-defined HTTP APIs)
```

## Quick start

```bash
# Build (Makefile, injects git version)
make build

# Test all packages
make test    # or: go test ./...

# Run (uses embedded agent_config.yaml, serves on port 10531)
make run

# Run with external config
go run . -config /path/to/agent_config.yaml -port 10531

# Cross-compile all targets (linux/darwin × amd64/arm64)
make build-all

# Version tagging
make tag-patch   # v0.0.X → v0.0.(X+1)
make tag-minor   # v0.X.0
make tag-major   # vX.0.0
```

### CI/CD

GitHub Actions (`.github/workflows/release.yml`):
- **Triggers**: push to `main`, `v*` tags, manual dispatch
- **Build job**: tests → cross-compiles 4 targets → uploads artifacts
- **Release job** (tags only): creates GitHub Release with binaries, source archive, and `main.go` convenience download
- Semver pre-release detection: any dash after `vX.Y.Z` (e.g. `v0.0.3-rc1`) is marked as pre-release

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
      → ToolResolver.Resolve(ctx)  // wraps tools: permission gate → error classification + truncation
      → agent.NewAgent → adk.NewChatModelAgent (CloudWeGo Eino)
      → runner.Query → AsyncIterator[*adk.AgentEvent]
    → handleStreamingResponse or handleNonStreamingResponse
```

## Key packages

### `agent` (package `agent`)
- `NewAgent(ctx, *AgentConfig, *AgentOptions) -> adk.Agent` — creates ChatModelAgent with pre-wrapped tools (callers handle middleware via ToolResolver)
- `NewRunner(ctx, adk.Agent) -> *adk.Runner`
- Default model: `custom/glm-5.1`, default base URL: `http://localhost:12000/v1`

### `runtime` (package `runtime`)
- `Runtime` — lifecycle: `New` → `RegisterTools` (feeds ToolResolver) → `Start` (resolves + wraps) → `Query` / `Close`
- `ToolResolver` — single tool resolution path: register → resolve (with middleware wrapping) → agent
- `AiAgentService` — full agent pipeline: config merge → context injection → tool resolve → agent execute
- `ApprovalGate` — human-in-the-loop via Eino interrupts (modes: `headless`, `always`, `on_demand`)
- `ContextBuilder` — assembles system prompt with memory, persona, document, and skill blocks

### `tool` (package `tool`)
- `BuildToolsFromConfig(cfg) -> []tool.BaseTool` — builds APITool instances from YAML ToolDefs
- `APITool` — HTTP tool caller with: URL template substitution, env var resolution, JSON-string fallback parsing, Cloudflare-style envelope unwrapping, empty query param cleanup
- Env var syntax in URLs: `$VAR_NAME` (all-caps, must start with letter)
- Built-in demo tools (from `agent_config.yaml`):
  - `get_weather` — wttr.in weather lookup
  - `define_word` — dictionaryapi.dev definitions
  - `get_country_info` — REST Countries facts
  - `search_papers` — OpenAlex academic search

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
- YAML schema: `version`, `system_prompt`, `disabled_tools[]`, `tools[].{name, description, url, method, parameters, http_headers}`

## API endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/v1/chat/completions` | POST | OpenAI-compatible chat completions (stream + non-stream) |
| `/v1/tools` | GET | List registered tool names |
| `/health` | GET | Liveness check: `{"status":"ok","started":true,"tools":4,"version":"..."}` |
| `/health/ready` | GET | Readiness probe: 200 `{"status":"ready"}` or 503 `{"status":"not_ready"}` |

## Agent config (YAML)

```yaml
version: v1
system_prompt: |
  You are a helpful AI assistant.
disabled_tools:
  - dangerous_tool
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
| `DISABLED_TOOLS` | Comma-separated tool names to block via permission gate | (none) |
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
11. **Tool resolution is consolidated** — `Runtime.RegisterTools` feeds `ToolResolver`, which handles middleware wrapping during `Resolve()`. `agent.NewAgent` no longer wraps tools. All tool sources should register through `ToolResolver`.
12. **Approval interrupts** use Eino's `tool.Interrupt`/`tool.GetInterruptState`/`tool.GetResumeContext` dance. See `runtime/approval.go`.
13. **`IsInterruptEvent` uses JSON roundtrip** to detect approval interrupts from agent events (reflection-free).
14. **Two unused `ctx` params** — `runtime/aiAgent.go:188` and `:232` are unused (known, `unusedparams` lint complaint).
15. **Message history is structured** — `buildConversationQuery` in `handlers.go` preserves system messages as "System instructions:" context, separates conversation history with role prefixes, and passes the last user message cleanly. System messages from the request augment the agent's base instruction.
16. **Kratos auth is extract-only** — `extractUserID` reads `Authorization: kratos:<token>` but never validates the token against the Kratos admin API (marked `TODO` in `handlers.go:33`).
17. **Graceful shutdown** — server handles SIGINT/SIGTERM with 30s drain timeout. In-flight requests complete before exit. HTTP server has read (15s), write (120s), and idle (60s) timeouts.
18. **Structured logging** — all logging uses `log/slog` with text handler on stderr. Debug-level logs include tool truncation details, memory context injection, and env var warnings.
19. **Request context propagation** — `APITool.InvokableRun` uses the incoming `ctx` (from Eino's tool execution pipeline) for HTTP requests. This means request-level deadlines and cancellation propagate into tool calls. The HTTP handler passes `r.Context()` through `memory.WithUserID` → `runtime.Query` → Eino runner → tool.
20. **`AiAgentService.ExecAgent` returns an iterator** — `ExecAgentResult.Events` is a raw event iterator. Callers consume it for streaming or use `CollectResult(iter)` for buffered output. The `Stream` field was removed from `ExecAgentParams`.
21. **Permission gate is wired** — `DISABLED_TOOLS` env var (comma-separated) and `disabled_tools` YAML field both feed `PermissionConfig.DisabledTools`. Disabled tools return a standardized blocked response instead of executing. Env var entries are merged on top of YAML entries.

## Status & roadmap

### Done

- OpenAI-compatible `/v1/chat/completions` (streaming + non-streaming)
- YAML-declarative tool definitions with HTTP execution
- Middleware pipeline: permission gate → error classification → truncation
- Permission checker wired (YAML `disabled_tools` + `DISABLED_TOOLS` env var)
- 4-layer config merge (default → server → user → agent) with workspace scoping
- User memory system (heuristic extraction + 4 agent-callable tools)
- Human-in-the-loop approval via Eino interrupts
- Context builder (persona, memory, documents, skill hints)
- Structured message history (system messages preserved, role-separated turns)
- Consolidated tool resolution via `ToolResolver` (single path: register → resolve → wrap → agent)
- Graceful shutdown (SIGINT/SIGTERM, 30s drain, HTTP server timeouts)
- Structured logging via `log/slog` (debug/info/warn/error levels)
- Request context propagation into tool HTTP calls (deadlines + cancellation)
- `AiAgentService.ExecAgent` returns event iterator (caller-driven streaming/buffered)
- Deep health check (`/health` with runtime status + tool count, `/health/ready` for K8s readiness)
- GitHub Actions CI/CD with cross-compilation and releases
- Makefile with build/test/release helpers

### TODO / not yet production-ready

- **Memory extraction is heuristic-only** — needs LLM-based extraction for production
- **MCP/plugin/market tool sources** — enum + resolver skeleton exists but not wired
- **Kratos token validation** — user ID extracted but never verified
- **No rate limiting or request validation** — relies on Plano gateway for these
