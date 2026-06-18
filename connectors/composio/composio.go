package composio

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// DefaultBaseURL is the Composio backend REST root. Override with
	// WithBaseURL for self-hosted or staging deploys.
	//
	// We target v3.1 because @composio/core 0.10.x also targets v3.1, and
	// the public docs site only describes v3.1 endpoints. v3 is still
	// callable but most new features land on v3.1 first.
	DefaultBaseURL = "https://backend.composio.dev/api/v3.1"

	// APIHeader is the project-scope auth header. Organizations use
	// x-org-api-key instead — not covered here because LobeHub only ever
	// provisions project keys via COMPOSIO_API_KEY.
	APIHeader = "x-api-key"
)

// Composio is a thread-safe REST client for the Composio platform
// (https://composio.dev), used by egent-lobehub to expose 250+ third-party
// SaaS tools (Slack, Gmail, GitHub, Notion, Linear, etc.) to LobeHub agents
// without per-app OAuth, manifest, or SDK work.
//
// Composio ships no official Go SDK; @composio/core (v0.10.0, TypeScript) is
// a thin wrapper around the same REST API this client calls directly. The
// endpoints covered here mirror what the LobeHub TypeScript integration
// uses (lobehub/src/server/services/composio/, lobehub/apps/server/src/routers/{lambda,tools}/composio.ts)
// so existing LobeHub connections and plugin state keep working when this
// client is swapped in behind the same handlers.
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
//
// # Relationship to the rest of egent-lobehub
//
// Composio is intentionally a pure REST client with no dependency on Eino,
// the runtime, or Postgres. A separate adapter (planned, see
// LOBEHUB_BACKEND_DATABASE_MAP.md §0 Tier 3) will wrap each connection's
// toolset as Eino tools and register them through runtime.ToolResolver.
type Composio struct {
	apiKey  string
	client  *http.Client
	logger  *slog.Logger
	baseURL string
}

// NewComposer creates a new Composio client. The apiKey is required; pass an
// empty string only if you also want to disable the integration. Returns
// (nil, nil) on empty apiKey so callers can branch on availability with
// the same idiom as egent-lobehub/knowledge.NewService.
func NewComposer(apiKey string, opts ...Option) (*Composio, error) {
	if apiKey == "" {
		return nil, nil
	}
	c := &Composio{
		apiKey:  apiKey,
		baseURL: DefaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// doRequest is the single HTTP entry point. It signs the request, decodes
// the JSON response, and converts non-2xx into *APIError.
//
// `v` may be nil (e.g. for DELETE), a *string (raw body decoded as text),
// or any other type decoded with json.NewDecoder. Mirrors the dispatch
// pattern in groq-go/extensions/composio/composio.go:50-85.
func (c *Composio) doRequest(req *http.Request, v any) error {
	req.Header.Set(APIHeader, c.apiKey)
	req.Header.Set("Accept", "application/json")
	if ct := req.Header.Get("Content-Type"); ct == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("composio: http: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return parseAPIError(res.StatusCode, body)
	}
	if v == nil {
		return nil
	}
	switch o := v.(type) {
	case *string:
		b, err := io.ReadAll(io.LimitReader(res.Body, 4096))
		if err != nil {
			return fmt.Errorf("composio: read body: %w", err)
		}
		*o = string(b)
		return nil
	default:
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			return fmt.Errorf("composio: decode response: %w (body=%q)", err, truncate(string(body), 256))
		}
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
