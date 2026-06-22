package palace

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// helper that builds a request through the AuthMiddleware and
// captures what the inner handler observed.
func runThroughMiddleware(req *http.Request) (status int, userID, workspaceID string) {
	var observed struct {
		userID      string
		workspaceID string
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed.userID = UserIDFromContext(r.Context())
		observed.workspaceID = WorkspaceIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mw := (&AuthMiddleware{}).Wrap(inner)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	return rec.Code, observed.userID, observed.workspaceID
}

func TestAuthMiddleware_ExtractsArchActorID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("x-arch-actor-id", "user-from-plano")
	status, uid, _ := runThroughMiddleware(req)
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
	if uid != "user-from-plano" {
		t.Errorf("expected user-from-plano, got %q", uid)
	}
}

func TestAuthMiddleware_FallsBackToXUserID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("X-User-ID", "user-from-script")
	_, uid, _ := runThroughMiddleware(req)
	if uid != "user-from-script" {
		t.Errorf("expected X-User-ID fallback, got %q", uid)
	}
}

func TestAuthMiddleware_PrefersArchActorOverUserID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("x-arch-actor-id", "primary")
	req.Header.Set("X-User-ID", "fallback")
	_, uid, _ := runThroughMiddleware(req)
	if uid != "primary" {
		t.Errorf("expected x-arch-actor-id to win, got %q", uid)
	}
}

func TestAuthMiddleware_NoIdentityIsAnonymous(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	_, uid, _ := runThroughMiddleware(req)
	if uid != "anonymous" {
		t.Errorf("expected anonymous fallback, got %q", uid)
	}
}

// The legacy `Authorization: kratos:<token>` was a real security
// issue: the previous extractUserID implementation treated the
// token itself as the user-id without any validation. The new
// middleware MUST NOT trust this header.
func TestAuthMiddleware_IgnoresLegacyKratosHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("Authorization", "kratos:any-token-the-caller-wants")
	_, uid, _ := runThroughMiddleware(req)
	if uid == "any-token-the-caller-wants" {
		t.Fatal("SECURITY: kratos: token used as user-id — impersonation bug not fixed")
	}
	if uid != "anonymous" {
		t.Errorf("expected anonymous when only kratos header present, got %q", uid)
	}
}

func TestAuthMiddleware_ExtractsWorkspaceID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("x-arch-actor-id", "u-1")
	req.Header.Set("X-Workspace-Id", "ws-team-42")
	_, uid, ws := runThroughMiddleware(req)
	if uid != "u-1" {
		t.Errorf("expected u-1, got %q", uid)
	}
	if ws != "ws-team-42" {
		t.Errorf("expected ws-team-42, got %q", ws)
	}
}

func TestAuthMiddleware_EmptyWorkspaceIsEmptyString(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("x-arch-actor-id", "u-1")
	_, _, ws := runThroughMiddleware(req)
	if ws != "" {
		t.Errorf("expected empty workspace, got %q", ws)
	}
}

func TestAuthMiddleware_TrimsWhitespace(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("x-arch-actor-id", "  spaced-user  ")
	_, uid, _ := runThroughMiddleware(req)
	if uid != "spaced-user" {
		t.Errorf("expected trimmed user-id, got %q", uid)
	}
}

func TestAuthMiddleware_EmptyAfterTrimIsAnonymous(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/identity", nil)
	req.Header.Set("x-arch-actor-id", "   ")
	_, uid, _ := runThroughMiddleware(req)
	if uid != "anonymous" {
		t.Errorf("expected anonymous for whitespace-only header, got %q", uid)
	}
}

// UserIDFromContext / WorkspaceIDFromContext must also work when
// called from a bare context (e.g. tests that don't go through the
// middleware). This keeps the helper safe to use everywhere.
func TestContextHelpers_BareContext(t *testing.T) {
	if uid := UserIDFromContext(httptest.NewRequest(http.MethodGet, "/x", nil).Context()); uid != "anonymous" {
		t.Errorf("expected anonymous, got %q", uid)
	}
	if ws := WorkspaceIDFromContext(httptest.NewRequest(http.MethodGet, "/x", nil).Context()); ws != "" {
		t.Errorf("expected empty workspace, got %q", ws)
	}
}