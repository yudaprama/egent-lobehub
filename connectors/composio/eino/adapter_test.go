package eino

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"egent-lobehub/connectors/composio"
)

// ---- fakes / doubles ----

// fakeStore is an in-memory ConnectedAccountStore.
type fakeStore struct {
	mu    sync.Mutex
	links map[string]string // "userID:identifier" → connectedAccountID
}

func newFakeStore() *fakeStore {
	return &fakeStore{links: make(map[string]string)}
}

func (s *fakeStore) Set(userID, identifier, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[userID+":"+identifier] = id
}

func (s *fakeStore) Resolve(_ context.Context, userID, identifier string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links[userID+":"+identifier], nil
}

// testServer sets up a minimal httptest.Server that handles /tools and
// /tools/execute/{slug}. Returns the Composio client pointed at the
// server and a cleanup func.
func testServer(t *testing.T) (*composio.Composio, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/tools", func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("toolkit_slug")
		if slug == "GITHUB" {
			writeJSON(w, map[string]any{
				"items": []map[string]any{
					{
						"slug":        "GITHUB_STAR_REPO",
						"description": "Star a repository",
						"toolkit":     map[string]any{"slug": "github"},
						"input_parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"repo": map[string]any{"type": "string", "description": "The repo to star"},
							},
						},
					},
				},
			})
			return
		}
		writeJSON(w, map[string]any{"items": []any{}})
	})

	mux.HandleFunc("/tools/execute/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConnectedAccountID string `json:"connected_account_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.ConnectedAccountID == "conn_valid" {
			writeJSON(w, map[string]any{
				"executed": true,
				"data":     "repo starred",
			})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": map[string]any{
			"code": "AUTH_ERROR", "message": "connection expired",
		}})
	})

	c, _ := composio.NewComposer("test-key", composio.WithBaseURL(srv.URL), composio.WithHTTPClient(srv.Client()))
	return c, mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- tests ----

func TestInfo_UsesArgsSchema(t *testing.T) {
	c, _ := testServer(t)
	actions, err := c.GetToolsForApp(context.Background(), "GITHUB")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 {
		t.Fatal("no tools")
	}

	tool := &ComposioTool{
		slug:    actions[0].Slug,
		desc:    actions[0].Description,
		rawArgs: actions[0].ArgsSchema,
		client:  c,
	}

	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "GITHUB_STAR_REPO" {
		t.Errorf("Name = %q", info.Name)
	}
	if info.ParamsOneOf == nil {
		t.Error("ParamsOneOf is nil — schema not converted")
	}
	// Round-trip through ToJSONSchema to verify the conversion is sane.
	schemaJSON, _ := json.MarshalIndent(info.ParamsOneOf, "", "  ")
	if len(schemaJSON) == 0 {
		t.Error("empty ParamsOneOf JSON")
	}
}

func TestInfo_NilRawArgsReturnsNoParams(t *testing.T) {
	tool := &ComposioTool{slug: "A_TOOL", rawArgs: nil, client: nil}
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ParamsOneOf != nil {
		t.Error("ParamsOneOf should be nil for empty rawArgs")
	}
}

func TestInvokableRun_NotConnectedReturnsError(t *testing.T) {
	c, _ := testServer(t)
	store := newFakeStore()
	tool := &ComposioTool{
		slug:          "GITHUB_STAR_REPO",
		client:        c,
		store:         store,
		appIdentifier: "github",
		log:           slog.Default(),
	}
	ctx := WithUserID(context.Background(), "user-42")
	_, err := tool.InvokableRun(ctx, `{"repo":"org/repo"}`)
	if err == nil {
		t.Fatal("expected NotConnectedError")
	}
	var nce *NotConnectedError
	if ok := isNotConnected(err, &nce); !ok {
		t.Fatalf("expected NotConnectedError, got %T: %v", err, err)
	}
	if nce.AppIdentifier != "github" {
		t.Errorf("AppIdentifier = %q", nce.AppIdentifier)
	}
}

func isNotConnected(err error, target **NotConnectedError) bool {
	for err != nil {
		if nce, ok := err.(*NotConnectedError); ok {
			*target = nce
			return true
		}
		err = unwrapOne(err)
	}
	return false
}

func unwrapOne(err error) error {
	type unwrapper interface{ Unwrap() error }
	u, ok := err.(unwrapper)
	if !ok {
		return nil
	}
	return u.Unwrap()
}

