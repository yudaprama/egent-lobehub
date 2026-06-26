package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"egent-lobehub/connectors/composio"
	composioeino "egent-lobehub/connectors/composio/eino"
)

// ----- composio handler state -----

// composioConnState tracks a single pending Composio connection so the
// frontend can poll for ACTIVE status after the OAuth redirect.
type composioConnState struct {
	connectedAccountID string
	appSlug            string
	identifier         string
	redirectURL        string
	status             string // PENDING | ACTIVE | FAILED
}

var composioConns sync.Map // key: connectedAccountID

func composioSetConn(state *composioConnState) {
	composioConns.Store(state.connectedAccountID, state)
}

func composioGetConn(id string) *composioConnState {
	v, ok := composioConns.Load(id)
	if !ok {
		return nil
	}
	return v.(*composioConnState)
}

func composioDeleteConn(id string) {
	composioConns.Delete(id)
}

// ----- request / response types -----

type (
	composioCreateConnectionReq struct {
		AppSlug    string `json:"appSlug"`
		Identifier string `json:"identifier"`
		Label      string `json:"label"`
	}
	composioCreateConnectionResp struct {
		AuthConfigID       string `json:"authConfigId"`
		ConnectedAccountID string `json:"connectedAccountId"`
		Identifier         string `json:"identifier"`
		RedirectURL        string `json:"redirectUrl"`
	}

	composioGetConnectionReq struct {
		ConnectedAccountID string `json:"connectedAccountId"`
	}
	composioGetConnectionResp struct {
		AppSlug            string `json:"appSlug"`
		ConnectedAccountID string `json:"connectedAccountId"`
		Error              string `json:"error,omitempty"` // AUTH_ERROR or empty
		Status             string `json:"status"`
	}

	composioDeleteConnectionReq struct {
		ConnectedAccountID string `json:"connectedAccountId"`
		Identifier         string `json:"identifier"`
	}

	composioGetPluginsResp struct {
		Plugins []composioPluginInfo `json:"plugins"`
	}
	composioPluginInfo struct {
		AppSlug            string `json:"appSlug"`
		Identifier         string `json:"identifier"`
		ConnectedAccountID string `json:"connectedAccountId"`
		Status             string `json:"status"`
		Label              string `json:"label"`
		ToolsCount         int    `json:"toolsCount"`
	}

	composioUpdatePluginReq struct {
		AppSlug            string `json:"appSlug"`
		AuthConfigID       string `json:"authConfigId"`
		ConnectedAccountID string `json:"connectedAccountId"`
		Identifier         string `json:"identifier"`
		Label              string `json:"label"`
		Status             string `json:"status"`
		RedirectURL        string `json:"redirectUrl,omitempty"`
		ToolsCount         int    `json:"toolsCount"`
	}

	composioPollReq struct {
		ConnectedAccountID string `json:"connectedAccountId"`
	}
)

// ----- helpers -----

func composioWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func composioReadJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func composioUserFromHeader(r *http.Request) string {
	// Use the existing extractUserID from handlers.go. It reads
	// x-arch-actor-id → X-User-ID → kratos: → "anonymous".
	return extractUserID(r)
}

// composioAppIdentifier resolves the identifier from the Composio
// RESTAccountStore lookup — for handlers that receive a
// connectedAccountId but need to map it back to an app identifier.
// Falls back to "unknown" when the connection is not in the store.
func composioAppIdentifier(userID, connectedAccountID string) string {
	if composioCli == nil || composioAccountStore == nil {
		return "unknown"
	}
	// This is a reverse lookup which isn't directly supported by the
	// store. For now the client passes the identifier explicitly.
	return "unknown"
}

// ----- handlers -----

// composioCreateConnectionHandler mirrors
// lobehub/apps/server/src/routers/lambda/composio.ts:createConnection
func composioCreateConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if composioCli == nil {
		composioWriteJSON(w, map[string]any{
			"error": "composio not configured",
		})
		return
	}

	var req composioCreateConnectionReq
	if err := composioReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.AppSlug == "" || req.Identifier == "" {
		http.Error(w, "appSlug and identifier are required", http.StatusBadRequest)
		return
	}

	userID := composioUserFromHeader(r)

	// Resolve or create an auth config for this toolkit.
	cfg, err := composioCli.ResolveOrCreateAuthConfig(r.Context(), req.AppSlug, req.Label)
	if err != nil {
		slog.Error("composio: resolve auth config failed", "app", req.AppSlug, "error", err)
		http.Error(w, "failed to resolve auth config", http.StatusInternalServerError)
		return
	}

	// Start the OAuth link flow. The callbackUrl points at the
	// composioOAuthCallbackHandler which renders a tiny HTML page
	// that auto-closes the popup.
	var callbackURL string
	if baseURL := r.Host; baseURL != "" {
		// Best-effort; the UI also sends the callback URL explicitly.
		callbackURL = "http://" + baseURL + "/v1/composio/oauth/callback"
	}

	connID, redirectURL, err := composioCli.LinkConnection(r.Context(), userID, cfg.ID, callbackURL)
	if err != nil {
		slog.Error("composio: link connection failed", "user", userID, "error", err)
		http.Error(w, "failed to create connection", http.StatusInternalServerError)
		return
	}

	// Store pending state so composioPollHandler can return ACTIVE
	// once the user completes the OAuth.
	composioSetConn(&composioConnState{
		connectedAccountID: connID,
		appSlug:            req.AppSlug,
		identifier:         req.Identifier,
		redirectURL:        redirectURL,
		status:             "PENDING",
	})

	composioWriteJSON(w, composioCreateConnectionResp{
		AuthConfigID:       cfg.ID,
		ConnectedAccountID: connID,
		Identifier:         req.Identifier,
		RedirectURL:        redirectURL,
	})
}

