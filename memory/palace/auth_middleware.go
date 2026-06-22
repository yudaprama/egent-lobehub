package palace

import (
	"context"
	"net/http"
	"strings"
)

// AuthMiddleware is a thin wrapper around http.Handler that runs the
// user-id and workspace-id extraction once per request, stores the
// values in the request context, and passes the augmented request to
// the inner handler. Mirrors the pREST `UserFilterMiddleware` pattern
// (see prest/middlewares/userfilter.go).
//
// Scope: this middleware is wired only on the public /v1/memory/*
// route family. Internal endpoints (chat completions, tools, health
// checks) keep their own extractUserID helper in main because they
// have different trust and identity semantics.
//
// Trust model: the only authoritative identity source is the
// `x-arch-actor-id` header set by the upstream gateway (Plano's
// brightstaff orchestrator after Kratos/Talos verify). The
// `X-User-ID` header is kept as a fallback for non-Plano callers
// (dev tools, internal scripts). The legacy `Authorization: kratos:`
// token-as-user pattern is REMOVED — it let any unauthenticated
// request impersonate any user. The TODO in handlers.go noted this
// risk; this middleware codifies the fix.
type AuthMiddleware struct {
	Inner http.Handler
}

type contextKey int

const (
	userIDKey contextKey = iota
	workspaceIDKey
)

// Wrap returns an http.Handler that runs the auth extraction and
// then dispatches to inner. Use as: `mux.Handle("/v1/memory/",
// (&AuthMiddleware{Inner: mux2}).Wrap)` or wrap the whole mux.
func (m *AuthMiddleware) Wrap(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if uid := strings.TrimSpace(r.Header.Get("x-arch-actor-id")); uid != "" {
			ctx = context.WithValue(ctx, userIDKey, uid)
		} else if uid := strings.TrimSpace(r.Header.Get("X-User-ID")); uid != "" {
			ctx = context.WithValue(ctx, userIDKey, uid)
		} else {
			ctx = context.WithValue(ctx, userIDKey, "anonymous")
		}
		if ws := strings.TrimSpace(r.Header.Get("X-Workspace-Id")); ws != "" {
			ctx = context.WithValue(ctx, workspaceIDKey, ws)
		}
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

// UserIDFromContext returns the user ID stored in the request context
// by AuthMiddleware. Returns "anonymous" when the middleware did not
// run (e.g. internal callers passing a bare context) or the request
// had no identity header.
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDKey).(string); ok && v != "" {
		return v
	}
	return "anonymous"
}

// WorkspaceIDFromContext returns the workspace ID stored in the
// request context, or empty string when none was provided.
func WorkspaceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(workspaceIDKey).(string); ok {
		return v
	}
	return ""
}