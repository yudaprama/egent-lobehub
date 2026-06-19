package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"egent-lobehub/connectors/composio"
)

// fakeComposioServer stands up an httptest.Server that handles the two
// endpoints the handlers hit: /auth_configs POST + /connected_accounts/link
// POST. Other endpoints return 404.
func fakeComposioServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/auth_configs", func(w http.ResponseWriter, r *http.Request) {
		// Always "find" an existing config so we don't try to create.
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "auth_test", "name": "Test", "type": "use_composio_managed_auth", "toolkit": map[string]any{"slug": "gmail"}},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	mux.HandleFunc("/connected_accounts/link", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connected_account_id": "ca_test123",
			"redirect_url":         "https://connect.composio.dev/link/test",
		})
	})

	mux.HandleFunc("/connected_accounts/", func(w http.ResponseWriter, r *http.Request) {
		// GET single connection — return ACTIVE for the test
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "ca_test123",
			"status":  "ACTIVE",
			"toolkit": map[string]any{"slug": "gmail"},
		})
	})

	return srv
}

func TestComposioHandlers_CreateConnection_Success(t *testing.T) {
	srv := fakeComposioServer(t)
	c, err := composio.NewComposer("test-key", composio.WithBaseURL(srv.URL), composio.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	composioCli = c
	t.Cleanup(func() { composioCli = nil })

	body, _ := json.Marshal(map[string]any{
		"appSlug": "GMAIL", "identifier": "gmail", "label": "Gmail",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/composio/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-arch-actor-id", "user-1")
	rr := httptest.NewRecorder()

	composioCreateConnectionHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp composioCreateConnectionResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ConnectedAccountID != "ca_test123" {
		t.Errorf("ConnectedAccountID = %q", resp.ConnectedAccountID)
	}
	if resp.RedirectURL == "" {
		t.Error("RedirectURL is empty")
	}
	if resp.AuthConfigID != "auth_test" {
		t.Errorf("AuthConfigID = %q", resp.AuthConfigID)
	}
	// Verify state was stored
	if state := composioGetConn("ca_test123"); state == nil {
		t.Error("connection state not stored")
	}
}

func TestComposioHandlers_CreateConnection_WrongMethod(t *testing.T) {
	composioCli = nil
	req := httptest.NewRequest(http.MethodGet, "/v1/composio/connections", nil)
	rr := httptest.NewRecorder()
	composioCreateConnectionHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestComposioHandlers_CreateConnection_NoClient(t *testing.T) {
	composioCli = nil
	body, _ := json.Marshal(map[string]any{
		"appSlug": "GMAIL", "identifier": "gmail", "label": "Gmail",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/composio/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	composioCreateConnectionHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	// Body should be an error message.
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error"] == nil {
		t.Errorf("expected error in body, got %v", resp)
	}
}

func TestComposioHandlers_PollHandler_Success(t *testing.T) {
	srv := fakeComposioServer(t)
	c, err := composio.NewComposer("test-key", composio.WithBaseURL(srv.URL), composio.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	composioCli = c
	t.Cleanup(func() { composioCli = nil })

	// Seed state
	composioSetConn(&composioConnState{
		connectedAccountID: "ca_test123",
		appSlug:            "GMAIL",
		identifier:         "gmail",
		status:             "PENDING",
	})

	body, _ := json.Marshal(map[string]any{"connectedAccountId": "ca_test123"})
	req := httptest.NewRequest(http.MethodPost, "/v1/composio/connections/poll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	composioPollHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp composioGetConnectionResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ACTIVE" {
		t.Errorf("Status = %q", resp.Status)
	}
	// State should be updated
	if state := composioGetConn("ca_test123"); state.status != "ACTIVE" {
		t.Errorf("state.Status = %q, want ACTIVE", state.status)
	}
}

func TestComposioHandlers_DeleteHandler(t *testing.T) {
	composioCli = nil // delete works even without client (best-effort)
	composioSetConn(&composioConnState{connectedAccountID: "ca_to_delete"})
	body, _ := json.Marshal(map[string]any{"connectedAccountId": "ca_to_delete", "identifier": "gmail"})
	req := httptest.NewRequest(http.MethodPost, "/v1/composio/connections/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	composioDeleteConnectionHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	if composioGetConn("ca_to_delete") != nil {
		t.Error("connection should be deleted from state")
	}
}

func TestComposioHandlers_UpdatePlugin(t *testing.T) {
	composioCli = nil
	composioSetConn(&composioConnState{
		connectedAccountID: "ca_update",
		status:             "PENDING",
	})
	body, _ := json.Marshal(map[string]any{
		"appSlug":            "GMAIL",
		"authConfigId":       "auth_test",
		"connectedAccountId": "ca_update",
		"identifier":         "gmail",
		"label":              "Gmail",
		"status":             "ACTIVE",
		"toolsCount":         42,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/composio/plugins/update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	composioUpdatePluginHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	if state := composioGetConn("ca_update"); state.status != "ACTIVE" {
		t.Errorf("state.Status = %q, want ACTIVE", state.status)
	}
}

func TestComposioHandlers_OAuthCallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/composio/oauth/callback?status=success", nil)
	rr := httptest.NewRecorder()
	composioOAuthCallbackHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct == "" {
		t.Error("no Content-Type header")
	}
	body := rr.Body.String()
	if !contains(body, "Authorization complete") {
		t.Errorf("body missing success message: %s", body)
	}
}

func TestComposioHandlers_OAuthCallback_Failed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/composio/oauth/callback?status=failed", nil)
	rr := httptest.NewRecorder()
	composioOAuthCallbackHandler(rr, req)
	body := rr.Body.String()
	if !contains(body, "Authorization failed") {
		t.Errorf("body missing failure message: %s", body)
	}
}

func TestComposioHandlers_GetPlugins(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/composio/plugins", nil)
	rr := httptest.NewRecorder()
	composioGetPluginsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	var resp composioGetPluginsResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (len(substr) == 0 || stringContains(s, substr))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
