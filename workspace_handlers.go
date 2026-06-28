package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"egent-lobehub/authz"
)

// Workspace management with DUAL-WRITE: the Postgres rows (workspaces,
// workspace_members) AND the Keto authz tuples must agree. Keto is the authz
// source of truth; the tables are a UI-listing mirror. The browser cannot write
// Keto tuples (admin API), so this runs server-side behind the cookie edge
// (X-User-Id injected by Oathkeeper). dbPool points at the app database.

const (
	roleOwner  = "owner"
	roleMember = "member"
	roleViewer = "viewer"
)

// roleToRelation maps an app role to the Keto Workspace relation.
var roleToRelation = map[string]string{
	roleOwner:  "owners",
	roleMember: "members",
	roleViewer: "viewers",
}

// authzWorkspaceHandler is the status-code adapter Ory Oathkeeper's remote_json
// authorizer calls to gate workspace access (the Phase 1 edge gate). It returns
// 200 (allow) / 403 (deny) — necessary because Keto's /relation-tuples/check
// answers 200 with {"allowed": bool} in the BODY, which remote_json (status-only)
// cannot read. Internal-only: Oathkeeper (loopback) calls it directly on :10531,
// NOT via the public edge. Fails closed.
//
// Payload (set by the Oathkeeper rule, per HTTP method):
//
//	{"user": "<subject>", "workspace": "<X-Workspace-Id>", "permission": "view|write|manage"}
//
// Empty workspace = personal scope → always allowed (pREST scopes to
// user_id + workspace_id IS NULL).
func authzWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User       string `json:"user"`
		Workspace  string `json:"workspace"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.User == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if body.Workspace == "" {
		w.WriteHeader(http.StatusOK) // personal scope
		return
	}
	if ketoClient == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	perm := body.Permission
	if perm != "view" && perm != "write" && perm != "manage" {
		perm = "view"
	}
	allowed, err := ketoClient.CheckWorkspace(r.Context(), body.Workspace, body.User, perm)
	if err != nil {
		slog.Error("authz workspace gate: keto check", "err", err)
		http.Error(w, "authz error", http.StatusForbidden) // fail closed
		return
	}
	if !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// workspacesHandler dispatches /v1/workspaces by method:
//   - POST   → create a workspace owned by the caller
//   - DELETE → delete a workspace the caller owns (manage)
func workspacesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		createWorkspace(w, r)
	case http.MethodDelete:
		deleteWorkspace(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// createWorkspace handles POST /v1/workspaces — create a workspace owned by the caller.
func createWorkspace(w http.ResponseWriter, r *http.Request) {
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if dbPool == nil || ketoClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "workspaces not configured")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name required")
		return
	}

	id, err := generateNanoID(12)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "id generation failed")
		return
	}
	wsID := "ws_" + id
	ctx := r.Context()

	// Postgres: workspace + owner membership in one tx.
	tx, err := dbPool.Begin(ctx)
	if err != nil {
		slog.Error("create workspace: begin tx", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not create workspace")
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspaces (id, name, owner_id) VALUES ($1, $2, $3)`,
		wsID, body.Name, userID); err != nil {
		slog.Error("create workspace: insert workspace", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not create workspace")
		return
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		wsID, userID, roleOwner); err != nil {
		slog.Error("create workspace: insert member", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not create workspace")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("create workspace: commit", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not create workspace")
		return
	}

	// Keto: owner tuple (idempotent PUT upsert). On failure the row exists
	// without authz → the gate denies access; log loudly for repair.
	if err := ketoClient.WriteTuple(ctx, authz.Tuple{
		Namespace: authz.WorkspaceNamespace,
		Object:    wsID,
		Relation:  "owners",
		SubjectID: userID,
	}); err != nil {
		slog.Error("create workspace: keto owner tuple FAILED (row exists, authz missing)",
			"workspace", wsID, "user", userID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "workspace created but authz write failed; retry")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": wsID, "name": body.Name, "role": roleOwner})
}

// workspaceMembersHandler: POST /v1/workspaces/members — add/update a member of a
// workspace the caller owns (manage permission).
func workspaceMembersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if dbPool == nil || ketoClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "workspaces not configured")
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
		MemberID    string `json:"memberId"`
		Role        string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.WorkspaceID == "" || body.MemberID == "" {
		writeJSONError(w, http.StatusBadRequest, "workspaceId and memberId required")
		return
	}
	relation, ok := roleToRelation[body.Role]
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "role must be owner|member|viewer")
		return
	}

	ctx := r.Context()
	// Only an owner (manage) may change membership.
	allowed, err := ketoClient.CheckWorkspace(ctx, body.WorkspaceID, userID, "manage")
	if err != nil {
		slog.Error("add member: keto check", "err", err)
		writeJSONError(w, http.StatusBadGateway, "authz check failed")
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, "only an owner can manage members")
		return
	}

	if _, err := dbPool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		body.WorkspaceID, body.MemberID, body.Role); err != nil {
		slog.Error("add member: insert", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not add member")
		return
	}
	if err := ketoClient.WriteTuple(ctx, authz.Tuple{
		Namespace: authz.WorkspaceNamespace,
		Object:    body.WorkspaceID,
		Relation:  relation,
		SubjectID: body.MemberID,
	}); err != nil {
		slog.Error("add member: keto tuple FAILED", "workspace", body.WorkspaceID,
			"member", body.MemberID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "member added but authz write failed; retry")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"workspaceId": body.WorkspaceID, "memberId": body.MemberID, "role": body.Role,
	})
}

