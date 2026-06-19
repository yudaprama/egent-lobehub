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
| `chunk`, `knowledge`, `search` (RAG/semantic search) | `knowledge/` package (wraps `github.com/kawai-network/fileprocessor`) |
| Agent config merge (4-layer) | `config/config.go` |
| `UserInterventionConfig` | `runtime/approval.go` |
| MCP/plugin tool resolution | `runtime/aiAgent_tools.go` (ToolResolver) |
| `ComposioService` (Slack/Gmail/GitHub/etc.) | `connectors/composio/` (REST) + `connectors/composio/eino/` (Eino tool) + `composio_handlers.go` (HTTP) |

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
│   └── task/     — Durable task execution via Temporal (workflow + activities + saga)
├── middleware/   — Tool wrappers: truncation, error classification, permission gate
├── memory/       — User memory extraction (heuristic), in-memory store, memory tools
├── knowledge/    — Semantic search over ingested files (pgvector, HNSW); `knowledge_search` agent tool
├── config/       — Layered agent config merging (default → server → user → agent)
├── connectors/composio/         — Composio REST client (v3.1, stdlib only)
├── connectors/composio/eino/    — Eino tool adapter: `ComposioTool` + `Builder` + `RESTAccountStore` (pREST)
├── composio_handlers.go          — `/v1/composio/*` HTTP handlers (connection lifecycle, OAuth callback)
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

### `knowledge` (package `knowledge`)
- `Service` — wires `fileprocessor.NewPublicEmbeddingsStoreWithPool` + `NewPostgresFileStore.ChunkStore()` + `NewSearcher` against the existing lobehub `public.embeddings` table (dim 1024)
- `Service.UserFileIDs(ctx, userID)` — queries `public.files WHERE user_id = $1` to scope the user's file IDs before semantic search
- `KnowledgeSearchTool` — exposes `knowledge_search` to the agent (reads userID from context via `memory.UserIDFromContext`)
- `KnowledgeBackend` interface — allows tests to inject a fake `Searcher` and `UserFileIDs` without a DB
- Disabled by default: set `KNOWLEDGE_PG_DSN` + `OPENAI_API_KEY` (or `MODEL_API_KEY`) in env to enable
- Ingestion happens in AList via `RegisterFileUploadedHook` in `alist/cmd/bridge.go` (uses the same `fileprocessor` library)
- Dependency: `github.com/kawai-network/fileprocessor v0.4.1` (bundles `pgvector-go v0.4.0`, `jackc/pgx v5.10.0`)

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

### `runtime/task` (package `task`)
Durable task execution via Temporal. The Go port of LobeHub's
`TaskRunnerService` + `TaskLifecycleService` pair (TS source at
`apps/server/src/services/taskRunner/` and
`apps/server/src/services/taskLifecycle/`).

**Why this exists:** LobeHub's TS task runner lives in a Next.js request
handler. If the process dies mid-execution there is no durable recovery
path — the task is stuck in `running` forever and the heartbeat fuse is
the only cleanup. Moving the lifecycle into Temporal workflows gives:
  - Retryable activities (transient LLM failures no longer fail the task)
  - Persisted workflow state (resume from last successful activity after
    a crash)
  - Saga compensations (atomic multi-step task with rollback on failure)
  - Query + signal handlers (status / cancel from any worker)

**Key types:**
- `TaskRunWorkflow` — the Temporal workflow function. Encapsulates the
  full run: resolve → build prompt → transition to running → run agent
  → on-topic-complete → cascade downstream tasks. Each side-effecting
  step registers a `Compensation`; on terminal failure all compensations
  run in reverse order (`runCompensations`).
- `Activities` — all the side-effecting work. Bound to a `TaskStore`
  and an `AgentExecutor` (the boundary between the workflow layer and
  the Eino runtime). Activities are defensive about non-Temporal
  contexts so they can be unit-tested without spinning up a worker.
- `TaskStore` — the persistence layer (resolve, status transitions,
  topic links, checkpoint config, cascade). `InMemoryStore` is the
  reference implementation and is what ships in dev; production must
  wire a Postgres-backed implementation (see
  `LOBEHUB_BACKEND_DATABASE_MAP.md` for the table inventory).
- `AgentExecutor` — the boundary between the workflow and the Eino
  runtime. `RuntimeExecutor` wraps `runtime.AiAgentService.ExecAgent`;
  `MockExecutor` is the test fake.
- `Worker` — in-process Temporal worker. `Start` registers the workflow
  + activities and begins polling; `Stop` is graceful shutdown.
  `RegisterHTTP` wires `/v1/tasks/{run,counts,*}` endpoints onto a mux.

**HTTP endpoints** (mounted by `Worker.RegisterHTTP`):
  - `POST /v1/tasks/run` — start a TaskRunWorkflow
  - `GET  /v1/tasks/{id}/status` — query the workflow's status handler
  - `GET  /v1/tasks/{id}/result` — query the workflow's result handler
  - `POST /v1/tasks/{id}/cancel` — signal the workflow to cancel
  - `GET  /v1/tasks/counts` — debug endpoint, counts by status