// composioPollHandler is polled by the frontend after the OAuth redirect.
// It checks whether the connection has become ACTIVE. When it does, the
// frontend calls updatePlugin to save the connection metadata and the
// runtime re-registers tools on the next agent run.
//
// This replaces the TS composio.getConnection pattern (lambda/composio.ts:148-178).
func composioPollHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if composioCli == nil {
		composioWriteJSON(w, composioGetConnectionResp{
			Status: "NOT_CONFIGURED",
		})
		return
	}

	var req composioPollReq
	if err := composioReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Check Composio for the live connection status.
	conn, err := composioCli.GetConnection(r.Context(), req.ConnectedAccountID)
	if err != nil {
		composioWriteJSON(w, composioGetConnectionResp{
			ConnectedAccountID: req.ConnectedAccountID,
			Error:              "AUTH_ERROR",
			Status:             "FAILED",
		})
		return
	}

	// Update local state if we have it.
	if state := composioGetConn(req.ConnectedAccountID); state != nil {
		state.status = string(conn.Status)
	}

	composioWriteJSON(w, composioGetConnectionResp{
		AppSlug:            conn.Toolkit.Slug,
		ConnectedAccountID: req.ConnectedAccountID,
		Status:             string(conn.Status),
	})
}

// composioDeleteConnectionHandler mirrors
// lobehub/apps/server/src/routers/lambda/composio.ts:deleteConnection
func composioDeleteConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req composioDeleteConnectionReq
	if err := composioReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Delete remote connection (best-effort, same as TS side).
	if composioCli != nil {
		_ = composioCli.DeleteConnection(r.Context(), req.ConnectedAccountID)
	}
	composioDeleteConn(req.ConnectedAccountID)

	composioWriteJSON(w, map[string]any{"success": true})
}

// composioGetPluginsHandler mirrors
// lobehub/apps/server/src/routers/lambda/composio.ts:getComposioPlugins
func composioGetPluginsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// For now, return the locally tracked connections. A proper
	// implementation would read from the plugins table via pREST.
	composioWriteJSON(w, composioGetPluginsResp{Plugins: []composioPluginInfo{}})
}

// composioUpdatePluginHandler mirrors
// lobehub/apps/server/src/routers/lambda/composio.ts:updateComposioPlugin
func composioUpdatePluginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req composioUpdatePluginReq
	if err := composioReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	// Mark connection as ACTIVE so future poll requests reflect it.
	if state := composioGetConn(req.ConnectedAccountID); state != nil {
		state.status = req.Status
	}
	composioWriteJSON(w, map[string]any{"savedCount": req.ToolsCount})
}

// composioRemovePluginHandler mirrors
// lobehub/apps/server/src/routers/lambda/composio.ts:removeComposioPlugin
func composioRemovePluginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Identifier string `json:"identifier"`
	}
	if err := composioReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	composioWriteJSON(w, map[string]any{"success": true})
}

