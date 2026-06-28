package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Per-user API key management. The SPA, after authenticating (Kratos session →
// X-User-Id injected by the Oathkeeper edge), lets the user mint their OWN Talos
// gateway key here. The key is then used by the browser as `Authorization:
// Bearer <key>` directly against Plano's CORS-enabled model/agent listeners.
//
// Each key is bound to the user's identity as its Talos `actor_id`, so billing
// and any leak are scoped to that single user. Issue/list/revoke proxy to the
// Talos admin API (NOT exposed to the browser — only reachable server-side).

// talosAdminURL returns the Talos admin base URL (issue/list/revoke keys).
func talosAdminURL() string {
	if v := os.Getenv("TALOS_ADMIN_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:4420"
}

// defaultKeyTTL is how long a user-minted key lives unless overridden.
const defaultKeyTTL = "720h" // 30 days

// issuedAPIKey mirrors the Talos admin IssuedApiKey resource (secret omitted).
type issuedAPIKey struct {
	KeyID      string `json:"key_id"`
	Name       string `json:"name"`
	ActorID    string `json:"actor_id"`
	Status     string `json:"status"`
	CreateTime string `json:"create_time"`
	ExpireTime string `json:"expire_time"`
}

// keysHandler serves GET /v1/keys (list the caller's keys) and
// POST /v1/keys (issue a new key for the caller).
func keysHandler(w http.ResponseWriter, r *http.Request) {
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		listKeys(w, userID)
	case http.MethodPost:
		issueKey(w, r, userID)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func issueKey(w http.ResponseWriter, r *http.Request, userID string) {
	var body struct {
		Name string `json:"name"`
		TTL  string `json:"ttl"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	}
	if body.Name == "" {
		body.Name = "web-app"
	}
	if body.TTL == "" {
		body.TTL = defaultKeyTTL
	}

	reqBody, _ := json.Marshal(map[string]string{
		"actor_id": userID,
		"name":     body.Name,
		"ttl":      body.TTL,
	})
	resp, err := talosDo(http.MethodPost, talosAdminURL()+"/v2alpha1/admin/issuedApiKeys", reqBody)
	if err != nil {
		slog.Error("issue key: talos call failed", "err", err, "user", userID)
		writeJSONError(w, http.StatusBadGateway, "could not issue key")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		slog.Error("issue key: talos non-2xx", "status", resp.StatusCode, "body", string(b))
		writeJSONError(w, http.StatusBadGateway, "could not issue key")
		return
	}

	var issued struct {
		IssuedAPIKey issuedAPIKey `json:"issued_api_key"`
		Secret       string       `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
		writeJSONError(w, http.StatusBadGateway, "bad talos response")
		return
	}
	// The secret is returned ONCE — the client must store it now.
	writeJSON(w, http.StatusOK, map[string]any{
		"keyId":      issued.IssuedAPIKey.KeyID,
		"name":       issued.IssuedAPIKey.Name,
		"secret":     issued.Secret,
		"expireTime": issued.IssuedAPIKey.ExpireTime,
	})
}

func listKeys(w http.ResponseWriter, userID string) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf("actor_id=%q", userID))
	resp, err := talosDo(http.MethodGet, talosAdminURL()+"/v2alpha1/admin/issuedApiKeys?"+q.Encode(), nil)
	if err != nil {
		slog.Error("list keys: talos call failed", "err", err, "user", userID)
		writeJSONError(w, http.StatusBadGateway, "could not list keys")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSONError(w, http.StatusBadGateway, "could not list keys")
		return
	}
	var listed struct {
		IssuedAPIKeys []issuedAPIKey `json:"issued_api_keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		writeJSONError(w, http.StatusBadGateway, "bad talos response")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": listed.IssuedAPIKeys})
}

// keysRevokeHandler serves POST /v1/keys/revoke — revokes a key the caller owns.
func keysRevokeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		KeyID string `json:"keyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.KeyID == "" {
		writeJSONError(w, http.StatusBadRequest, "keyId required")
		return
	}

	// Ownership check: the key must belong to this user before we revoke it,
	// otherwise a user could revoke anyone's key by guessing key ids.
	if !keyBelongsToUser(body.KeyID, userID) {
		writeJSONError(w, http.StatusNotFound, "key not found")
		return
	}

	reqBody, _ := json.Marshal(map[string]string{"reason": "REVOCATION_REASON_UNSPECIFIED"})
	endpoint := fmt.Sprintf("%s/v2alpha1/admin/issuedApiKeys/%s:revoke", talosAdminURL(), url.PathEscape(body.KeyID))
	resp, err := talosDo(http.MethodPost, endpoint, reqBody)
	if err != nil {
		slog.Error("revoke key: talos call failed", "err", err, "user", userID)
		writeJSONError(w, http.StatusBadGateway, "could not revoke key")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSONError(w, http.StatusBadGateway, "could not revoke key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": body.KeyID})
}

// keyBelongsToUser reports whether keyID is an issued key whose actor_id matches
// userID, by listing the user's keys (the admin API filters server-side).
func keyBelongsToUser(keyID, userID string) bool {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf("actor_id=%q", userID))
	resp, err := talosDo(http.MethodGet, talosAdminURL()+"/v2alpha1/admin/issuedApiKeys?"+q.Encode(), nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false
	}
	var listed struct {
		IssuedAPIKeys []issuedAPIKey `json:"issued_api_keys"`
	}
	if json.NewDecoder(resp.Body).Decode(&listed) != nil {
		return false
	}
	for _, k := range listed.IssuedAPIKeys {
		if k.KeyID == keyID {
			return true
		}
	}
	return false
}

// talosDo performs an HTTP request to the Talos admin API.
func talosDo(method, endpoint string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return client.Do(req)
}

// writeJSONError writes a JSON {"error": msg} body. (writeJSON lives in db_helpers.go.)
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
