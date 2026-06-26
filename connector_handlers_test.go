package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"egent-lobehub/keyvault"
)

// newTestKeyVault returns a 32-byte AES-256 keyvault for round-trip tests.
func newTestKeyVault(t *testing.T) *keyvault.Encryptor {
	t.Helper()
	kv, err := keyvault.NewFromRawKey(make([]byte, 32)) // all-zero key is fine for tests
	if err != nil {
		t.Fatal(err)
	}
	return kv
}

func postJSON(t *testing.T, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestConnectorEncryptDecrypt_RoundTrip(t *testing.T) {
	connectorKeyVault = newTestKeyVault(t)
	t.Cleanup(func() { connectorKeyVault = nil })

	creds := map[string]any{"token": "supersecret", "type": "bearer"}

	rr := httptest.NewRecorder()
	connectorEncryptCredentialsHandler(rr, postJSON(t, connectorEncryptReq{Credentials: creds}))
	if rr.Code != http.StatusOK {
		t.Fatalf("encrypt status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var enc connectorEncryptResp
	if err := json.Unmarshal(rr.Body.Bytes(), &enc); err != nil {
		t.Fatal(err)
	}
	if enc.Ciphertext == "" || enc.Ciphertext == `{"token":"supersecret","type":"bearer"}` {
		t.Fatalf("ciphertext missing or not encrypted: %q", enc.Ciphertext)
	}

	rr2 := httptest.NewRecorder()
	connectorDecryptCredentialsHandler(rr2, postJSON(t, connectorDecryptReq{Ciphertext: enc.Ciphertext}))
	if rr2.Code != http.StatusOK {
		t.Fatalf("decrypt status = %d, body = %s", rr2.Code, rr2.Body.String())
	}
	var dec connectorDecryptResp
	if err := json.Unmarshal(rr2.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	got, _ := dec.Credentials.(map[string]any)
	if got["token"] != "supersecret" || got["type"] != "bearer" {
		t.Errorf("decrypted credentials = %#v, want the original bearer token", dec.Credentials)
	}
}

func TestConnectorEncrypt_FailOpenWhenNoKeyVault(t *testing.T) {
	connectorKeyVault = nil // disabled → passthrough
	t.Cleanup(func() { connectorKeyVault = nil })

	rr := httptest.NewRecorder()
	connectorEncryptCredentialsHandler(rr, postJSON(t, connectorEncryptReq{
		Credentials: map[string]any{"apiKey": "x", "type": "apikey"},
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var enc connectorEncryptResp
	_ = json.Unmarshal(rr.Body.Bytes(), &enc)
	// Fail-open returns the JSON-stringified plaintext, not ciphertext.
	if enc.Ciphertext == "" {
		t.Error("expected passthrough plaintext, got empty")
	}
}

func TestConnectorDecrypt_EmptyCiphertextReturnsEmpty(t *testing.T) {
	connectorKeyVault = newTestKeyVault(t)
	t.Cleanup(func() { connectorKeyVault = nil })

	rr := httptest.NewRecorder()
	connectorDecryptCredentialsHandler(rr, postJSON(t, connectorDecryptReq{Ciphertext: ""}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var dec connectorDecryptResp
	if err := json.Unmarshal(rr.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec.Credentials != nil {
		t.Errorf("expected nil credentials, got %#v", dec.Credentials)
	}
}

func TestConnectorEncrypt_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	connectorEncryptCredentialsHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestConnectorDecrypt_GarbageCiphertext(t *testing.T) {
	connectorKeyVault = newTestKeyVault(t)
	t.Cleanup(func() { connectorKeyVault = nil })

	rr := httptest.NewRecorder()
	connectorDecryptCredentialsHandler(rr, postJSON(t, connectorDecryptReq{Ciphertext: "not-valid-ciphertext"}))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for undecryptable ciphertext", rr.Code)
	}
}
