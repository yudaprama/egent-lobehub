package main

import (
	"net/http/httptest"
	"testing"
)

func TestExtractUserID_XUserIDHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-User-ID", "user-42")
	got := extractUserID(r)
	if got != "user-42" {
		t.Errorf("expected user-42, got %q", got)
	}
}

func TestExtractUserID_KratosToken(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "kratos:session-token-abc")
	got := extractUserID(r)
	if got != "session-token-abc" {
		t.Errorf("expected session-token-abc, got %q", got)
	}
}

func TestExtractUserID_DefaultsToAnonymous(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	got := extractUserID(r)
	if got != "anonymous" {
		t.Errorf("expected anonymous, got %q", got)
	}
}

func TestExtractUserID_HeaderPriority(t *testing.T) {
	// X-User-ID should win over Authorization
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-User-ID", "header-user")
	r.Header.Set("Authorization", "kratos:token-user")
	got := extractUserID(r)
	if got != "header-user" {
		t.Errorf("expected X-User-ID to take priority, got %q", got)
	}
}

func TestExtractUserID_IgnoresNonKratosAuth(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer some-jwt-token")
	got := extractUserID(r)
	if got != "anonymous" {
		t.Errorf("expected anonymous (Bearer not supported yet), got %q", got)
	}
}

// TestExtractUserID_PlanoProxyChain documents the header passthrough contract:
// Plano's agent listener (brightstaff agent_chat handler) clones ALL client
// request headers when forwarding to egent-lobehub. The only headers Plano
// injects itself are x-arch-upstream (agent_id) and x-envoy-max-retries: 3.
// So if the client sends X-User-ID to Plano:8001, egent-lobehub sees it.
func TestExtractUserID_PlanoProxyChain(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	// Simulate: client → Plano:8001 → egent-lobehub:10531
	// Plano forwards client headers verbatim.
	r.Header.Set("X-User-ID", "alice")
	r.Header.Set("x-arch-upstream", "lobehub-agent")
	r.Header.Set("x-envoy-max-retries", "3")
	got := extractUserID(r)
	if got != "alice" {
		t.Errorf("expected X-User-ID from client (via Plano passthrough), got %q", got)
	}
}

// TestExtractUserID_PlanoActorID documents the new brightstaff agent-path
// behavior: after Talos verifies the API key, Plano injects
// x-arch-actor-id: <id> before forwarding to egent-lobehub. This header
// takes priority over X-User-ID (Plano is trusted).
func TestExtractUserID_PlanoActorID(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-arch-actor-id", "talos-user-123")
	r.Header.Set("X-User-ID", "should-be-ignored")
	got := extractUserID(r)
	if got != "talos-user-123" {
		t.Errorf("expected x-arch-actor-id to take priority, got %q", got)
	}
}

// TestExtractArchAgentID documents that egent-lobehub can read Plano's own
// routing header for logging/audit. It's NOT a user identifier.
func TestExtractArchAgentID(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-arch-upstream", "lobehub-agent")
	got := extractArchAgentID(r)
	if got != "lobehub-agent" {
		t.Errorf("expected x-arch-upstream=lobehub-agent, got %q", got)
	}
}
