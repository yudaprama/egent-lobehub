package composio

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// testToken is the value the mock server expects in x-api-key. Mirrors
// groq-go/internal/test/server.go's testAPI constant so the parity is
// explicit.
const testToken = "this-is-my-secure-token-do-not-steal!!"

// mockServer returns an httptest.Server that dispatches to the supplied
// handlers based on `method path`. The handler may inspect/verify the
// request and write any response. Requests with the wrong API key get 401;
// unmatched routes get 404.
//
// This is a stdlib-only equivalent of groq-go/internal/test's ServerTest +
// ComposioTestServer so this package stays self-contained (no groq-go
// dependency in tests either).
type mockServer struct {
	t        *testing.T
	handlers map[string]http.HandlerFunc
	server   *httptest.Server
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	m := &mockServer{
		t:        t,
		handlers: make(map[string]http.HandlerFunc),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.dispatch))
	t.Cleanup(m.server.Close)
	return m
}

// on registers a handler for "METHOD /path". Path may contain `*` which is
// treated as a regex .* — same convention as groq-go's RegisterHandler.
func (m *mockServer) on(route string, h http.HandlerFunc) {
	route = strings.ReplaceAll(route, "*", ".*")
	m.handlers[route] = h
}

func (m *mockServer) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(APIHeader) != testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	route := r.Method + " " + r.URL.Path
	for k, h := range m.handlers {
		// Use a simple substring match on the route key. Tests register
		// either a full "METHOD /path" or a prefix; this is sufficient
		// because the catalog of routes in this package is small and
		// each path is unique.
		if strings.Contains(route, strings.SplitN(k, " ", 2)[1]) {
			// Verify method matches when the route specifies one.
			parts := strings.SplitN(k, " ", 2)
			if len(parts) == 2 && parts[0] != "" && parts[0] != r.Method {
				continue
			}
			h(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

// mustClient builds a Composio client pointed at the mock server. Fails
// the test if construction errors (which only happens on programmer error
// because we always pass a non-empty key here).
func (m *mockServer) mustClient(t *testing.T, opts ...Option) *Composio {
	t.Helper()
	c, err := NewComposer(testToken, append([]Option{WithBaseURL(m.server.URL)}, opts...)...)
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func readBody(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// ---------- NewComposer ----------

func TestNewComposer_EmptyKeyReturnsNil(t *testing.T) {
	c, err := NewComposer("")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c != nil {
		t.Fatalf("client = %v, want nil", c)
	}
}

func TestNewComposer_AppliesOptions(t *testing.T) {
	c, err := NewComposer("k", WithBaseURL("https://example.com/api/"), WithTimeout(0))
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "https://example.com/api" {
		t.Errorf("baseURL = %q, want %q", c.baseURL, "https://example.com/api")
	}
}

// ---------- ListConnections ----------

func TestListConnections(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /connected_accounts", func(w http.ResponseWriter, r *http.Request) {
		// Verify default query params are absent (caller didn't set them).
		q := r.URL.Query()
		if q.Get("user_uuid") != "" {
			t.Errorf("user_uuid = %q, want empty", q.Get("user_uuid"))
		}
		writeJSON(t, w, map[string]any{
			"items": []map[string]any{
				{"id": "conn_1", "status": "ACTIVE", "toolkit": map[string]any{"slug": "github"}},
				{"id": "conn_2", "status": "FAILED", "toolkit": map[string]any{"slug": "slack"}},
			},
		})
	})
	c := m.mustClient(t)

	got, err := c.ListConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "conn_1" || got[0].Status != StatusActive {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Toolkit.Slug != "slack" {
		t.Errorf("got[1].Toolkit.Slug = %q", got[1].Toolkit.Slug)
	}
}

func TestListConnections_AuthOptions(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /connected_accounts", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("user_uuid") != "user-123" {
			t.Errorf("user_uuid = %q", q.Get("user_uuid"))
		}
		if q.Get("showActiveOnly") != "true" {
			t.Errorf("showActiveOnly = %q", q.Get("showActiveOnly"))
		}
		writeJSON(t, w, map[string]any{"items": []any{}})
	})
	c := m.mustClient(t)

	_, err := c.ListConnections(context.Background(),
		WithUserUUID("user-123"),
		WithShowActiveOnly(true),
	)
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- GetConnection ----------

func TestGetConnection(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /connected_accounts/conn_1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"id":      "conn_1",
			"status":  "ACTIVE",
			"toolkit": map[string]any{"slug": "github"},
		})
	})
	c := m.mustClient(t)

	got, err := c.GetConnection(context.Background(), "conn_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive || got.Toolkit.Slug != "github" {
		t.Errorf("got = %+v", got)
	}
}

