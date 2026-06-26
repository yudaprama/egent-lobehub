package palace

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"egent-lobehub/memory"
)

// searchFake records the SearchInput it receives and returns canned
// results, so the tool layer can be tested without a database.
type searchFake struct {
	fakeStore
	gotUserID string
	gotInput  SearchInput
	results   []SearchResult
	err       error
}

func (f *searchFake) Search(_ context.Context, userID string, in SearchInput) ([]SearchResult, error) {
	f.gotUserID = userID
	f.gotInput = in
	return f.results, f.err
}

func newSearchTool(f *searchFake) *SearchTool { return NewSearchTool(f) }

func TestSearchTool_HappyPath(t *testing.T) {
	f := &searchFake{results: []SearchResult{
		{BaseMemory: BaseMemory{MemoryLayer: LayerPreference, Summary: "prefers dark mode", Tags: []string{"ui"}}, Score: 0.91},
		{BaseMemory: BaseMemory{MemoryLayer: LayerIdentity, Title: "Alice"}},
	}}
	ctx := memory.WithUserID(context.Background(), "user-1")

	out, err := newSearchTool(f).InvokableRun(ctx, `{"query":"theme settings","layers":"preference, identity","limit":5}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if f.gotUserID != "user-1" {
		t.Errorf("userID = %q, want user-1", f.gotUserID)
	}
	if f.gotInput.Query != "theme settings" {
		t.Errorf("query = %q", f.gotInput.Query)
	}
	if !reflect.DeepEqual(f.gotInput.Layers, []string{"preference", "identity"}) {
		t.Errorf("layers = %#v, want [preference identity]", f.gotInput.Layers)
	}
	if f.gotInput.Limit != 5 {
		t.Errorf("limit = %d, want 5", f.gotInput.Limit)
	}
	if !strings.Contains(out, "dark mode") || !strings.Contains(out, "relevance 0.91") {
		t.Errorf("formatted output missing expected content:\n%s", out)
	}
	if !strings.Contains(out, "[identity] Alice") {
		t.Errorf("output should fall back to title when summary empty:\n%s", out)
	}
}

func TestSearchTool_DefaultLimit(t *testing.T) {
	f := &searchFake{}
	ctx := memory.WithUserID(context.Background(), "user-1")
	if _, err := newSearchTool(f).InvokableRun(ctx, `{"query":"x"}`); err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if f.gotInput.Limit != 10 {
		t.Errorf("default limit = %d, want 10", f.gotInput.Limit)
	}
	if f.gotInput.Layers != nil {
		t.Errorf("layers should be nil when omitted, got %#v", f.gotInput.Layers)
	}
}

func TestSearchTool_EmptyResults(t *testing.T) {
	f := &searchFake{results: nil}
	ctx := memory.WithUserID(context.Background(), "user-1")
	out, err := newSearchTool(f).InvokableRun(ctx, `{"query":"nothing here"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, "No memories found") {
		t.Errorf("want 'No memories found', got %q", out)
	}
}

func TestSearchTool_MissingUserID(t *testing.T) {
	f := &searchFake{}
	_, err := newSearchTool(f).InvokableRun(context.Background(), `{"query":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "user_id") {
		t.Errorf("want user_id error, got %v", err)
	}
}

func TestSearchTool_BlankQuery(t *testing.T) {
	f := &searchFake{}
	ctx := memory.WithUserID(context.Background(), "user-1")
	_, err := newSearchTool(f).InvokableRun(ctx, `{"query":"   "}`)
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Errorf("want query-required error, got %v", err)
	}
}

func TestSearchTool_NilStore(t *testing.T) {
	ctx := memory.WithUserID(context.Background(), "user-1")
	out, err := NewSearchTool(nil).InvokableRun(ctx, `{"query":"x"}`)
	if err != nil {
		t.Fatalf("nil store should not error: %v", err)
	}
	if !strings.Contains(out, "not configured") {
		t.Errorf("want 'not configured', got %q", out)
	}
}

func TestSearchTool_BadJSON(t *testing.T) {
	f := &searchFake{}
	ctx := memory.WithUserID(context.Background(), "user-1")
	if _, err := newSearchTool(f).InvokableRun(ctx, `{not json}`); err == nil {
		t.Error("want parse error for bad JSON")
	}
}

func TestSearchTool_Info(t *testing.T) {
	info, err := NewSearchTool(&searchFake{}).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "memory_palace_search" {
		t.Errorf("tool name = %q", info.Name)
	}
}

func TestInPlaceholders(t *testing.T) {
	idx := 3
	var args []any
	got := inPlaceholders([]string{"a", "b", "c"}, &idx, &args)
	if got != "$3,$4,$5" {
		t.Errorf("placeholders = %q, want $3,$4,$5", got)
	}
	if idx != 6 {
		t.Errorf("idx = %d, want 6", idx)
	}
	if !reflect.DeepEqual(args, []any{"a", "b", "c"}) {
		t.Errorf("args = %#v", args)
	}
}

func TestDeref(t *testing.T) {
	s := "hi"
	if deref(&s) != "hi" {
		t.Error("deref(&s) should return value")
	}
	if deref(nil) != "" {
		t.Error("deref(nil) should return empty string")
	}
}

func TestFormatSearchResults(t *testing.T) {
	out := formatSearchResults([]SearchResult{
		{BaseMemory: BaseMemory{MemoryLayer: LayerActivity, Summary: "ran 5k"}, Score: 0.5},
		{BaseMemory: BaseMemory{MemoryLayer: LayerContext, Details: "only details"}},
	})
	if !strings.Contains(out, "1. [activity] ran 5k (relevance 0.50)") {
		t.Errorf("first line wrong:\n%s", out)
	}
	if !strings.Contains(out, "2. [context] only details") {
		t.Errorf("second line should use details fallback:\n%s", out)
	}
	if strings.Contains(out, "(relevance") && strings.Count(out, "relevance") != 1 {
		t.Errorf("zero-score result should omit relevance:\n%s", out)
	}
}

// TestPgStore_Search_Integration exercises the real cosine-ranked query
// against Postgres + pgvector. It is skipped unless PALACE_TEST_DSN points
// at a database with the user_memories schema and the vector extension.
func TestPgStore_Search_Integration(t *testing.T) {
	dsn := os.Getenv("PALACE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PALACE_TEST_DSN to run the palace search integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	store := NewPgStore(pool, nil) // nil embedder → recency + ILIKE fallback
	const uid = "palace-search-it"
	t.Cleanup(func() { _ = store.DeleteAll(ctx, uid) })
	_ = store.DeleteAll(ctx, uid)

	if _, err := store.CreatePreference(ctx, uid, PreferenceInput{ConclusionDirectives: "always use dark mode"}); err != nil {
		t.Fatalf("seed preference: %v", err)
	}
	if _, err := store.CreateIdentity(ctx, uid, IdentityInput{Description: "lives in Berlin"}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	// Fallback path: ILIKE narrows to the matching row.
	res, err := store.Search(ctx, uid, SearchInput{Query: "dark mode"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || !strings.Contains(res[0].Summary, "dark mode") {
		t.Fatalf("ILIKE search returned %d rows: %#v", len(res), res)
	}

	// Layer filter narrows independently of text.
	res, err = store.Search(ctx, uid, SearchInput{Query: "Berlin", Layers: []string{"identity"}})
	if err != nil {
		t.Fatalf("layer search: %v", err)
	}
	if len(res) != 1 || res[0].MemoryLayer != LayerIdentity {
		t.Fatalf("layer search returned %#v", res)
	}
}