// deleteAllTuplesForObject removes every relation tuple on a workspace object
// (owners ∪ members ∪ viewers). Used when a workspace is deleted. Best-effort
// per tuple; returns the first error so the caller can log it.
func deleteAllTuplesForObject(ctx context.Context, workspaceID string) error {
	tuples, err := ketoClient.ListTuples(ctx, authz.TupleQuery{
		Namespace: authz.WorkspaceNamespace,
		Object:    workspaceID,
	})
	if err != nil {
		return err
	}
	var firstErr error
	for _, t := range tuples {
		if err := ketoClient.DeleteTuple(ctx, t); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// deleteSubjectTuples removes every relation a subject holds on a workspace
// object (so a removed member keeps no residual owners/members/viewers tuple).
func deleteSubjectTuples(ctx context.Context, workspaceID, subjectID string) error {
	tuples, err := ketoClient.ListTuples(ctx, authz.TupleQuery{
		Namespace: authz.WorkspaceNamespace,
		Object:    workspaceID,
		SubjectID: subjectID,
	})
	if err != nil {
		return err
	}
	var firstErr error
	for _, t := range tuples {
		if err := ketoClient.DeleteTuple(ctx, t); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// deleteWorkspace handles DELETE /v1/workspaces {workspaceId} — owner-only. The
// workspaces row is removed (FK cascade clears members + scoped content), then
// all Keto tuples for the object are expanded and deleted.
func deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if dbPool == nil || ketoClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "workspaces not configured")
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspaceID == "" {
		writeJSONError(w, http.StatusBadRequest, "workspaceId required")
		return
	}

	ctx := r.Context()
	allowed, err := ketoClient.CheckWorkspace(ctx, body.WorkspaceID, userID, "manage")
	if err != nil {
		slog.Error("delete workspace: keto check", "err", err)
		writeJSONError(w, http.StatusBadGateway, "authz check failed")
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, "only an owner can delete a workspace")
		return
	}

	if _, err := dbPool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, body.WorkspaceID); err != nil {
		slog.Error("delete workspace: delete row", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not delete workspace")
		return
	}
	if err := deleteAllTuplesForObject(ctx, body.WorkspaceID); err != nil {
		slog.Error("delete workspace: keto tuple cleanup FAILED (row gone, tuples linger)",
			"workspace", body.WorkspaceID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "workspace deleted but authz cleanup failed; retry")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": body.WorkspaceID, "deleted": true})
}

// workspaceRemoveMemberHandler: POST /v1/workspaces/members/remove
// {workspaceId, memberId} — owner-only (manage). Removes the member row and all
// their Keto tuples on the workspace.
func workspaceRemoveMemberHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if dbPool == nil || ketoClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "workspaces not configured")
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
		MemberID    string `json:"memberId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.WorkspaceID == "" || body.MemberID == "" {
		writeJSONError(w, http.StatusBadRequest, "workspaceId and memberId required")
		return
	}

	ctx := r.Context()
	allowed, err := ketoClient.CheckWorkspace(ctx, body.WorkspaceID, userID, "manage")
	if err != nil {
		slog.Error("remove member: keto check", "err", err)
		writeJSONError(w, http.StatusBadGateway, "authz check failed")
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, "only an owner can manage members")
		return
	}
	// Guard: don't strand a workspace with no owner. The last owner must delete
	// the workspace instead of removing themselves out of it.
	if body.MemberID == userID {
		writeJSONError(w, http.StatusBadRequest, "use leave to remove yourself")
		return
	}

	if _, err := dbPool.Exec(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		body.WorkspaceID, body.MemberID); err != nil {
		slog.Error("remove member: delete row", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not remove member")
		return
	}
	if err := deleteSubjectTuples(ctx, body.WorkspaceID, body.MemberID); err != nil {
		slog.Error("remove member: keto tuple cleanup FAILED", "workspace", body.WorkspaceID,
			"member", body.MemberID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "member removed but authz cleanup failed; retry")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"workspaceId": body.WorkspaceID, "memberId": body.MemberID, "removed": true,
	})
}

// workspaceLeaveHandler: POST /v1/workspaces/leave {workspaceId} — self-remove
// (any member). A sole owner cannot leave (they must delete the workspace).
func workspaceLeaveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if dbPool == nil || ketoClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "workspaces not configured")
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspaceID == "" {
		writeJSONError(w, http.StatusBadRequest, "workspaceId required")
		return
	}

	ctx := r.Context()
	// If the caller is the sole owner, refuse — leaving would orphan the workspace.
	var role string
	if err := dbPool.QueryRow(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		body.WorkspaceID, userID).Scan(&role); err != nil {
		writeJSONError(w, http.StatusNotFound, "not a member of this workspace")
		return
	}
	if role == roleOwner {
		var ownerCount int
		if err := dbPool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'owner'`,
			body.WorkspaceID).Scan(&ownerCount); err != nil {
			slog.Error("leave workspace: count owners", "err", err)
			writeJSONError(w, http.StatusBadGateway, "could not leave workspace")
			return
		}
		if ownerCount <= 1 {
			writeJSONError(w, http.StatusBadRequest, "sole owner cannot leave; delete the workspace instead")
			return
		}
	}

	if _, err := dbPool.Exec(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		body.WorkspaceID, userID); err != nil {
		slog.Error("leave workspace: delete row", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not leave workspace")
		return
	}
	if err := deleteSubjectTuples(ctx, body.WorkspaceID, userID); err != nil {
		slog.Error("leave workspace: keto tuple cleanup FAILED", "workspace", body.WorkspaceID,
			"user", userID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "left workspace but authz cleanup failed; retry")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"workspaceId": body.WorkspaceID, "left": true})
}

