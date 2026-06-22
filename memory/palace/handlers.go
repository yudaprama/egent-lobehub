package palace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler is the HTTP layer over [Store]. It is mounted under
// /v1/memory/* on the egent-lobehub :10531 server and exposes the
// LobeHub userMemory tRPC procedures as REST-style endpoints.
//
// Every request goes through the standard egent-lobehub extractUserID
// helper (plano x-arch-actor-id → X-User-ID → kratos: → "anonymous").
// The palace store enforces user-scoping at the database layer too,
// so cross-tenant leaks are blocked even when a caller spoofs the
// header (the worst case is they see the "anonymous" user's data).
type Handler struct {
	store Store
}

// NewHandler creates an HTTP handler backed by the given store.
// Returns nil when store is nil — callers should skip mounting the
// /v1/memory/* route family in that case.
func NewHandler(store Store) *Handler {
	if store == nil {
		return nil
	}
	return &Handler{store: store}
}

// Register mounts all palace endpoints under /v1/memory/* on the mux.
// No-op when h is nil.
func (h *Handler) Register(mux *http.ServeMux) {
	if h == nil {
		return
	}
	mux.HandleFunc("/v1/memory/identity", h.identityCollection)
	mux.HandleFunc("/v1/memory/identity/", h.identityItem)
	mux.HandleFunc("/v1/memory/activity", h.activityCollection)
	mux.HandleFunc("/v1/memory/activity/", h.activityItem)
	mux.HandleFunc("/v1/memory/context", h.contextCollection)
	mux.HandleFunc("/v1/memory/context/", h.contextItem)
	mux.HandleFunc("/v1/memory/experience", h.experienceCollection)
	mux.HandleFunc("/v1/memory/experience/", h.experienceItem)
	mux.HandleFunc("/v1/memory/preference", h.preferenceCollection)
	mux.HandleFunc("/v1/memory/preference/", h.preferenceItem)
	mux.HandleFunc("/v1/memory/all", h.deleteAll)
}

// common request bodies used by the create endpoints. The TS tRPC
// schema validates these; we re-validate the minimal required pieces
// here (layer-specific handlers below do the typed validation).

type createIdentityReq struct {
	Identity IdentityInput `json:"identity"`
}

type updateReq struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// userIDFromRequest extracts the user identity from the request. This
// duplicates the helper in handlers.go because palace lives in its
// own package and we don't want to introduce a circular dep on `main`.
// The semantics are identical to the main extractUserID helper.
func userIDFromRequest(r *http.Request) string {
	if uid := r.Header.Get("x-arch-actor-id"); uid != "" {
		return uid
	}
	if uid := r.Header.Get("X-User-ID"); uid != "" {
		return uid
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "kratos:") {
		return strings.TrimPrefix(auth, "kratos:")
	}
	return "anonymous"
}

// readJSON decodes the request body into v. Returns nil for an empty
// body (callers decide whether that's an error).
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error with the given status code. Internal
// errors (500) are logged with the underlying error for debugging;
// only a generic message is sent to the client.
func writeError(w http.ResponseWriter, r *http.Request, status int, msg string, wrap ...error) {
	if status >= 500 {
		slog.ErrorContext(r.Context(), "palace handler error",
			"status", status, "msg", msg, "err", wrap)
		msg = "internal server error"
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// mapStoreError converts a Store error to the appropriate HTTP status.
// ErrNotFound → 404, anything else → 500.
func mapStoreError(w http.ResponseWriter, r *http.Request, err error, action string) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "memory row not found")
		return
	}
	writeError(w, r, http.StatusInternalServerError, action, err)
}

// ============ Identity ============

func (h *Handler) identityCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in IdentityInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !in.Type.Valid() {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid identity type %q", in.Type))
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := h.store.CreateIdentity(ctx, userID, in)
	if err != nil {
		mapStoreError(w, r, err, "create identity")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Handler) identityItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/memory/identity/")
	if id == "" {
		writeError(w, r, http.StatusBadRequest, "missing identity id")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodPatch:
		var in IdentityUpdate
		if err := readJSON(r, &in); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if err := h.store.UpdateIdentity(ctx, userID, id, in); err != nil {
			mapStoreError(w, r, err, "update identity")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	case http.MethodDelete:
		if err := h.store.DeleteIdentity(ctx, userID, id); err != nil {
			mapStoreError(w, r, err, "delete identity")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ============ Activity ============

func (h *Handler) activityCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in ActivityInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if in.Type == "" {
		writeError(w, r, http.StatusBadRequest, "activity type is required")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := h.store.CreateActivity(ctx, userID, in)
	if err != nil {
		mapStoreError(w, r, err, "create activity")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Handler) activityItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/memory/activity/")
	if id == "" {
		writeError(w, r, http.StatusBadRequest, "missing activity id")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodPatch:
		var in ActivityUpdate
		if err := readJSON(r, &in); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if err := h.store.UpdateActivity(ctx, userID, id, in); err != nil {
			mapStoreError(w, r, err, "update activity")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	case http.MethodDelete:
		if err := h.store.DeleteActivity(ctx, userID, id); err != nil {
			mapStoreError(w, r, err, "delete activity")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ============ Context ============

func (h *Handler) contextCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in ContextInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if in.Title == "" {
		writeError(w, r, http.StatusBadRequest, "context title is required")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := h.store.CreateContext(ctx, userID, in)
	if err != nil {
		mapStoreError(w, r, err, "create context")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Handler) contextItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/memory/context/")
	if id == "" {
		writeError(w, r, http.StatusBadRequest, "missing context id")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodPatch:
		var in ContextUpdate
		if err := readJSON(r, &in); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if err := h.store.UpdateContext(ctx, userID, id, in); err != nil {
			mapStoreError(w, r, err, "update context")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	case http.MethodDelete:
		if err := h.store.DeleteContext(ctx, userID, id); err != nil {
			mapStoreError(w, r, err, "delete context")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ============ Experience ============

func (h *Handler) experienceCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in ExperienceInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := h.store.CreateExperience(ctx, userID, in)
	if err != nil {
		mapStoreError(w, r, err, "create experience")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Handler) experienceItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/memory/experience/")
	if id == "" {
		writeError(w, r, http.StatusBadRequest, "missing experience id")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodPatch:
		var in ExperienceUpdate
		if err := readJSON(r, &in); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if err := h.store.UpdateExperience(ctx, userID, id, in); err != nil {
			mapStoreError(w, r, err, "update experience")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	case http.MethodDelete:
		if err := h.store.DeleteExperience(ctx, userID, id); err != nil {
			mapStoreError(w, r, err, "delete experience")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ============ Preference ============

func (h *Handler) preferenceCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in PreferenceInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := h.store.CreatePreference(ctx, userID, in)
	if err != nil {
		mapStoreError(w, r, err, "create preference")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Handler) preferenceItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/memory/preference/")
	if id == "" {
		writeError(w, r, http.StatusBadRequest, "missing preference id")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodPatch:
		var in PreferenceUpdate
		if err := readJSON(r, &in); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if err := h.store.UpdatePreference(ctx, userID, id, in); err != nil {
			mapStoreError(w, r, err, "update preference")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	case http.MethodDelete:
		if err := h.store.DeletePreference(ctx, userID, id); err != nil {
			mapStoreError(w, r, err, "delete preference")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ============ Delete all ============

func (h *Handler) deleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := UserIDFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := h.store.DeleteAll(ctx, userID); err != nil {
		mapStoreError(w, r, err, "delete all memories")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}