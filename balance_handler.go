package main

import (
	"context"
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

// balanceHandler: GET /v1/balance — returns the signed-in user's metering
// balance from the Talos admin API. The user is the Talos actor, so we read
// /v2alpha1/admin/actorBalances/{actorId}. This runs server-side (admin token,
// behind the cookie edge) because the admin surface must never face the browser.
//
// A missing balance is reported by Talos as unlimited (quota 0, remaining 0);
// we pass that through so the UI can show "unlimited" / no cap.
func balanceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	bal, err := fetchActorBalance(r.Context(), userID)
	if err != nil {
		slog.Error("balance: talos read failed", "actor", userID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not read balance")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"remainingMicros": bal.RemainingMicros,
		"quotaMicros":     bal.QuotaMicros,
	})
}

type actorBalance struct {
	// grpc-gateway emits int64 as JSON strings; ",string" parses them.
	QuotaMicros     int64 `json:"quotaMicros,string"`
	RemainingMicros int64 `json:"remainingMicros,string"`
}

func fetchActorBalance(ctx context.Context, actorID string) (actorBalance, error) {
	base := strings.TrimRight(os.Getenv("TALOS_URL"), "/")
	if base == "" {
		base = "http://localhost:4420"
	}
	endpoint := base + "/v2alpha1/admin/actorBalances/" + url.PathEscape(actorID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return actorBalance{}, fmt.Errorf("build request: %w", err)
	}
	if tok := os.Getenv("TALOS_ADMIN_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return actorBalance{}, fmt.Errorf("talos request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return actorBalance{}, fmt.Errorf("talos HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var bal actorBalance
	if err := json.Unmarshal(body, &bal); err != nil {
		return actorBalance{}, fmt.Errorf("decode balance: %w (body=%q)", err, string(body))
	}
	return bal, nil
}
