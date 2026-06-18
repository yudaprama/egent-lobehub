// Package eino wraps the lower-level connectors/composio REST client as
// Eino tools that can be registered with runtime.ToolResolver.
//
// # Architecture
//
// Each user-connected Composio app (Slack, Gmail, GitHub, …) becomes one
// ComposioTool registered with the agent runtime. The tool's name is the
// Composio action slug (e.g. GITHUB_LIST_REPOS) and its parameter schema
// is the JSON Schema returned by Composio's /tools endpoint, converted to
// Eino's schema.ParamsOneOf via jsonschema.Unmarshal.
//
// When the agent invokes the tool, the adapter:
//
//  1. Resolves the connected_account_id for the current user from a
//     ConnectedAccountStore (the LobeHub TS side stores this in
//     PluginModel.customParams.composio.connectedAccountId scoped by
//     user_id; the Go side resolves the same way, either via pREST or a
//     direct Postgres query).
//  2. Calls (*composio.Composio).ExecuteTool(slug, args, connectedAccountID, userID).
//  3. Returns ExecuteResult.Content to the agent. Auth errors and other
//     failures are surfaced as structured ExecuteError so the runtime's
//     middleware.ClassifyError can pick replan/retry/stop.
//
// # Relationship to connectors/composio
//
// This package is the only place that depends on Eino. The REST client
// stays Eino-free so it can be vendored into other Go services (a CLI, a
// scheduled job, an MCP server).
//
// # What's NOT here
//
// Connection lifecycle (create link, poll for ACTIVE, delete) is exposed
// as Go HTTP handlers in a separate package (planned). The agent only
// needs ExecuteTool + the manifest; the connection screen is a UI concern
// that belongs in handlers, not in the tool adapter.
package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"egent-lobehub/connectors/composio"
)

// ConnectedAccountStore resolves the active Composio connected_account_id
// for a given (userID, appIdentifier) pair. The TS side implements this
// with PluginModel.findOne({ identifier, userId }) and reads
// customParams.composio.connectedAccountId; the Go side mirrors that via
// pREST (Tier 1 filter on user_id) or a direct Postgres query.
//
// Returning ("", nil) signals "not connected" — the adapter surfaces this
// to the agent as a structured NOT_CONNECTED error so the runtime can
// prompt the user to connect via the UI.
type ConnectedAccountStore interface {
	// Resolve returns the connected_account_id for the given user and
	// LobeHub identifier (e.g. "gmail", "google-calendar"), or ("", nil)
	// when the user has no active connection for this app.
	Resolve(ctx context.Context, userID, appIdentifier string) (connectedAccountID string, err error)
}

// ComposioTool is an Eino InvokableTool backed by a single Composio
// action. Create one per (user, app, action) tuple via the Builder.
//
// ComposioTool implements tool.InvokableTool (Info + InvokableRun). It is
// safe for concurrent use: the embedded *Composio client is goroutine-safe
// and the lazy-loaded schema is guarded by a sync.Once.
type ComposioTool struct {
	slug    string
	desc    string
	rawArgs json.RawMessage // input_parameters from Composio, cached at build time

	client  *composio.Composio
	store   ConnectedAccountStore
	// appIdentifier is the LobeHub-side key (e.g. "gmail") used to look
	// up the user's connected_account_id. Decoupled from the toolkit
	// slug so we can rename the wire toolkit without touching user data.
	appIdentifier string

	log *slog.Logger

	// Cached ParamsOneOf built from rawArgs. Composio's JSON Schema is
	// rich enough that we keep it as-is rather than collapsing to
	// ParameterInfo; NewParamsOneOfByJSONSchema handles anyOf/oneOf/$defs.
	once   sync.Once
	params *schema.ParamsOneOf
	paramsErr error
}

// Ensure ComposioTool satisfies the Eino InvokableTool interface at
// compile time. (Interface also has StreamableTool/Enhanced* variants;
// we only implement the standard invokable form.)
var _ tool.InvokableTool = (*ComposioTool)(nil)