func TestGetConnection_401ReturnsSyntheticAuthError(t *testing.T) {
	m := newMockServer(t)
	// Override the auth check by registering a handler that always 401s
	// regardless of key. We do this by matching the URL path explicitly
	// before the standard auth check fires.
	m.server.Close()
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/connected_accounts/conn_x") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get(APIHeader) != testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(m.server.Close)

	c, err := NewComposer(testToken, WithBaseURL(m.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetConnection(context.Background(), "conn_x")
	if err != nil {
		t.Fatalf("err = %v, want nil (auth errors are in-band)", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("Status = %q, want FAILED", got.Status)
	}
	if got.ErrorReason != "AUTH_ERROR" {
		t.Errorf("ErrorReason = %q, want AUTH_ERROR", got.ErrorReason)
	}
}

func TestGetConnection_EmptyID(t *testing.T) {
	m := newMockServer(t)
	c := m.mustClient(t)
	_, err := c.GetConnection(context.Background(), "")
	if err == nil {
		t.Fatal("err = nil, want validation error")
	}
}

// ---------- LinkConnection ----------

func TestLinkConnection(t *testing.T) {
	m := newMockServer(t)
	m.on("POST /connected_accounts/link", func(w http.ResponseWriter, r *http.Request) {
		var body linkRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.UserID != "user-1" || body.AuthConfigID != "auth_1" {
			t.Errorf("body = %+v", body)
		}
		if body.CallbackURL != "https://app/cb" {
			t.Errorf("CallbackURL = %q", body.CallbackURL)
		}
		writeJSON(t, w, map[string]any{
			"connected_account_id": "conn_new",
			"redirect_url":         "https://auth.example/connect?token=abc",
		})
	})
	c := m.mustClient(t)

	id, redirect, err := c.LinkConnection(context.Background(), "user-1", "auth_1", "https://app/cb")
	if err != nil {
		t.Fatal(err)
	}
	if id != "conn_new" {
		t.Errorf("id = %q", id)
	}
	if redirect != "https://auth.example/connect?token=abc" {
		t.Errorf("redirect = %q", redirect)
	}
}

func TestLinkConnection_MissingUserID(t *testing.T) {
	m := newMockServer(t)
	c := m.mustClient(t)
	_, _, err := c.LinkConnection(context.Background(), "", "auth_1", "")
	if err == nil {
		t.Fatal("err = nil, want validation error")
	}
}

// ---------- DeleteConnection ----------

func TestDeleteConnection(t *testing.T) {
	m := newMockServer(t)
	deleted := false
	m.on("DELETE /connected_accounts/conn_1", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	c := m.mustClient(t)

	if err := c.DeleteConnection(context.Background(), "conn_1"); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("delete handler not called")
	}
}

// ---------- Auth configs ----------

func TestListAuthConfigs(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"items": []map[string]any{
				{"id": "auth_1", "name": "GitHub", "type": "use_composio_managed_auth", "toolkit": map[string]any{"slug": "github"}},
			},
		})
	})
	c := m.mustClient(t)

	got, err := c.ListAuthConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "auth_1" || got[0].Toolkit.Slug != "github" {
		t.Errorf("got = %+v", got)
	}
}

