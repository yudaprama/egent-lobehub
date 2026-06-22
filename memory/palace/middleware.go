package palace

import (
	"context"
	"net/http"
)

// AuthChecker decides whether a request is allowed to perform a write
// (create / update / delete) on palace data. The default
// implementation in main.go is backed by Ory Keto; nil means "no
// authorization required" (personal-scope deployment).
//
// The interface stays in palace to avoid importing the authz package
// (which would create a cycle through the egent-lobehub main
// package). main.go supplies a small adapter.
type AuthChecker interface {
	// CheckMessageWrite reports whether userID has write permission on
	// workspaceID. When workspaceID is empty the call must succeed
	// (personal-scope writes skip the workspace check, matching the
	// LobeHub withScopedPermission behavior).
	CheckMessageWrite(ctx context.Context, userID, workspaceID string) error
}

// EnvAuthChecker is the AuthChecker used when KETO_READ_URL is set.
// When KETO_READ_URL is unset (nil client) the checker passes every
// request — matches the authz.Client.Enabled() fail-open semantics
// for personal-scope deployments.
type EnvAuthChecker struct {
	Check func(ctx context.Context, userID, workspaceID string) error
}

// CheckMessageWrite satisfies AuthChecker.
func (e *EnvAuthChecker) CheckMessageWrite(ctx context.Context, userID, workspaceID string) error {
	if e == nil || e.Check == nil {
		return nil
	}
	return e.Check(ctx, userID, workspaceID)
}

// guard wraps next with the auth check. Returns next unchanged when
// auth is nil.
func (h *Handler) guard(auth AuthChecker, next http.HandlerFunc) http.HandlerFunc {
	if auth == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		userID := UserIDFromContext(r.Context())
		workspaceID := r.Header.Get("X-Workspace-Id")
		if userID == "" || userID == "anonymous" {
			writeError(w, r, http.StatusUnauthorized, "missing user identity")
			return
		}
		if err := auth.CheckMessageWrite(r.Context(), userID, workspaceID); err != nil {
			writeError(w, r, http.StatusForbidden, "forbidden: insufficient workspace permission")
			return
		}
		next(w, r)
	}
}

// RegisterWithAuth mounts every palace endpoint under /v1/memory/* on
// the mux, gating each one through the given AuthChecker. No-op when
// h is nil.
//
// When auth is nil (e.g. KETO_READ_URL is unset) every endpoint is
// mounted without a check, matching the fail-open behavior the
// LobeHub withScopedPermission middleware applies to personal-scope
// requests.
func (h *Handler) RegisterWithAuth(mux *http.ServeMux, auth AuthChecker) {
	if h == nil {
		return
	}
	mux.HandleFunc("/v1/memory/identity", h.guard(auth, h.identityCollection))
	mux.HandleFunc("/v1/memory/identity/", h.guard(auth, h.identityItem))
	mux.HandleFunc("/v1/memory/activity", h.guard(auth, h.activityCollection))
	mux.HandleFunc("/v1/memory/activity/", h.guard(auth, h.activityItem))
	mux.HandleFunc("/v1/memory/context", h.guard(auth, h.contextCollection))
	mux.HandleFunc("/v1/memory/context/", h.guard(auth, h.contextItem))
	mux.HandleFunc("/v1/memory/experience", h.guard(auth, h.experienceCollection))
	mux.HandleFunc("/v1/memory/experience/", h.guard(auth, h.experienceItem))
	mux.HandleFunc("/v1/memory/preference", h.guard(auth, h.preferenceCollection))
	mux.HandleFunc("/v1/memory/preference/", h.guard(auth, h.preferenceItem))
	mux.HandleFunc("/v1/memory/all", h.guard(auth, h.deleteAll))
}

// AllowUnauthenticated is an AuthChecker that lets every request
// through. Useful in tests and in personal-scope deployments where
// KETO_READ_URL is unset.
//
// It is a struct (not a pointer) so callers can pass AllowUnauthenticated{}
// directly without taking its address.
type AllowUnauthenticated struct{}

func (AllowUnauthenticated) CheckMessageWrite(_ context.Context, _, _ string) error { return nil }

// RejectAll is an AuthChecker that rejects every request. Useful in
// tests to verify the gating path.
type RejectAll struct{}

func (RejectAll) CheckMessageWrite(_ context.Context, _, _ string) error {
	return errForbidden
}

// errForbidden is a sentinel returned by RejectAll. The handler
// surfaces it as a generic 403 — the actual error text is not
// propagated to the client.
var errForbidden = stringError("forbidden")

// stringError lets us return a sentinel error value without
// importing errors at the top of this file.
type stringError string

func (s stringError) Error() string { return string(s) }