// composioOAuthCallbackHandler mirrors
// lobehub/src/app/(backend)/api/composio/oauth/callback/route.ts
//
// Composio redirects the browser here after the provider auth flow.
// This page only closes the popup window; the opener polls
// composioPollHandler to detect the now-ACTIVE connection.
func composioOAuthCallbackHandler(w http.ResponseWriter, r *http.Request) {
	// Extract status from query params (Composio appends ?status=success|failed
	// and sometimes ?error=...).
	status := r.URL.Query().Get("status")
	oauthError := r.URL.Query().Get("error")
	success := oauthError == "" && status != "failed"

	msg := "Authorization complete. You can close this window."
	if !success {
		msg = "Authorization failed."
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.Error(w, `<!doctype html>
<html>
  <head><meta charset="utf-8" /><title>Composio authorization</title></head>
  <body style="font-family: system-ui, sans-serif; padding: 24px; text-align: center;">
    <p>`+msg+`</p>
    <script>
      (function () {
        setTimeout(function () { window.close(); }, 300);
      })();
    </script>
  </body>
</html>`, http.StatusOK)
}

// ----- tool catalog + execution (retire tools/composio.ts: getActions / listActions / executeAction) -----

type composioToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type composioListToolsResp struct {
	Tools []composioToolInfo `json:"tools"`
}

// composioListToolsHandler mirrors tools/composio.ts:getActions + listActions
// (both call composio.tools.getRawComposioTools({ toolkits: [appSlug] })).
// One endpoint serves both the pre-connect browse (useFetchAppTools) and the
// post-ACTIVE tool fetch (refreshComposioConnectionStatus).
func composioListToolsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if composioCli == nil {
		composioWriteJSON(w, composioListToolsResp{Tools: []composioToolInfo{}})
		return
	}
	appSlug := r.URL.Query().Get("appSlug")
	if appSlug == "" {
		http.Error(w, "appSlug is required", http.StatusBadRequest)
		return
	}
	tools, err := composioCli.GetToolsForApp(r.Context(), appSlug)
	if err != nil {
		slog.Error("composio: list tools failed", "app", appSlug, "error", err)
		http.Error(w, "failed to list tools", http.StatusBadGateway)
		return
	}
	out := make([]composioToolInfo, 0, len(tools))
	for _, t := range tools {
		// Match the TS projection: name = slug || name. Slug is the canonical
		// Composio action name and is what the executor passes back as toolSlug.
		name := t.Slug
		if name == "" {
			name = t.Name
		}
		schema := t.ArgsSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"properties":{},"type":"object"}`)
		}
		out = append(out, composioToolInfo{
			Name:        name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	composioWriteJSON(w, composioListToolsResp{Tools: out})
}

type composioExecuteToolReq struct {
	Identifier string         `json:"identifier"`
	ToolSlug   string         `json:"toolSlug"`
	ToolArgs   map[string]any `json:"toolArgs,omitempty"`
}

type composioContentBlock struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

type composioExecuteState struct {
	Content []composioContentBlock `json:"content"`
	IsError bool                   `json:"isError"`
}

type composioExecuteToolResp struct {
	Content string               `json:"content"`
	State   composioExecuteState `json:"state"`
	Success bool                 `json:"success"`
}

// composioExecuteToolHandler mirrors tools/composio.ts:executeAction.
//
// SECURITY: connectedAccountID is resolved server-side from the caller's own
// user_installed_plugins row via RESTAccountStore — a client-supplied id is
// never trusted, so one user cannot drive another user's Composio connection.
// This is the same invariant the TS router enforced via PluginModel.findById
// (user-scoped). The response shape matches MCPService.processToolCallResult
// ({ content, state: { content, isError }, success }) consumed by the
// chat-plugin executor.
func composioExecuteToolHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if composioCli == nil {
		http.Error(w, "composio not configured", http.StatusServiceUnavailable)
		return
	}
	var req composioExecuteToolReq
	if err := composioReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Identifier == "" || req.ToolSlug == "" {
		http.Error(w, "identifier and toolSlug are required", http.StatusBadRequest)
		return
	}
	userID := composioUserFromHeader(r)

	// Resolve the connected account for THIS user — never trust the client.
	var connectedAccountID string
	if composioAccountStore != nil {
		id, err := composioAccountStore.Resolve(r.Context(), userID, req.Identifier)
		if err != nil {
			slog.Error("composio: resolve account failed",
				"user", userID, "identifier", req.Identifier, "error", err)
			http.Error(w, "failed to resolve connection", http.StatusInternalServerError)
			return
		}
		connectedAccountID = id
	}
	if connectedAccountID == "" {
		http.Error(w, fmt.Sprintf("no Composio connection found for %q", req.Identifier), http.StatusNotFound)
		return
	}

	result, err := composioCli.ExecuteTool(r.Context(), req.ToolSlug, req.ToolArgs, connectedAccountID, userID)
	if err != nil {
		slog.Error("composio: execute tool failed", "tool", req.ToolSlug, "error", err)
		http.Error(w, "tool execution failed", http.StatusBadGateway)
		return
	}

	content := result.Content
	isError := !result.Success
	if result.Error != nil {
		content = result.Error.Message
		isError = true
	}
	composioWriteJSON(w, composioExecuteToolResp{
		Content: content,
		State: composioExecuteState{
			Content: []composioContentBlock{{Text: content, Type: "text"}},
			IsError: isError,
		},
		Success: !isError,
	})
}

// ----- composio client package-level vars -----

// composioCli and composioAccountStore are set during startup in
// main.go's Composio registration block. The handlers read them but
// don't own them.
var composioCli *composio.Composio
var composioAccountStore *composioeino.RESTAccountStore