func TestCreateManagedAuthConfig(t *testing.T) {
	m := newMockServer(t)
	m.on("POST /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		var body authConfigCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Slug must be normalised to upper-snake.
		if body.Toolkit.Slug != "GITHUB" {
			t.Errorf("Toolkit.Slug = %q, want GITHUB", body.Toolkit.Slug)
		}
		if body.AuthConfig.Type != "use_composio_managed_auth" {
			t.Errorf("Type = %q", body.AuthConfig.Type)
		}
		writeJSON(t, w, map[string]any{
			"id":      "auth_new",
			"name":    "GITHUB",
			"type":    "use_composio_managed_auth",
			"toolkit": map[string]any{"slug": "github"},
		})
	})
	c := m.mustClient(t)

	got, err := c.CreateManagedAuthConfig(context.Background(), "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "auth_new" || got.Toolkit.Slug != "github" {
		t.Errorf("got = %+v", got)
	}
}

func TestFindAuthConfigForToolkit(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"items": []map[string]any{
				{"id": "auth_1", "toolkit": map[string]any{"slug": "GITHUB"}},
				{"id": "auth_2", "toolkit": map[string]any{"slug": "slack"}},
			},
		})
	})
	c := m.mustClient(t)

	got, err := c.FindAuthConfigForToolkit(context.Background(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "auth_1" {
		t.Errorf("got = %+v", got)
	}

	// Not found.
	got, err = c.FindAuthConfigForToolkit(context.Background(), "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestResolveOrCreateAuthConfig_FindsExisting(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"items": []map[string]any{
				{"id": "auth_existing", "toolkit": map[string]any{"slug": "github"}},
			},
		})
	})
	createCalled := false
	m.on("POST /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		createCalled = true
	})
	c := m.mustClient(t)

	got, err := c.ResolveOrCreateAuthConfig(context.Background(), "github", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "auth_existing" {
		t.Errorf("ID = %q, want auth_existing", got.ID)
	}
	if createCalled {
		t.Error("create was called but should have been skipped")
	}
}

func TestResolveOrCreateAuthConfig_CreatesWhenMissing(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"items": []any{}})
	})
	m.on("POST /auth_configs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"id":      "auth_new",
			"name":    "slack",
			"type":    "use_composio_managed_auth",
			"toolkit": map[string]any{"slug": "slack"},
		})
	})
	c := m.mustClient(t)

	got, err := c.ResolveOrCreateAuthConfig(context.Background(), "slack", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "auth_new" {
		t.Errorf("ID = %q, want auth_new", got.ID)
	}
}

// ---------- GetTools ----------

func TestGetTools(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("toolkit_slug") != "GITHUB" {
			t.Errorf("toolkit_slug = %q, want GITHUB", q.Get("toolkit_slug"))
		}
		writeJSON(t, w, map[string]any{
			"items": []map[string]any{
				{
					"slug":        "GITHUB_LIST_REPOS",
					"description": "List repositories",
					"toolkit":     map[string]any{"slug": "github"},
					"input_parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"owner": map[string]any{"type": "string"},
						},
					},
				},
			},
		})
	})
	c := m.mustClient(t)

	got, err := c.GetToolsForApp(context.Background(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Slug != "GITHUB_LIST_REPOS" {
		t.Errorf("Slug = %q", got[0].Slug)
	}
	// ArgsSchema should preserve the raw input_parameters object.
	var schema map[string]any
	if err := json.Unmarshal(got[0].ArgsSchema, &schema); err != nil {
		t.Fatalf("unmarshal args schema: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v", schema["type"])
	}
}

func TestGetTools_SearchAndTags(t *testing.T) {
	m := newMockServer(t)
	m.on("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("search") != "send email" {
			t.Errorf("search = %q", q.Get("search"))
		}
		if q.Get("tags") != "Actions,Production" {
			t.Errorf("tags = %q", q.Get("tags"))
		}
		writeJSON(t, w, map[string]any{"items": []any{}})
	})
	c := m.mustClient(t)

	_, err := c.GetTools(context.Background(),
		WithSearch("send email"),
		WithTags("Actions", "Production"),
	)
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- ExecuteTool ----------