// internalWorkspaceBootstrapHandler: POST /internal/workspaces/bootstrap — called
// by the Kratos after-registration web_hook (server-side, NOT the browser; the
// /internal/ path is not routed by the public Oathkeeper edge, and egent-lobehub
// binds loopback-only). Provisions the new user's default workspace so every user
// is workspace-first. Gated by a shared secret (X-Bootstrap-Secret) for
// defense-in-depth, and idempotent (deterministic id + ON CONFLICT).
func internalWorkspaceBootstrapHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	secret := os.Getenv("KRATOS_HOOK_SECRET")
	if secret == "" || r.Header.Get("X-Bootstrap-Secret") != secret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if dbPool == nil || ketoClient == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "workspaces not configured")
		return
	}
	var body struct {
		UserID string `json:"userId"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
		writeJSONError(w, http.StatusBadRequest, "userId required")
		return
	}
	if body.Name == "" {
		body.Name = "My Workspace"
	}
	wsID := "ws_default_" + body.UserID // deterministic → idempotent
	ctx := r.Context()

	tx, err := dbPool.Begin(ctx)
	if err != nil {
		slog.Error("bootstrap workspace: begin tx", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not bootstrap workspace")
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspaces (id, name, owner_id) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO NOTHING`,
		wsID, body.Name, body.UserID); err != nil {
		slog.Error("bootstrap workspace: insert workspace", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not bootstrap workspace")
		return
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
		 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
		wsID, body.UserID); err != nil {
		slog.Error("bootstrap workspace: insert member", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not bootstrap workspace")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("bootstrap workspace: commit", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not bootstrap workspace")
		return
	}

	if err := ketoClient.WriteTuple(ctx, authz.Tuple{
		Namespace: authz.WorkspaceNamespace,
		Object:    wsID,
		Relation:  "owners",
		SubjectID: body.UserID,
	}); err != nil {
		slog.Error("bootstrap workspace: keto owner tuple FAILED", "workspace", wsID, "user", body.UserID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "authz write failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": wsID, "name": body.Name})
}