**Activation:** set `TEMPORAL_HOST_PORT` (e.g. `localhost:7233`) in the
environment. When unset, the `/v1/tasks/*` endpoints return 503 and
the chat-completions API is unaffected.

**Dependency:** `go.temporal.io/sdk v1.45.0`. Run `temporal server
start-dev` locally to get a cluster without docker-compose.

**Limitations / roadmap:**
- `EmitHandoff`, `SynthesizeTopicBrief`, `RunAutoReview` are stubs.
  Production deployments must replace their bodies with the LLM calls
  (the activity names are stable so workers can be swapped in).
- `TaskStore` is in-memory. A Postgres implementation is the next step;
  the interface is in `store.go`.
- Cascade spawns child workflows but does not yet wait for them.
- Signal handlers (`cancel`, `pause`) are stubbed; query handlers
  (`status`, `result`, `status_detail`) are wired.

### `yamlconfig` (package `yamlconfig`)
- YAML schema: `version`, `system_prompt`, `disabled_tools[]`, `tools[].{name, description, url, method, parameters, http_headers}`

### `connectors/composio` (package `composio`)
Go REST client for the [Composio](https://composio.dev) platform
(`https://backend.composio.dev/api/v3.1`). A v3.1 port of the community
`groq-go/extensions/composio` package (which targets the deprecated v1/v2
API and hard-depends on groq-go). The groq-go dep is removed so the package
stays vendorable into any Go service.

**Why this exists:** LobeHub ships a TypeScript Composio client
(`lobehub/src/server/services/composio/`) and a tRPC router
(`lobehub/apps/server/src/routers/lambda/composio.ts`) that drives 250+
third-party SaaS integrations (Slack, Gmail, GitHub, Notion, Linear, etc.)
via Composio's managed OAuth. The Go port lets egent-lobehub expose the
same surface without duplicating the per-app OAuth/manifest work.

**Surface** (mirrors the TS `ComposioService`):
- `NewComposer(apiKey, opts...) -> (*Composio, error)` — returns nil when apiKey is empty so callers can branch on availability
- Connection lifecycle: `LinkConnection`, `GetConnection`, `DeleteConnection`, `ListConnections`
- Auth configs: `ListAuthConfigs`, `CreateManagedAuthConfig`, `FindAuthConfigForToolkit`, `ResolveOrCreateAuthConfig`
- Tools: `GetTools`, `GetToolsForApp`, `ExecuteTool`
- Catalog: `COMPOSIO_APP_TYPES` (21+ apps), `GetAppByIdentifier`, `GetAppBySlug`, `NormaliseSlug`
- Structured errors: `APIError` with `IsAuthError()` (401/403) and `IsRetryable()` (429/5xx)

**Env:** `COMPOSIO_API_KEY` (required), `COMPOSIO_BASE_URL` (optional override).

**Test coverage:** 29 mocked unit tests + 10 live integration tests (skipped when `COMPOSIO_API_KEY` unset).

### `connectors/composio/eino` (package `eino`)
Eino adapter wrapping `connectors/composio` as `tool.InvokableTool`s.

**Key types:**
- `ComposioTool` — single Eino `InvokableTool` backed by one Composio action. Lazy-parses JSON Schema on first `Info()` call.
- `Builder` — fetches manifest per app, emits one `ComposioTool` per action. Skips apps whose manifest fetch fails.
- `ConnectedAccountStore` (interface) — `Resolve(ctx, userID, appIdentifier) -> (connectedAccountID, error)`. Returning `("", nil)` triggers `NotConnectedError`.
- `RESTAccountStore` — pREST-backed impl: `GET /lobehub/public/plugins?identifier=eq.<id>`, reads `custom_params.composio.connected_account_id`, filters by `user_id`.

**Schema conversion:** Composio's `input_parameters` JSON Schema → `eino-contrib/jsonschema` → `schema.NewParamsOneOfByJSONSchema`. Preserves `anyOf`, `oneOf`, `$defs`.

**Wiring:** `main.go` blocks on `COMPOSIO_API_KEY` + `PREST_URL` (default `http://localhost:3000`) and registers the resulting tools at startup.

**Test coverage:** 14 adapter tests + 7 pREST-store tests.

### `composio_handlers.go` (package `main`)
HTTP handlers mounted at `/v1/composio/*` that replace the tRPC router at
`lobehub/apps/server/src/routers/lambda/composio.ts`. The LobeChat frontend
can drive connection lifecycle through Go instead of Next.js.

| Method | Path | Purpose | TS parity |
|---|---|---|---|
| POST | `/v1/composio/connections` | Resolve/create auth config + start OAuth link | `composio.createConnection` |
| POST | `/v1/composio/connections/poll` | Poll `GetConnection` for ACTIVE | `composio.getConnection` |
| POST | `/v1/composio/connections/delete` | Best-effort remote + local delete | `composio.deleteConnection` |
| GET  | `/v1/composio/plugins` | List user's connected plugins | `composio.getComposioPlugins` |
| POST | `/v1/composio/plugins/update` | Mark ACTIVE + persist tool count | `composio.updateComposioPlugin` |
| POST | `/v1/composio/plugins/remove` | Remove local plugin entry | `composio.removeComposioPlugin` |
| GET  | `/v1/composio/oauth/callback` | Popup-closing HTML | `lobehub/src/app/(backend)/api/composio/oauth/callback/route.ts` |

**State:** in-memory `sync.Map` keyed by `connected_account_id`. Swap for a pREST-backed `PluginStore` (read/write `lobehub.public.plugins`) when scaling.

**Test coverage:** 9 handler tests with stdlib `httptest`.

## API endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/v1/chat/completions` | POST | OpenAI-compatible chat completions (stream + non-stream) |
| `/v1/tools` | GET | List registered tool names |
| `/v1/composio/connections` | POST | Start OAuth link for a third-party app |
| `/v1/composio/connections/poll` | POST | Poll connection lifecycle status |
| `/v1/composio/connections/delete` | POST | Remove a connection |
| `/v1/composio/plugins` | GET | List user's connected plugins |
| `/v1/composio/plugins/update` | POST | Mark plugin ACTIVE |
| `/v1/composio/plugins/remove` | POST | Remove a local plugin entry |
| `/v1/composio/oauth/callback` | GET | OAuth popup landing (auto-closes after 300ms) |
| `/health` | GET | Liveness check |
| `/health/ready` | GET | Readiness probe: 200 or 503 |

## Environment variables

| Var | Default | Used by | Purpose |
|---|---|---|---|
| `OPENAI_API_KEY` / `MODEL_API_KEY` | — | `agent/` | LLM provider key |
| `MODEL_BASE_URL` | `https://openrouter.ai/api/v1` | `agent/` | LLM provider base URL |
| `MODEL_NAME` | `custom/glm-5.1` | `agent/` | Model name |
| `KNOWLEDGE_PG_DSN` | — | `knowledge/` | Enables `knowledge_search` tool |
| `OPENAI_EMBEDDINGS_URL` | — | `knowledge/` | OpenAI-compatible embedding endpoint |
| `OPENAI_EMBEDDINGS_MODEL` | `text-embedding-3-small` | `knowledge/` | Embedding model (must produce 1024-dim) |
| `COMPOSIO_API_KEY` | — | `connectors/composio/`, `composio_handlers.go` | Project-scope Composio key. When set, registers Eino tools per action + exposes `/v1/composio/*` HTTP. When unset, all Composio code is no-op. |
| `COMPOSIO_BASE_URL` | `https://backend.composio.dev/api/v3.1` | `connectors/composio/` | Override for self-hosted/staging. |
| `PREST_URL` | `http://localhost:3000` | `connectors/composio/eino/`, `composio_handlers.go` | pREST base URL for `RESTAccountStore` + plugin lookups. |
| `TEMPORAL_HOST_PORT` | — | `runtime/task/` | Enables Temporal task worker. When unset, `/v1/tasks/*` returns 503. |
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
| `KNOWLEDGE_PG_DSN` | Postgres DSN for `knowledge_search` tool (enable RAG over ingested files) | (none — tool disabled) |
| `OPENAI_EMBEDDINGS_URL` | Embedding API endpoint (OpenAI-compatible) | `MODEL_BASE_URL + /embeddings` |
| `OPENAI_EMBEDDINGS_MODEL` | Embedding model name | `text-embedding-3-small` (1024 dims) |
| `OPENAI_API_KEY` | API key for embeddings (fallback: `MODEL_API_KEY`) | (none — uses OpenRouter random key) |
| `MODEL_API_KEY` | Fallback API key for embeddings when `OPENAI_API_KEY` is not set | (none) |
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
17. **Knowledge search is disabled by default** — `KNOWLEDGE_PG_DSN` must be set for the `knowledge_search` tool to be wired. Without it the agent only has memory tools and HTTP API tools. When enabled, per-user scoping happens via `public.files.user_id` — users only see chunks from their own ingested files.
18. **Embedding dim is pinned to 1024** — the lobehub `public.embeddings` schema uses `vector(1024)`. The embedder is constructed with `dim=1024` regardless of the underlying model's max dim. `text-embedding-3-small` supports 1024 natively; for other models, the dim is truncated by the OpenAI API `dimensions` parameter.
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
- **Knowledge search** (`knowledge_search` agent tool) — semantic search over `public.embeddings` via `fileprocessor.Searcher`; per-user scoping; dim=1024, HNSW, `pgvector-go v0.4.0` bundled via `github.com/kawai-network/fileprocessor v0.4.1`
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
