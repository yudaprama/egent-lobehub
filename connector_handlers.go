package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"egent-lobehub/keyvault"
)

// connectorKeyVault is the at-rest encryptor for connector credentials. It is
// the same instance the runtime uses (KEY_VAULTS_SECRET), assigned in main.go
// after the runtime is created. nil means encryption is disabled — the
// keyvault fail-opens (Encrypt/Decrypt are passthroughs), matching the TS
// behaviour when KEY_VAULTS_SECRET is unset.
//
// Why egent only does crypto (not CRUD): per lobehub AGENTS.md rule #1, simple
// CRUD must go through pREST in the frontend service. The ONLY part of
// connector create/getForEdit/update that pREST cannot do is AES-GCM
// encrypt/decrypt of the `credentials` column. So egent exposes two pure
// crypto endpoints and the frontend orchestrates pREST read/write around them
// — the same split the composio pilot used (egent OAuth + pREST persistence).
var connectorKeyVault *keyvault.Encryptor

type (
	// connectorEncryptReq carries the plaintext credentials object
	// (bearer/apikey/header). JSON-stringified then encrypted, mirroring the
	// TS ConnectorModel which stored encrypt(JSON.stringify(credentials)).
	connectorEncryptReq struct {
		Credentials any `json:"credentials"`
	}
	connectorEncryptResp struct {
		Ciphertext string `json:"ciphertext"`
	}
	connectorDecryptReq struct {
		Ciphertext string `json:"ciphertext"`
	}
	// connectorDecryptResp returns the parsed credentials object, or the raw
	// plaintext string when the stored value was not valid JSON.
	connectorDecryptResp struct {
		Credentials any `json:"credentials"`
	}
)

// connectorEncryptCredentialsHandler mirrors the encrypt half of
// apps/server/src/database/models/connector ConnectorModel.create/update.
// The frontend stores the returned ciphertext in user_connectors.credentials.
//
// POST /v1/connector/credentials/encrypt  { "credentials": {...} }
//   → { "ciphertext": "<base64-tink-or-plaintext-if-disabled>" }
func connectorEncryptCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req connectorEncryptReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	plaintext, err := json.Marshal(req.Credentials)
	if err != nil {
		http.Error(w, "failed to encode credentials", http.StatusBadRequest)
		return
	}
	ct, err := connectorKeyVault.EncryptString(string(plaintext))
	if err != nil {
		slog.Error("connector: encrypt credentials failed", "error", err)
		http.Error(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, connectorEncryptResp{Ciphertext: ct})
}

// connectorDecryptCredentialsHandler mirrors the decrypt half of
// ConnectorModel (getForEdit reads the ciphertext and decrypts it so the edit
// form can pre-fill user-set credentials). The frontend later strips OAuth2
// tokens + oidcConfig.clientSecret before rendering.
//
// POST /v1/connector/credentials/decrypt  { "ciphertext": "..." }
//   → { "credentials": {...} | "raw-string" }
//
// Returns an empty credentials field when the caller passes no ciphertext
// (a connector with no stored credentials).
func connectorDecryptCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req connectorDecryptReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if req.Ciphertext == "" {
		writeJSON(w, http.StatusOK, connectorDecryptResp{})
		return
	}
	pt, err := connectorKeyVault.DecryptString(req.Ciphertext)
	if err != nil {
		slog.Error("connector: decrypt credentials failed", "error", err)
		http.Error(w, "decryption failed", http.StatusBadGateway)
		return
	}
	var creds any
	if err := json.Unmarshal([]byte(pt), &creds); err != nil {
		// Not JSON (e.g. fail-open dev mode stored a raw token) — return as-is.
		creds = pt
	}
	writeJSON(w, http.StatusOK, connectorDecryptResp{Credentials: creds})
}