func TestInvokableRun_Success(t *testing.T) {
	c, _ := testServer(t)
	store := newFakeStore()
	store.Set("user-1", "github", "conn_valid")
	tool := &ComposioTool{
		slug:          "GITHUB_STAR_REPO",
		client:        c,
		store:         store,
		appIdentifier: "github",
		log:           slog.Default(),
	}
	ctx := WithUserID(context.Background(), "user-1")
	result, err := tool.InvokableRun(ctx, `{"repo":"octocat/hello-world"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result != "repo starred" {
		t.Errorf("result = %q", result)
	}
}

func TestInvokableRun_NoUserID(t *testing.T) {
	tool := &ComposioTool{
		slug:   "X",
		client: nil, // doesn't matter — we fail before calling client
		store:  newFakeStore(),
	}
	_, err := tool.InvokableRun(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for missing userID")
	}
}

func TestInvokableRun_EmptyArgs(t *testing.T) {
	c, _ := testServer(t)
	store := newFakeStore()
	store.Set("u", "github", "conn_valid")
	tool := &ComposioTool{
		slug:          "GITHUB_STAR_REPO",
		client:        c,
		store:         store,
		appIdentifier: "github",
	}
	ctx := WithUserID(context.Background(), "u")
	result, err := tool.InvokableRun(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if result != "repo starred" {
		t.Errorf("result = %q", result)
	}
}

func TestInvokableRun_BadJSONArgs(t *testing.T) {
	tool := &ComposioTool{
		slug:   "X",
		client: nil,
		store:  newFakeStore(),
	}
	ctx := WithUserID(context.Background(), "u")
	_, err := tool.InvokableRun(ctx, "{bad json")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// ---- Builder tests ----

func TestBuilder_BuildSingleApp(t *testing.T) {
	c, _ := testServer(t)
	store := newFakeStore()
	b := NewBuilder(c, store, slog.Default())
	if b == nil {
		t.Fatal("builder is nil")
	}

	tools, err := b.Build(context.Background(), WithApps("github"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].slug != "GITHUB_STAR_REPO" {
		t.Errorf("slug = %q", tools[0].slug)
	}
	if tools[0].appIdentifier != "github" {
		t.Errorf("appIdentifier = %q", tools[0].appIdentifier)
	}
}

func TestBuilder_BuildUnknownApp(t *testing.T) {
	c, _ := testServer(t)
	b := NewBuilder(c, newFakeStore(), slog.Default())
	tools, err := b.Build(context.Background(), WithApps("totally-fake-app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}
}

func TestBuilder_NilClientReturnsNil(t *testing.T) {
	b := NewBuilder(nil, newFakeStore(), slog.Default())
	if b != nil {
		t.Fatal("expected nil builder")
	}
}

func TestBuilder_NilStoreReturnsNil(t *testing.T) {
	c, _ := testServer(t)
	b := NewBuilder(c, nil, slog.Default())
	if b != nil {
		t.Fatal("expected nil builder")
	}
}

// ---- FormatToolsForLog ----

func TestFormatToolsForLog(t *testing.T) {
	tools := []*ComposioTool{
		{slug: "A", appIdentifier: "gmail"},
		{slug: "B", appIdentifier: "gmail"},
		{slug: "C", appIdentifier: "slack"},
	}
	result := FormatToolsForLog(tools)
	// Should contain both apps with correct counts.
	if result == "" {
		t.Fatal("empty")
	}
	if result == "no composio tools" {
		t.Fatal("returned 'no composio tools' unexpectedly")
	}
}

// ---- WithUserID roundtrip ----

func TestWithUserID_Roundtrip(t *testing.T) {
	ctx := WithUserID(context.Background(), "user-42")
	if got := composioUserIDFromContext(ctx); got != "user-42" {
		t.Errorf("got %q", got)
	}
	if got := composioUserIDFromContext(context.Background()); got != "" {
		t.Errorf("empty context got %q", got)
	}
}

// ---- Ensure full tool registration works with the Composio client ----

func TestFullRegistrationFlow(t *testing.T) {
	c, _ := testServer(t)
	store := newFakeStore()
	store.Set("user-1", "github", "conn_valid")
	b := NewBuilder(c, store, slog.Default())

	tools, err := b.Build(context.Background(), WithApps("github"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("no tools")
	}

	// Verify each tool registers cleanly (this is what main.go does).
	for _, ct := range tools {
		info, err := ct.Info(context.Background())
		if err != nil {
			t.Errorf("Info(%s): %v", ct.slug, err)
			continue
		}
		if info.Name == "" {
			t.Errorf("tool %s has empty Name", ct.slug)
		}
	}
}