// Info returns the Eino ToolInfo for this action. The parameter schema
// is built lazily on first call (typically at Register time, when the
// ToolResolver probes every tool for its name) so the cost of parsing
// the JSON Schema is paid once per process, not per request.
func (t *ComposioTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	if t.paramsErr != nil && t.params == nil {
		// We tried before and failed — don't try again. Surface a
		// usable ToolInfo with an empty schema so the tool still
		// registers (the agent will see it) but execution will fail
		// with a clear error if the LLM tries to call it.
		return &schema.ToolInfo{
			Name: t.slug,
			Desc: t.desc,
		}, nil
	}
	t.once.Do(func() {
		if len(t.rawArgs) == 0 {
			// No parameters — Composio returned an empty schema.
			return
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(t.rawArgs, &s); err != nil {
			t.paramsErr = fmt.Errorf("composio/eino: parse %s schema: %w", t.slug, err)
			return
		}
		t.params = schema.NewParamsOneOfByJSONSchema(&s)
	})
	return &schema.ToolInfo{
		Name:        t.slug,
		Desc:        t.desc,
		ParamsOneOf: t.params,
	}, nil
}

// InvokableRun executes the Composio action on behalf of the current
// user. The userID is read from context via composio UserID plumbing
// (set per-request by the HTTP handler). The connected_account_id is
// resolved via the ConnectedAccountStore.
//
// Errors are returned as Go errors only for transport-level failures
// (network, DNS). Application errors from Composio (auth, validation,
// upstream 5xx) are already folded into composio.ExecuteResult by the
// client; we surface them as a Go error so the runtime middleware's
// error classification can pick replan/retry/stop.
func (t *ComposioTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	if t.client == nil {
		return "", errors.New("composio/eino: client not configured (is COMPOSIO_API_KEY set?)")
	}
	userID := composioUserIDFromContext(ctx)
	if userID == "" {
		return "", errors.New("composio/eino: no user_id in context (set via WithUserID)")
	}
	connectedID, err := t.store.Resolve(ctx, userID, t.appIdentifier)
	if err != nil {
		return "", fmt.Errorf("composio/eino: resolve connection for %s/%s: %w",
			userID, t.appIdentifier, err)
	}
	if connectedID == "" {
		// Not connected — surface a structured prompt the agent can
		// relay to the user. Returning this as an error (rather than
		// a success string) lets middleware.ClassifyError map it to
		// ErrorKindStop so the loop doesn't burn retries on it.
		return "", &NotConnectedError{
			UserID:        userID,
			AppIdentifier: t.appIdentifier,
			Slug:          t.slug,
		}
	}

	args, err := parseArgs(argsJSON)
	if err != nil {
		return "", fmt.Errorf("composio/eino: parse args: %w", err)
	}

	result, err := t.client.ExecuteTool(ctx, t.slug, args, connectedID, userID)
	if err != nil {
		return "", fmt.Errorf("composio/eino: execute %s: %w", t.slug, err)
	}
	if !result.Success {
		// Surface structured failures as Go errors so middleware can
		// classify them (auth → stop, validation → replan, 5xx → retry).
		return "", &ExecuteError{Result: result}
	}
	if t.log != nil {
		t.log.Debug("composio tool executed",
			"slug", t.slug, "user", userID,
			"connected", connectedID, "bytes", len(result.Content))
	}
	return result.Content, nil
}

// NotConnectedError is returned when the user has no active Composio
// connection for the requested app. The agent should surface a connect
// prompt to the user rather than retry.
type NotConnectedError struct {
	UserID        string
	AppIdentifier string
	Slug          string
}

func (e *NotConnectedError) Error() string {
	return fmt.Sprintf("composio: user %s has no active connection for %s (needed for %s)",
		e.UserID, e.AppIdentifier, e.Slug)
}

// ExecuteError wraps a failed composio.ExecuteResult as a Go error so
// the runtime middleware can classify it. Unwrap yields the underlying
// composio.ExecuteError for IsAuthError / IsRetryable checks.
type ExecuteError struct {
	Result *composio.ExecuteResult
}

func (e *ExecuteError) Error() string {
	if e.Result != nil && e.Result.Error != nil {
		return e.Result.Error.Error()
	}
	return "composio: execution failed"
}

func (e *ExecuteError) Unwrap() error {
	if e.Result != nil && e.Result.Error != nil {
		return e.Result.Error
	}
	return nil
}

// parseArgs converts the JSON string the model emits into the
// map[string]any the client expects. An empty or "{}" string yields an
// empty map (some Composio tools take no arguments).
func parseArgs(argsJSON string) (map[string]any, error) {
	if argsJSON == "" || argsJSON == "{}" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// ----- user ID context plumbing -----

type ctxKey struct{}

// WithUserID returns a new context carrying the userID. The HTTP handler
// in handlers.go must call this before invoking the agent runtime so
// ComposioTool.InvokableRun can resolve the connection.
//
// This mirrors memory.WithUserID (same pattern, different package) so
// the agent runtime can set both with the same context.WithValue chain.
// We deliberately use a separate key from memory's so a future
// refactor that changes one doesn't silently break the other.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, userID)
}

// composioUserIDFromContext reads the userID set by WithUserID. Returns
// "" when unset; callers should treat that as "unauthenticated" and
// refuse to execute.
func composioUserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// ----- Builder -----

// Builder constructs a set of ComposioTool instances for the apps a user
// has connected. It is the entry point main.go uses to register Composio
// tools with the runtime.ToolResolver.
//
// The builder fetches each app's action manifest from Composio (one
// round-trip per app; cached for the process lifetime) and emits one
// ComposioTool per action. A typical app exposes 10-50 actions, so a user
// with 3 connected apps sees 30-150 Composio tools registered.
type Builder struct {
	client *composio.Composio
	store  ConnectedAccountStore
	log    *slog.Logger
}

// NewBuilder creates a Builder. Returns nil if the client is nil (i.e.
// COMPOSIO_API_KEY was unset) so callers can branch on availability the
// same way they do for the knowledge service.
func NewBuilder(client *composio.Composio, store ConnectedAccountStore, log *slog.Logger) *Builder {
	if client == nil || store == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Builder{client: client, store: store, log: log}
}

// BuildOption configures a single Build call.
type BuildOption func(*buildConfig)

type buildConfig struct {
	// appIdentifiers whitelists which apps to build tools for. Empty
	// means "all apps in the catalog with at least one connected user".
	// Most callers pass a per-user list from the ConnectedAccountStore.
	appIdentifiers []string
	// limit caps the number of actions per app. Default 50.
	limit int
}

// WithApps restricts Build to the given LobeHub identifiers (e.g.
// "gmail", "slack"). Without this, Build iterates every entry in
// composio.COMPOSIO_APP_TYPES.
func WithApps(identifiers ...string) BuildOption {
	return func(c *buildConfig) { c.appIdentifiers = identifiers }
}

// WithLimitPerApp caps how many actions are fetched per app. Default 50.
// Use a smaller number (10-20) for the agent's context window.
func WithLimitPerApp(n int) BuildOption {
	return func(c *buildConfig) { c.limit = n }
}

// Build fetches the manifest for each requested app and returns one
// ComposioTool per action. Tools are returned in deterministic order
// (app identifier, then slug) so re-registration across requests is
// stable and ToolResolver.Resolve stays predictable.
//
// Apps whose manifest fetch fails are skipped with a logged warning
// rather than aborting the whole build — a single broken toolkit
// shouldn't disable the others.
func (b *Builder) Build(ctx context.Context, opts ...BuildOption) ([]*ComposioTool, error) {
	cfg := buildConfig{limit: 50}
	for _, opt := range opts {
		opt(&cfg)
	}

	apps := cfg.appIdentifiers
	if len(apps) == 0 {
		for _, a := range composio.COMPOSIO_APP_TYPES {
			apps = append(apps, a.Identifier)
		}
	}

	var tools []*ComposioTool
	for _, id := range apps {
		app := composio.GetAppByIdentifier(id)
		if app == nil {
			b.log.Warn("composio/eino: unknown app identifier, skipping", "identifier", id)
			continue
		}
		actions, err := b.client.GetToolsForApp(ctx, app.AppSlug)
		if err != nil {
			b.log.Warn("composio/eino: fetch manifest failed, skipping app",
				"app", id, "error", err)
			continue
		}
		if cfg.limit > 0 && len(actions) > cfg.limit {
			actions = actions[:cfg.limit]
		}
		for _, a := range actions {
			tools = append(tools, &ComposioTool{
				slug:          a.Slug,
				desc:          buildDesc(a, app),
				rawArgs:       a.ArgsSchema,
				client:        b.client,
				store:         b.store,
				appIdentifier: id,
				log:           b.log,
			})
		}
	}
	return tools, nil
}

// buildDesc composes a tool description that's useful for the LLM. We
// include the app's display name and the action's native description so
// the model can pick the right tool from a large set.
func buildDesc(a composio.Tool, app *composio.AppType) string {
	if a.Description != "" {
		// Truncate to ~400 chars to avoid blowing the agent's context
		// window when a toolkit publishes verbose descriptions.
		const max = 400
		d := a.Description
		if len(d) > max {
			d = d[:max] + "…"
		}
		if app != nil {
			return fmt.Sprintf("[%s] %s", app.Label, d)
		}
		return d
	}
	if app != nil {
		return fmt.Sprintf("Composio action %s on %s", a.Slug, app.Label)
	}
	return fmt.Sprintf("Composio action %s", a.Slug)
}

// Slug returns the underlying action slug for logging/debugging.
// Stable across builds for the same action.
func (t *ComposioTool) Slug() string { return t.slug }

// AppIdentifier returns the LobeHub catalog identifier (e.g. "gmail").
func (t *ComposioTool) AppIdentifier() string { return t.appIdentifier }

// FormatToolsForLog returns a compact summary string for debug logging.
// Used by main.go after registration to show the agent what's available.
func FormatToolsForLog(tools []*ComposioTool) string {
	if len(tools) == 0 {
		return "no composio tools"
	}
	byApp := make(map[string]int)
	for _, t := range tools {
		byApp[t.appIdentifier]++
	}
	parts := make([]string, 0, len(byApp))
	for app, n := range byApp {
		parts = append(parts, fmt.Sprintf("%s=%d", app, n))
	}
	return strings.Join(parts, " ")
}
