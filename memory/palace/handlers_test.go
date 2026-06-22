package palace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStore is an in-memory [Store] used to exercise the HTTP layer
// without a real Postgres. The state is captured in a map so tests
// can assert on the round-trip shape.
type fakeStore struct {
	rows map[string]map[string]any // layer -> id -> payload
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]map[string]any{
		"identity":   {},
		"activity":   {},
		"context":    {},
		"experience": {},
		"preference": {},
	}}
}

func (f *fakeStore) CreateIdentity(_ context.Context, _ string, in IdentityInput) (string, error) {
	if !in.Type.Valid() {
		return "", errors.New("invalid type")
	}
	id := "idn-fake-1"
	f.rows["identity"][id] = in
	return id, nil
}
func (f *fakeStore) UpdateIdentity(_ context.Context, _, _ string, _ IdentityUpdate) error { return nil }
func (f *fakeStore) DeleteIdentity(_ context.Context, _, _ string) error                   { return nil }
func (f *fakeStore) CreateActivity(_ context.Context, _ string, in ActivityInput) (string, error) {
	id := "act-fake-1"
	f.rows["activity"][id] = in
	return id, nil
}
func (f *fakeStore) UpdateActivity(_ context.Context, _, _ string, _ ActivityUpdate) error  { return nil }
func (f *fakeStore) DeleteActivity(_ context.Context, _, _ string) error                    { return nil }
func (f *fakeStore) CreateContext(_ context.Context, _ string, in ContextInput) (string, error) {
	if in.Title == "" {
		return "", errors.New("title required")
	}
	id := "ctx-fake-1"
	f.rows["context"][id] = in
	return id, nil
}
func (f *fakeStore) UpdateContext(_ context.Context, _, _ string, _ ContextUpdate) error   { return nil }
func (f *fakeStore) DeleteContext(_ context.Context, _, _ string) error                     { return nil }
func (f *fakeStore) CreateExperience(_ context.Context, _ string, in ExperienceInput) (string, error) {
	id := "exp-fake-1"
	f.rows["experience"][id] = in
	return id, nil
}
func (f *fakeStore) UpdateExperience(_ context.Context, _, _ string, _ ExperienceUpdate) error { return nil }
func (f *fakeStore) DeleteExperience(_ context.Context, _, _ string) error                    { return nil }
func (f *fakeStore) CreatePreference(_ context.Context, _ string, in PreferenceInput) (string, error) {
	id := "prf-fake-1"
	f.rows["preference"][id] = in
	return id, nil
}
func (f *fakeStore) UpdatePreference(_ context.Context, _, _ string, _ PreferenceUpdate) error { return nil }
func (f *fakeStore) DeletePreference(_ context.Context, _, _ string) error                    { return nil }
func (f *fakeStore) DeleteAll(_ context.Context, _ string) error                              { return nil }
func (f *fakeStore) HealthCheck(_ context.Context) error                                       { return nil }

// request is a small helper that runs a single request through the
// handler mux and returns the recorded response.
func request(t *testing.T, h *Handler, method, path string, body string, userID string) *httptest.ResponseRecorder {
	t.Helper()
	return requestWithAuth(t, h, method, path, body, userID, nil)
}

func requestWithAuth(t *testing.T, h *Handler, method, path string, body, userID string, auth AuthChecker) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterWithAuth(mux, auth)
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if userID != "" {
		req.Header.Set("x-arch-actor-id", userID)
	}
	// Wrap with AuthMiddleware so the request context carries the
	// user-id and workspace-id that handlers read via
	// UserIDFromContext. Mirrors the production wiring in main.go.
	wrapped := (&AuthMiddleware{}).Wrap(mux)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

func TestHandler_CreateIdentity_HappyPath(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"description":"Alice is a researcher","role":"scientist","relationship":"colleague","type":"professional"}`
	rec := request(t, h, http.MethodPost, "/v1/memory/identity", body, "user-1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp["id"], "idn-fake-") {
		t.Errorf("expected idn-fake-* id, got %q", resp["id"])
	}
}

func TestHandler_CreateIdentity_InvalidType(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"description":"oops","type":"alien"}`
	rec := request(t, h, http.MethodPost, "/v1/memory/identity", body, "user-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_CreateContext_MissingTitle(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"description":"missing title"}`
	rec := request(t, h, http.MethodPost, "/v1/memory/context", body, "user-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_CreateActivity_MissingType(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"narrative":"going for a run"}`
	rec := request(t, h, http.MethodPost, "/v1/memory/activity", body, "user-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_DeleteAll_MethodNotAllowed(t *testing.T) {
	h := NewHandler(newFakeStore())
	rec := request(t, h, http.MethodGet, "/v1/memory/all", "", "user-1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandler_UpdateIdentity_NotFound(t *testing.T) {
	store := newFakeStore()
	// Replace DeleteIdentity with one that returns ErrNotFound.
	h := NewHandler(&notFoundIdentityStore{Store: store})
	body := `{"description":"updated"}`
	rec := request(t, h, http.MethodPatch, "/v1/memory/identity/does-not-exist", body, "user-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandler_NewHandler_NilStore(t *testing.T) {
	if h := NewHandler(nil); h != nil {
		t.Errorf("expected nil handler for nil store, got %+v", h)
	}
}

func TestHandler_Register_NilHandlerIsNoop(t *testing.T) {
	var h *Handler
	mux := http.NewServeMux()
	// Should not panic.
	h.Register(mux)
}

func TestHandler_Auth_RejectsAnonymous(t *testing.T) {
	h := NewHandler(newFakeStore())
	// No x-arch-actor-id → middleware should reject.
	rec := requestWithAuth(t, h, http.MethodPost, "/v1/memory/identity",
		`{"description":"alice"}`, "", RejectAll{})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Auth_RejectsForbidden(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"description":"alice","type":"personal"}`
	rec := requestWithAuth(t, h, http.MethodPost, "/v1/memory/identity", body, "user-1", RejectAll{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Auth_NilIsFailOpen(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"description":"alice","type":"personal"}`
	rec := requestWithAuth(t, h, http.MethodPost, "/v1/memory/identity", body, "user-1", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 (fail-open), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Auth_AllowUnauthenticated(t *testing.T) {
	h := NewHandler(newFakeStore())
	body := `{"description":"alice","type":"personal"}`
	rec := requestWithAuth(t, h, http.MethodPost, "/v1/memory/identity", body, "user-1", AllowUnauthenticated{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// notFoundIdentityStore wraps fakeStore but forces UpdateIdentity to
// return ErrNotFound, exercising the error-mapping path.
type notFoundIdentityStore struct {
	Store
}

func (n *notFoundIdentityStore) UpdateIdentity(_ context.Context, _, _ string, _ IdentityUpdate) error {
	return ErrNotFound
}

// Compile-time check that the fake store satisfies the interface,
// so a future interface change does not silently break the tests.
var _ Store = (*fakeStore)(nil)