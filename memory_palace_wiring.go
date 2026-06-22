package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
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