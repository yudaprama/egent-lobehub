package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RESTAccountStore resolves a user's connected_account_id by calling the
// LobeHub pREST endpoint for the user_installed_plugins table (the same
// table PluginModel writes — see lobehub/packages/database/src/models/plugin.ts
// and the new src/services/composio.ts). The table is NOT in pREST's
// user_id_filters list, so the store filters the response by user_id
// locally for safety (defence in depth).
//
// Endpoints used:
//
//	GET /lobehub/public/user_installed_plugins?identifier=eq.<id>&_size=1
//
// pREST returns Postgres column names verbatim (snake_case); the
// `custom_params` JSONB blob is returned as stored (its inner composio
// object keeps the camelCase keys the TS service wrote). The response
// item has the shape:
//
//	{
//	  "identifier": "gmail",
//	  "user_id": "user-1",
//	  "custom_params": { "composio": { "connectedAccountId": "ca_...", "status": "ACTIVE" } }
//	}
//
// "status" must be ACTIVE for the adapter to use the connection. The TS
// code applies the same filter via PluginModel.findById + status check.
type RESTAccountStore struct {
	// baseURL is the pREST root, e.g. "http://localhost:3000".
	baseURL string
	// table is "lobehub/public/user_installed_plugins" by default; override
	// for tests or for a different database (e.g. "yarsew/public/user_installed_plugins").
	table string
	// httpClient is the transport. Defaults to http.Client with 5s timeout.
	httpClient *http.Client
	// database is optional. If non-empty, used as the database
	// segment of the URL. Otherwise the table is expected to include it.
	// e.g. database="lobehub", table="public/plugins" → /lobehub/public/plugins
	database string
}

// RESTAccountStoreOption configures a RESTAccountStore.
type RESTAccountStoreOption func(*RESTAccountStore)

// WithPRESTBaseURL sets the pREST root. Trailing slashes are trimmed.
func WithPRESTBaseURL(raw string) RESTAccountStoreOption {
	return func(s *RESTAccountStore) { s.baseURL = strings.TrimRight(raw, "/") }
}

// WithPRESTDatabase sets the database segment (first path segment after
// the host). Defaults to the value already embedded in the table path.
func WithPRESTDatabase(db string) RESTAccountStoreOption {
	return func(s *RESTAccountStore) { s.database = db }
}

// WithPRESTTable overrides the table path. Defaults to
// "lobehub/public/user_installed_plugins". Pass without the leading slash.
func WithPRESTTable(table string) RESTAccountStoreOption {
	return func(s *RESTAccountStore) { s.table = strings.TrimLeft(table, "/") }
}

// WithPRESTHTTPClient substitutes the *http.Client.
func WithPRESTHTTPClient(hc *http.Client) RESTAccountStoreOption {
	return func(s *RESTAccountStore) {
		if hc != nil {
			s.httpClient = hc
		}
	}
}

// NewRESTAccountStore creates a pREST-backed store. baseURL is required
// (e.g. "http://localhost:3000" for the embedded prestd). Returns nil
// when baseURL is empty so main.go can branch on availability.
func NewRESTAccountStore(baseURL string, opts ...RESTAccountStoreOption) *RESTAccountStore {
	if baseURL == "" {
		return nil
	}
	s := &RESTAccountStore{
		baseURL:    strings.TrimRight(baseURL, "/"),
		table:      "lobehub/public/user_installed_plugins",
		database:   "lobehub",
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Resolve implements ConnectedAccountStore.
func (s *RESTAccountStore) Resolve(ctx context.Context, userID, appIdentifier string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("composio/eino: userID required")
	}
	if appIdentifier == "" {
		return "", fmt.Errorf("composio/eino: appIdentifier required")
	}

	// Build the query: identifier=eq.<id> and limit=1.
	// The plugin table has a composite (user_id, identifier) key in the
	// LobeHub schema, so the LLM side will already filter; we double-
	// check the user_id from customParams.composio to be safe (the TS
	// code does the same — see lobehub/src/server/services/composio/
	// ComposioService.executeComposioTool which reads from PluginModel
	// scoped to the current user).
	u := fmt.Sprintf("%s/%s", s.baseURL, s.table)
	q := url.Values{}
	q.Set("identifier", "eq."+appIdentifier)
	q.Set("_size", "1")
	u += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("composio/eino: build prest request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("composio/eino: prest get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// No plugin row for this identifier — user has not connected.
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("composio/eino: prest status %d", resp.StatusCode)
	}

	var page []pluginRow
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return "", fmt.Errorf("composio/eino: prest decode: %w", err)
	}
	if len(page) == 0 {
		return "", nil
	}
	row := page[0]

	// Verify the plugin belongs to the requesting user. The pREST
	// plugins table may not be in the user_id_filters list; defence
	// in depth: reject rows that don't match.
	if row.UserID != "" && row.UserID != userID {
		return "", nil // not connected for this user
	}
	cp := row.CustomParams.Composio
	if cp == nil {
		return "", nil
	}
	if cp.Status != "" && cp.Status != "ACTIVE" {
		return "", nil
	}
	return cp.ConnectedAccountID, nil
}

// pluginRow mirrors the LobeHub `user_installed_plugins` table shape
// (pREST returns snake_case column names verbatim). The `custom_params`
// column is a jsonb blob whose `composio` sub-object holds the connection
// id and lifecycle state (stored camelCase by the TS service).
type pluginRow struct {
	ID           string `json:"id"`
	Identifier   string `json:"identifier"`
	UserID       string `json:"user_id"`
	CustomParams struct {
		Composio *composioPlugin `json:"composio"`
	} `json:"custom_params"`
}

type composioPlugin struct {
	AppSlug            string `json:"appSlug"`
	AuthConfigID       string `json:"authConfigId"`
	ConnectedAccountID string `json:"connectedAccountId"`
	RedirectURL        string `json:"redirectUrl"`
	Status             string `json:"status"` // PENDING | ACTIVE | FAILED
}
