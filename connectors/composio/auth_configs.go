package composio

import (
	"context"
	"fmt"
	"net/http"
)

// AuthConfig represents a Composio auth configuration. Auth configs decide
// how users authenticate to a toolkit (OAuth, API key, basic, etc.) and
// which scopes/redirect URLs are in play.
//
// Mirrors lobehub/apps/server/src/routers/lambda/composio.ts:39-52 where
// the router lists configs to find an existing one for a toolkit before
// creating a fresh composio-managed one.
type AuthConfig struct {
	// ID is the nanoid Composio assigns (e.g. "auth_abc123").
	ID string `json:"id"`
	// Name is the human-readable name shown in the Composio dashboard.
	Name string `json:"name,omitempty"`
	// Type is "use_composio_managed_auth" (default) or "use_custom_auth".
	Type string `json:"type,omitempty"`
	// Toolkit is the toolkit this config authenticates against.
	Toolkit Toolkit `json:"toolkit,omitempty"`
	// AuthScheme is the auth scheme (e.g. "oauth2", "api_key", "basic").
	// Only set for use_custom_auth configs.
	AuthScheme string `json:"auth_scheme,omitempty"`
	// IsEnabledForRouter is true when this config can be used by the
	// Composio tool router sessions (not yet wired in egent-lobehub).
	IsEnabledForRouter bool `json:"is_enabled_for_tool_router,omitempty"`
}

type rawAuthConfig struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	Toolkit            *Toolkit `json:"toolkit"`
	AuthScheme         string   `json:"auth_scheme"`
	IsEnabledForRouter bool     `json:"is_enabled_for_tool_router"`
}

func (r *rawAuthConfig) toPublic() *AuthConfig {
	out := &AuthConfig{
		ID:                 r.ID,
		Name:               r.Name,
		Type:               r.Type,
		AuthScheme:         r.AuthScheme,
		IsEnabledForRouter: r.IsEnabledForRouter,
	}
	if r.Toolkit != nil {
		out.Toolkit = *r.Toolkit
	}
	return out
}

// authConfigCreateRequest is the body of POST /auth_configs.
// Only the composio-managed and api_key variants are surfaced here; the
// TypeScript SDK supports the full range (proxy_config, tool_access_config,
// OAuth field overrides) but LobeHub only ever uses managed auth in
// practice.
type authConfigCreateRequest struct {
	Toolkit struct {
		Slug string `json:"slug"`
	} `json:"toolkit"`
	AuthConfig authConfigFields `json:"auth_config"`
}

type authConfigFields struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	AuthScheme  string         `json:"authScheme,omitempty"`
	Credentials map[string]any `json:"credentials,omitempty"`
}

type authConfigCreateResponse struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Toolkit *Toolkit `json:"toolkit"`
}

// ListAuthConfigs returns the project's auth configurations. The LobeHub
// integration calls this on connection create to find an existing config
// for a toolkit before creating a fresh one
// (lobehub/apps/server/src/routers/lambda/composio.ts:41-44).
//
// Mirrors composio.authConfigs.list() in @composio/core.
func (c *Composio) ListAuthConfigs(ctx context.Context) ([]AuthConfig, error) {
	req, err := newJSONRequest(ctx, http.MethodGet, c.baseURL+"/auth_configs", nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Items []rawAuthConfig `json:"items"`
	}
	if err := c.doRequest(req, &raw); err != nil {
		return nil, err
	}
	out := make([]AuthConfig, 0, len(raw.Items))
	for i := range raw.Items {
		out = append(out, *raw.Items[i].toPublic())
	}
	return out, nil
}

// CreateManagedAuthConfig creates a new auth config for a toolkit with
// type=use_composio_managed_auth. Composio then handles the OAuth flow,
// refresh tokens, and credential storage — the caller does not see the
// user-facing access/refresh tokens, which keeps them off our Postgres
// tables and shrinks the KeyVaultsGateKeeper surface area.
//
// This is the path LobeHub's lambda/composio.ts:46-49 falls back to when
// no env-pinned COMPOSIO_AUTH_CONFIG_IDS entry matches the toolkit.
//
// toolkitSlug is normalised to upper-snake before the request so callers
// can pass either "github" or "GITHUB". name defaults to the slug if empty.
func (c *Composio) CreateManagedAuthConfig(ctx context.Context, toolkitSlug, name string) (*AuthConfig, error) {
	if toolkitSlug == "" {
		return nil, fmt.Errorf("composio: toolkitSlug required")
	}
	if name == "" {
		name = toolkitSlug
	}
	body := authConfigCreateRequest{}
	body.Toolkit.Slug = NormaliseSlug(toolkitSlug)
	body.AuthConfig.Type = "use_composio_managed_auth"
	body.AuthConfig.Name = name

	req, err := newJSONRequest(ctx, http.MethodPost, c.baseURL+"/auth_configs", body)
	if err != nil {
		return nil, err
	}
	var raw authConfigCreateResponse
	if err := c.doRequest(req, &raw); err != nil {
		return nil, err
	}
	out := &AuthConfig{
		ID:   raw.ID,
		Name: raw.Name,
		Type: raw.Type,
	}
	if raw.Toolkit != nil {
		out.Toolkit = *raw.Toolkit
	}
	return out, nil
}

// FindAuthConfigForToolkit returns the first auth config whose toolkit slug
// matches (case-insensitively). Returns (nil, nil) if not found. The
// LobeHub router uses the same lookup-then-create pattern
// (lambda/composio.ts:39-52); we expose the lookup as a public helper so
// the caller can decide whether to create a fresh config or fail.
func (c *Composio) FindAuthConfigForToolkit(ctx context.Context, toolkitSlug string) (*AuthConfig, error) {
	configs, err := c.ListAuthConfigs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range configs {
		if equalFoldASCII(configs[i].Toolkit.Slug, toolkitSlug) {
			return &configs[i], nil
		}
	}
	return nil, nil
}

// ResolveOrCreateAuthConfig implements the lookup-then-create pattern used
// by lobehub/apps/server/src/routers/lambda/composio.ts:39-52 in a single
// call: find an existing config for the toolkit, or create a fresh
// composio-managed one. Returns the resolved auth config id.
//
// Callers that want to override the auto-create behaviour (e.g. use a
// pre-pinned COMPOSIO_AUTH_CONFIG_IDS value) should bypass this and call
// ListAuthConfigs + CreateManagedAuthConfig directly.
func (c *Composio) ResolveOrCreateAuthConfig(ctx context.Context, toolkitSlug, name string) (*AuthConfig, error) {
	if existing, err := c.FindAuthConfigForToolkit(ctx, toolkitSlug); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	return c.CreateManagedAuthConfig(ctx, toolkitSlug, name)
}
