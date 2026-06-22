package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"egent-lobehub/authz"
	"egent-lobehub/memory/palace"
)

// memoryPalaceNotConfiguredHandler returns 503 for every /v1/memory/*
// request when the palace store is not configured (i.e. no shared
// Postgres pool). Mirrors the composioConnectHandler pattern of
// failing fast on a missing dependency instead of silently 500'ing.
//
// The Phase 2 read path stays on pREST SQL templates; the /v1/memory/*
// routes here are the write path. When the pool is missing, callers
// fall back to pREST for reads but cannot create/update memories.
func memoryPalaceNotConfiguredHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"error": "memory palace: shared Postgres pool not configured (set KNOWLEDGE_PG_DSN or DATABASE_URL)",
	}); err != nil {
		slog.Warn("memory palace: encode 503 response failed", "error", err)
	}
}

// buildPalaceAuth constructs the AuthChecker used by the palace write
// handlers. Returns nil when KETO_READ_URL is unset, matching the
// LobeHub withScopedPermission fail-open behavior for personal-scope
// deployments. When set, every palace write runs an Ory Keto check
// for the "write" relation on the workspace namespace — equivalent
// to the TS `withScopedPermission('message:create')` middleware.
//
// The Keto client itself is constructed lazily: we do not block
// startup on Keto reachability. Failed checks degrade to a 403 for
// the request, not a startup panic.
func buildPalaceAuth(keto *authz.Client) palace.AuthChecker {
	if keto == nil || !keto.Enabled() {
		return nil
	}
	return &palace.EnvAuthChecker{
		Check: func(ctx context.Context, userID, workspaceID string) error {
			if userID == "" {
				return errMissingUser
			}
			if workspaceID == "" {
				// Personal scope: no workspace check required.
				return nil
			}
			allowed, err := keto.CheckWorkspace(ctx, workspaceID, userID, "write")
			if err != nil {
				slog.Warn("memory palace: keto check failed", "user", userID, "workspace", workspaceID, "error", err)
				return errKetoUnavailable
			}
			if !allowed {
				return errWorkspaceForbidden
			}
			return nil
		},
	}
}

// sentinel errors used by buildPalaceAuth. They are kept private to
// this file; the middleware surfaces them as a generic 403 / 401 to
// the client.
var (
	errMissingUser         = palaceError("missing user identity")
	errWorkspaceForbidden  = palaceError("insufficient workspace permission")
	errKetoUnavailable     = palaceError("keto check failed")
)

type palaceError string

func (e palaceError) Error() string { return string(e) }