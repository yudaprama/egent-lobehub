package composio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// ConnectionStatus is the lifecycle state of a Composio connected account.
// Mirrors the ConnectionStatuses enum in @composio/core.
type ConnectionStatus string

const (
	StatusActive    ConnectionStatus = "ACTIVE"
	StatusInitiated ConnectionStatus = "INITIATED"
	StatusPending   ConnectionStatus = "PENDING"
	StatusFailed    ConnectionStatus = "FAILED"
	StatusExpired   ConnectionStatus = "EXPIRED"
	StatusDeleted   ConnectionStatus = "DELETED"
)

// ConnectedAccount represents a composio connected account. Mirrors the
// shape returned by GET /api/v3.1/connected_accounts/{id} and
// /api/v3.1/connected_accounts (list). LobeHub stores the ID in
// PluginModel.customParams.composio.connectedAccountId and the toolkit
// slug is read from .toolkit.slug.
//
// Note: this is the v3.1 projection, not the legacy v1 shape
// (IntegrationID, AppUniqueID, …) that groq-go/extensions/composio
// currently exposes. v1 will sunset alongside /v1/* and /v2/* endpoints.
type ConnectedAccount struct {
	ID           string           `json:"id"`
	Nanoid       string           `json:"nanoid,omitempty"`
	Status       ConnectionStatus `json:"status"`
	Toolkit      Toolkit          `json:"toolkit"`
	UserID       string           `json:"user_id,omitempty"`
	AuthConfigID string           `json:"auth_config_id,omitempty"`
	// ErrorReason is populated by GetConnection when Status == FAILED
	// and the upstream returned 401 (e.g. "AUTH_ERROR" so the UI can
	// prompt re-auth). LobeHub uses the same synthetic value.
	ErrorReason string `json:"error_reason,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	// Disabled is true when the user or admin disabled the connection
	// via PATCH /connected_accounts/{id}/status.
	Disabled bool `json:"disabled,omitempty"`
}

// Toolkit is the embedded toolkit reference on ConnectedAccount, Tool,
// and AuthConfig. The shape is the same in v3.1 across all three.
type Toolkit struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
	// LogoURL is set on the list response but not on get-by-id; the
	// caller can fall back to GetAppBySlug(...).Icon.
	LogoURL string `json:"logo,omitempty"`
}

// rawConnectedAccount is the wire shape returned by the v3.1 endpoints.
// The API sometimes wraps the status inside connectionData.val.status
// (older responses) and sometimes at the top level (current). We accept
// both and project to the public ConnectedAccount.
type rawConnectedAccount struct {
	ID             string           `json:"id"`
	Nanoid         string           `json:"nanoid"`
	Status         ConnectionStatus `json:"status"`
	Toolkit        *Toolkit         `json:"toolkit"`
	UserID         string           `json:"user_id"`
	AuthConfigID   string           `json:"auth_config_id"`
	CreatedAt      string           `json:"created_at"`
	UpdatedAt      string           `json:"updated_at"`
	Disabled       bool             `json:"disabled"`
	ConnectionData *struct {
		Val struct {
			Status      ConnectionStatus `json:"status"`
			RedirectURL string           `json:"redirectUrl"`
		} `json:"val"`
	} `json:"connection_data"`
}

// toPublic normalises the raw wire shape to the export-facing struct.
// Centralised so that GetConnection, ListConnections, and LinkConnection
// can all share the same field selection rules.
func (r *rawConnectedAccount) toPublic() *ConnectedAccount {
	out := &ConnectedAccount{
		ID:           r.ID,
		Nanoid:       r.Nanoid,
		Status:       r.Status,
		UserID:       r.UserID,
		AuthConfigID: r.AuthConfigID,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		Disabled:     r.Disabled,
	}
	if r.Toolkit != nil {
		out.Toolkit = *r.Toolkit
	}
	if out.ID == "" {
		out.ID = r.Nanoid
	}
	if out.Status == "" && r.ConnectionData != nil {
		out.Status = r.ConnectionData.Val.Status
	}
	return out
}

// Authorizer is the public interface for connection-management methods.
// The interface lets tests inject a fake, mirroring the LobeHub convention
// (egent-lobehub/knowledge.KnowledgeBackend).
type Authorizer interface {
	// ListConnections returns connected accounts visible to the configured
	// project key, optionally filtered by user_uuid and showActiveOnly.
	ListConnections(ctx context.Context, opts ...AuthOption) ([]ConnectedAccount, error)
	// GetConnection returns the current state of a single connection.
	// Mirrors lobehub/apps/server/src/routers/lambda/composio.ts:148-178.
	// Maps 401 errors to a synthetic {Status: FAILED, ErrorReason: "AUTH_ERROR"}
	// so the agent runtime and the UI can both branch on the same code.
	GetConnection(ctx context.Context, id string) (*ConnectedAccount, error)
	// LinkConnection starts a hosted OAuth (or API-key) flow and returns
	// the new connected account id and the redirect URL the user should be
	// sent to. This is the modern (non-deprecated) entry point — the
	// legacy POST /connected_accounts rejects composio-managed auth
	// configs with a Sunset response (deprecation on 2026-07-03).
	LinkConnection(ctx context.Context, userID, authConfigID, callbackURL string) (string, string, error)
	// DeleteConnection removes the connected account from Composio. Best-
	// effort from the LobeHub side: the TS code logs and continues on
	// failure (lambda/composio.ts:132-136) so a stale local PluginModel
	// row can still be cleaned up locally.
	DeleteConnection(ctx context.Context, id string) error
}

// ListConnections returns connected accounts visible to the project key.
// Mirrors composio.connectedAccounts.list in @composio/core and
// GetConnectedAccounts in groq-go/extensions/composio (which targets the
// deprecated v1 API; we use v3.1 here).
func (c *Composio) ListConnections(
	ctx context.Context,
	opts ...AuthOption,
) ([]ConnectedAccount, error) {
	u, err := url.Parse(c.baseURL + "/connected_accounts")
	if err != nil {
		return nil, fmt.Errorf("composio: parse url: %w", err)
	}
	q := u.Query()
	for _, opt := range opts {
		opt(&q)
	}
	u.RawQuery = q.Encode()

	req, err := newJSONRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Items      []rawConnectedAccount `json:"items"`
		NextCursor string                `json:"next_cursor"`
	}
	if err := c.doRequest(req, &raw); err != nil {
		return nil, err
	}
	out := make([]ConnectedAccount, 0, len(raw.Items))
	for i := range raw.Items {
		out = append(out, *raw.Items[i].toPublic())
	}
	return out, nil
}

// linkRequest is the POST /connected_accounts/link body. All fields are
// snake_case to match the v3.1 API exactly.
type linkRequest struct {
	AuthConfigID string `json:"auth_config_id"`
	UserID       string `json:"user_id"`
	CallbackURL  string `json:"callback_url,omitempty"`
	Alias        string `json:"alias,omitempty"`
}

// linkResponse is the POST /connected_accounts/link response. The JS SDK
// at @composio/core/src/index.mjs:3237 uses the same field names.
type linkResponse struct {
	ConnectedAccountID string `json:"connected_account_id"`
	RedirectURL        string `json:"redirect_url"`
}

// LinkConnection starts a hosted OAuth (or API-key) flow for a user. The
// caller redirects the browser to redirectURL; on completion, Composio
// redirects to callbackURL and the connection is queryable via
// GetConnection. Returns the new connected account id and the redirect URL.
//
// This is the modern (non-deprecated) entry point. The legacy
// POST /connected_accounts endpoint rejects composio-managed auth configs
// with a Sunset response (deprecation on 2026-07-03); do not fall back to
// it. See @composio/core/src/index.mjs:3171 and 5388.
//
// Mirrors composio.connectedAccounts.link in the JS SDK and the
// createConnection procedure in
// lobehub/apps/server/src/routers/lambda/composio.ts:21-122.
func (c *Composio) LinkConnection(
	ctx context.Context,
	userID, authConfigID, callbackURL string,
) (string, string, error) {
	if userID == "" {
		return "", "", fmt.Errorf("composio: userID required")
	}
	if authConfigID == "" {
		return "", "", fmt.Errorf("composio: authConfigID required")
	}
	body := linkRequest{
		AuthConfigID: authConfigID,
		UserID:       userID,
		CallbackURL:  callbackURL,
	}
	req, err := newJSONRequest(ctx, http.MethodPost, c.baseURL+"/connected_accounts/link", body)
	if err != nil {
		return "", "", err
	}
	var raw linkResponse
	if err := c.doRequest(req, &raw); err != nil {
		return "", "", err
	}
	return raw.ConnectedAccountID, raw.RedirectURL, nil
}

// GetConnection returns the current state of a connected account. Use this
// after redirecting the user through LinkConnection to poll for ACTIVE.
//
// LobeHub's lambda/composio.ts:148-178 maps 401 errors to a synthetic
// error: "AUTH_ERROR" + status: "FAILED" so the UI can prompt re-auth. We
// return the same shape so the agent runtime and the UI can both branch
// on the same code.
func (c *Composio) GetConnection(ctx context.Context, id string) (*ConnectedAccount, error) {
	if id == "" {
		return nil, fmt.Errorf("composio: id required")
	}
	req, err := newJSONRequest(ctx, http.MethodGet, c.baseURL+"/connected_accounts/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var raw rawConnectedAccount
	err = c.doRequest(req, &raw)
	if err != nil {
		// Mirror LobeHub's AUTH_ERROR special case.
		var apiErr *APIError
		if errorsAs(err, &apiErr) && apiErr.IsAuthError() {
			return &ConnectedAccount{
				ID:          id,
				Status:      StatusFailed,
				ErrorReason: "AUTH_ERROR",
			}, nil
		}
		return nil, err
	}
	return raw.toPublic(), nil
}

// DeleteConnection removes a connected account from Composio. This is
// best-effort from the LobeHub side: the TS code logs and continues on
// failure (lambda/composio.ts:132-136) so a stale local PluginModel row
// can still be cleaned up locally. We mirror that by returning the error
// without wrapping it in an ExecuteResult — the caller decides.
func (c *Composio) DeleteConnection(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("composio: id required")
	}
	req, err := newJSONRequest(ctx, http.MethodDelete, c.baseURL+"/connected_accounts/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.doRequest(req, nil)
}

// errorsAs is a tiny indirection over errors.As so we can keep the package
// self-contained and trivially fakable in tests. It walks the Unwrap chain
// to find the first error that can be assigned to target.
func errorsAs(err error, target any) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if apiErr, ok := target.(*(*APIError)); ok {
			if e, isAPI := err.(*APIError); isAPI {
				*apiErr = e
				return true
			}
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// newJSONRequest builds an *http.Request with the auth header pre-set and
// the body JSON-encoded (if non-nil). The X-API-Key header is set here
// rather than in doRequest so any 401 from the dialer itself surfaces
// with a clear "composio: http:" error rather than a generic transport
// message.
func newJSONRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var rdr *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("composio: marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("composio: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