func TestExecuteTool_StringContent(t *testing.T) {
	m := newMockServer(t)
	m.on("POST /tools/execute/GITHUB_LIST_REPOS", func(w http.ResponseWriter, r *http.Request) {
		var body executeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.ConnectedAccountID != "conn_1" {
			t.Errorf("ConnectedAccountID = %q", body.ConnectedAccountID)
		}
		if body.UserID != "user-1" {
			t.Errorf("UserID = %q", body.UserID)
		}
		if body.Version != "latest" {
			t.Errorf("Version = %q, want latest", body.Version)
		}
		if !body.AllowTracing {
			t.Error("AllowTracing = false, want true")
		}
		writeJSON(t, w, map[string]any{
			"executed": true,
			"data":     "repo count: 42",
		})
	})
	c := m.mustClient(t)

	got, err := c.ExecuteTool(context.Background(), "GITHUB_LIST_REPOS",
		map[string]any{"owner": "octocat"},
		"conn_1", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Success {
		t.Errorf("Success = false; error = %+v", got.Error)
	}
	if got.Content != "repo count: 42" {
		t.Errorf("Content = %q", got.Content)
	}
}

func TestExecuteTool_ArrayContent(t *testing.T) {
	m := newMockServer(t)
	m.on("POST /tools/execute/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"executed": true,
			"data": []any{
				"type-text-style",
				map[string]any{"type": "text", "text": "second block"},
				map[string]any{"type": "image", "url": "ignored"},
			},
		})
	})
	c := m.mustClient(t)

	got, err := c.ExecuteTool(context.Background(), "SOME_TOOL", nil, "conn", "u")
	if err != nil {
		t.Fatal(err)
	}
	// First array item is a plain string.
	// Second is a {type:"text", text:...} block — only Text is surfaced.
	// Third is a non-text object — JSON-stringified.
	want := "type-text-style\nsecond block\n" + `{"type":"image","url":"ignored"}`
	if got.Content != want {
		t.Errorf("Content = %q, want %q", got.Content, want)
	}
}

