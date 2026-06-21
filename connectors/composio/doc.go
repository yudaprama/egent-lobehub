// Package composio is a thread-safe Go client for the Composio platform
// (https://composio.dev), used by egent-lobehub to expose 250+ third-party
// SaaS tools (Slack, Gmail, GitHub, Notion, Linear, etc.) to LobeHub agents
// without per-app OAuth, manifest, or SDK work.
//
// # Why this package exists
//
// Composio ships no official Go SDK; @composio/core (v0.10.x, TypeScript) is
// a thin wrapper around the same REST API this client calls directly. There
// is one community package — github.com/conneroisu/groq-go/extensions/composio
// — but it targets the deprecated v1/v2 REST API and hard-depends on the
// groq-go module. This package is a v3.1 port with groq-go removed, plus
// the four connection-lifecycle methods (LinkConnection, GetConnection,
// DeleteConnection, ResolveOrCreateAuthConfig) that the community package
// lacks. Naming and types align with the groq-go extension so fixes can
// upstream later if maintainers want a v3.1 cut.
//
// # Endpoints covered
//
// Base URL: https://backend.composio.dev/api/v3.1
// Auth:     x-api-key: <COMPOSIO_API_KEY> (project scope)
//
//	GET  /connected_accounts                      → ListConnections
//	GET  /connected_accounts/{id}                 → GetConnection
//	POST /connected_accounts/link                 → LinkConnection
//	DELETE /connected_accounts/{id}               → DeleteConnection
//	GET  /auth_configs                            → ListAuthConfigs
//	POST /auth_configs                            → CreateManagedAuthConfig
//	GET  /tools?toolkit_slug=…                    → GetTools / GetToolsForApp
//	POST /tools/execute/{slug}                    → ExecuteTool
//
// Not covered: triggers, files upload, tool router sessions, the workbench
// — none of these are needed by egent-lobehub today.
//
// # Relationship to the rest of egent-lobehub
//
// The package is intentionally a pure REST client with no dependency on
// Eino, the runtime, or Postgres. A separate adapter (planned; see
// LOBEHUB_BACKEND_DATABASE_MAP.md §0 Tier 3) will wrap each connection's
// toolset as Eino tools and register them through runtime.ToolResolver.
//
// # LobeHub TS parity map
//
//	getComposioClient()                          → NewComposer
//	composio.tools.getRawComposioTools({tk})     → (*Composio).GetTools
//	composio.tools.execute(slug, {...})          → (*Composio).ExecuteTool
//	composio.authConfigs.list()                  → (*Composio).ListAuthConfigs
//	composio.authConfigs.create(slug, {managed}) → (*Composio).CreateManagedAuthConfig
//	composio.connectedAccounts.link(u, a, o)     → (*Composio).LinkConnection
//	composio.connectedAccounts.get(id)           → (*Composio).GetConnection
//	composio.connectedAccounts.delete(id)        → (*Composio).DeleteConnection
package composio