func TestExecuteTool_AuthErrorReturnsStructuredFailure(t *testing.T) {
	m := newMockServer(t)
	// Replace the server so the auth check fails for the execute path
	// even though our token is correct.
	m.server.Close()
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token revoked"}`))
	}))
	t.Cleanup(m.server.Close)

	c, err := NewComposer(testToken, WithBaseURL(m.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ExecuteTool(context.Background(), "SOME_TOOL", nil, "conn", "u")
	if err != nil {
		t.Fatalf("err = %v, want nil (auth errors are in-band)", err)
	}
	if got.Success {
		t.Error("Success = true, want false")
	}
	if got.Error == nil || got.Error.Code != "COMPOSIO_AUTH_ERROR" {
		t.Errorf("Error = %+v", got.Error)
	}
}

func TestExecuteTool_ServerErrorReturnsStructuredFailure(t *testing.T) {
	m := newMockServer(t)
	m.server.Close()
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(m.server.Close)

	c, err := NewComposer(testToken, WithBaseURL(m.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ExecuteTool(context.Background(), "SOME_TOOL", nil, "conn", "u")
	if err != nil {
		t.Fatalf("err = %v, want nil (5xx errors are in-band for ExecuteTool)", err)
	}
	if got.Success {
		t.Error("Success = true, want false")
	}
	if got.Error == nil || got.Error.Code != "COMPOSIO_ERROR" {
		t.Errorf("Error = %+v", got.Error)
	}
}

func TestExecuteTool_EmptySlug(t *testing.T) {
	m := newMockServer(t)
	c := m.mustClient(t)
	_, err := c.ExecuteTool(context.Background(), "", nil, "", "")
	if err == nil {
		t.Fatal("err = nil, want validation error")
	}
}

// ---------- collapseContent unit tests ----------

func TestCollapseContent(t *testing.T) {
	cases := []struct {
		name string
		in   []string // raw JSON payloads, tried in order
		want string
	}{
		{"empty", nil, ""},
		{"null only", []string{"null"}, ""},
		{"string", []string{`"hello"`}, "hello"},
		{"object", []string{`{"a":1}`}, `{"a":1}`},
		{"array of strings", []string{`["a","b"]`}, "a\nb"},
		{"array of text blocks", []string{`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`}, "first\nsecond"},
		{"array of mixed", []string{`["plain",{"type":"text","text":"block"},{"k":"v"}]`}, `plain
block
{"k":"v"}`},
		{"first null, second string", []string{"null", `"second"`}, "second"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payloads := make([]json.RawMessage, len(tc.in))
			for i, s := range tc.in {
				payloads[i] = json.RawMessage(s)
			}
			got := collapseContent(payloads...)
			if got != tc.want {
				t.Errorf("collapseContent(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------- parseAPIError unit tests ----------

func TestParseAPIError(t *testing.T) {
	t.Run("401 with code", func(t *testing.T) {
		e := parseAPIError(http.StatusUnauthorized, []byte(`{"code":"AUTH_ERROR","message":"token revoked"}`))
		if !e.IsAuthError() {
			t.Error("IsAuthError = false, want true")
		}
		if e.IsRetryable() {
			t.Error("IsRetryable = true, want false")
		}
		if e.Code != "AUTH_ERROR" {
			t.Errorf("Code = %q", e.Code)
		}
	})
	t.Run("401 without code infers AUTH_ERROR", func(t *testing.T) {
		e := parseAPIError(http.StatusUnauthorized, []byte(`{"message":"unauthorized"}`))
		if e.Code != "AUTH_ERROR" {
			t.Errorf("Code = %q, want AUTH_ERROR", e.Code)
		}
	})
	t.Run("429 retryable", func(t *testing.T) {
		e := parseAPIError(http.StatusTooManyRequests, []byte(`{"message":"slow down"}`))
		if !e.IsRetryable() {
			t.Error("IsRetryable = false, want true")
		}
		if e.IsAuthError() {
			t.Error("IsAuthError = true, want false")
		}
	})
	t.Run("500 retryable", func(t *testing.T) {
		e := parseAPIError(http.StatusInternalServerError, []byte(`internal error`))
		if !e.IsRetryable() {
			t.Error("IsRetryable = false, want true")
		}
	})
	t.Run("empty body falls back to status text", func(t *testing.T) {
		e := parseAPIError(http.StatusBadGateway, nil)
		if e.Code != "HTTP_502" {
			t.Errorf("Code = %q", e.Code)
		}
		if e.Message != http.StatusText(http.StatusBadGateway) {
			t.Errorf("Message = %q", e.Message)
		}
	})
	t.Run("error_code field used when code absent", func(t *testing.T) {
		e := parseAPIError(http.StatusUnprocessableEntity, []byte(`{"error_code":"validation_error","message":"bad input"}`))
		if e.Code != "validation_error" {
			t.Errorf("Code = %q", e.Code)
		}
	})
}

// ---------- catalog ----------

func TestCatalog(t *testing.T) {
	if len(COMPOSIO_APP_TYPES) < 15 {
		t.Errorf("COMPOSIO_APP_TYPES has %d entries, want at least 15", len(COMPOSIO_APP_TYPES))
	}
	cases := []struct {
		identifier string
		slug       string
	}{
		{"gmail", "GMAIL"},
		{"google-calendar", "GOOGLECALENDAR"},
		{"slack", "SLACK"},
		{"google-drive", "GOOGLEDRIVE"},
	}
	for _, tc := range cases {
		t.Run("identifier/"+tc.identifier, func(t *testing.T) {
			app := GetAppByIdentifier(tc.identifier)
			if app == nil {
				t.Fatalf("GetAppByIdentifier(%q) = nil", tc.identifier)
			}
			if app.AppSlug != tc.slug {
				t.Errorf("AppSlug = %q, want %q", app.AppSlug, tc.slug)
			}
			if !IsKnownIdentifier(tc.identifier) {
				t.Errorf("IsKnownIdentifier(%q) = false", tc.identifier)
			}
		})
		t.Run("slug/"+tc.slug, func(t *testing.T) {
			app := GetAppBySlug(tc.slug)
			if app == nil {
				t.Fatalf("GetAppBySlug(%q) = nil", tc.slug)
			}
			if app.Identifier != tc.identifier {
				t.Errorf("Identifier = %q, want %q", app.Identifier, tc.identifier)
			}
			// Case-insensitive lookup must also work.
			app2 := GetAppBySlug(strings.ToLower(tc.slug))
			if app2 == nil || app2.AppSlug != tc.slug {
				t.Errorf("case-insensitive GetAppBySlug(%q) failed", strings.ToLower(tc.slug))
			}
		})
	}

	t.Run("unknown identifier returns nil", func(t *testing.T) {
		if GetAppByIdentifier("nope-not-real") != nil {
			t.Error("expected nil")
		}
		if IsKnownIdentifier("nope-not-real") {
			t.Error("IsKnownIdentifier returned true")
		}
	})
}

func TestNormaliseSlug(t *testing.T) {
	cases := map[string]string{
		"github":          "GITHUB",
		"GITHUB":          "GITHUB",
		"google-calendar": "GOOGLECALENDAR",
		"GOOGLECALENDAR":  "GOOGLECALENDAR",
		"google_calendar": "GOOGLECALENDAR",
		"slack":           "SLACK",
		"":                "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := NormaliseSlug(in)
			if got != want {
				t.Errorf("NormaliseSlug(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// ---------- Composio matches Authorizer interface ----------

func TestComposio_ImplementsAuthorizer(t *testing.T) {
	var _ Authorizer = (*Composio)(nil)
}

// ---------- request signing ----------

func TestRequestSetsAPIKeyHeader(t *testing.T) {
	var gotHeader string
	m := newMockServer(t)
	m.on("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(APIHeader)
		writeJSON(t, w, map[string]any{"items": []any{}})
	})
	c := m.mustClient(t)

	_, _ = c.GetTools(context.Background())
	if gotHeader != testToken {
		t.Errorf("APIHeader = %q, want %q", gotHeader, testToken)
	}
}

// ---------- url encoding ----------

func TestURLEncoding_SpecialCharsInID(t *testing.T) {
	var gotRawPath string
	m := newMockServer(t)
	m.on("DELETE /connected_accounts/", func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is always decoded by net/http; check r.URL.RawPath
		// for the encoded form so we can verify our url.PathEscape call.
		gotRawPath = r.URL.RawPath
		w.WriteHeader(http.StatusNoContent)
	})
	c := m.mustClient(t)

	// An id with a slash should be escaped so it stays a single path
	// segment — otherwise the server would route it to a different
	// handler. We don't claim Composio issues such IDs today, but the
	// escape protects us against future ID format changes.
	if err := c.DeleteConnection(context.Background(), "conn/with/slash"); err != nil {
		t.Fatal(err)
	}
	want := "/connected_accounts/" + url.PathEscape("conn/with/slash")
	if gotRawPath != want {
		t.Errorf("gotRawPath = %q, want %q", gotRawPath, want)
	}
}

// ---------- silence unused warnings on helpers ----------

var _ = reflect.DeepEqual
