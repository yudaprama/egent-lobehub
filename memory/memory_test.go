package memory

import (
	"context"
	"strings"
	"testing"
)

func TestInMemoryStore_SetGet(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()

	if err := s.Set(ctx, "user-1", "name", "Alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := s.Get(ctx, "user-1", "name")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected entry, got nil")
	}
	if got.Value != "Alice" {
		t.Errorf("expected 'Alice', got %q", got.Value)
	}
}

func TestInMemoryStore_GetMissing(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	got, _ := s.Get(ctx, "user-1", "missing")
	if got != nil {
		t.Errorf("expected nil for missing key, got %+v", got)
	}
}

func TestInMemoryStore_SetUpdatesExisting(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	_ = s.Set(ctx, "u1", "k", "v1")
	_ = s.Set(ctx, "u1", "k", "v2")
	got, _ := s.Get(ctx, "u1", "k")
	if got.Value != "v2" {
		t.Errorf("expected 'v2', got %q", got.Value)
	}
}

func TestInMemoryStore_Delete(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	_ = s.Set(ctx, "u1", "k", "v")
	_ = s.Delete(ctx, "u1", "k")
	got, _ := s.Get(ctx, "u1", "k")
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestInMemoryStore_Search(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	_ = s.Set(ctx, "u1", "name", "Alice")
	_ = s.Set(ctx, "u1", "city", "Paris")
	_ = s.Set(ctx, "u1", "lang", "Go")

	results, _ := s.Search(ctx, "u1", "paris", 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Key != "city" {
		t.Errorf("expected key 'city', got %q", results[0].Key)
	}
}

func TestInMemoryStore_SearchEmptyForOtherUser(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	_ = s.Set(ctx, "u1", "name", "Alice")
	results, _ := s.Search(ctx, "u2", "alice", 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results for other user, got %d", len(results))
	}
}

func TestManager_RecallFormatsMemories(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	ctx := context.Background()
	_ = s.Set(ctx, "u1", "name", "Alice")
	_ = s.Set(ctx, "u1", "city", "Paris")

	out := mgr.Recall(ctx, "u1", "alice")
	if out == "" {
		t.Fatal("expected non-empty recall output")
	}
	if !strings.Contains(out, "[User Memory]") {
		t.Errorf("expected '[User Memory]' header, got: %q", out)
	}
}

func TestManager_RecallEmptyNoMemories(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	out := mgr.Recall(context.Background(), "u1", "anything")
	if out != "" {
		t.Errorf("expected empty string, got %q", out)
	}
}

func TestManager_ExtractAndStore_NamePattern(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	ctx := context.Background()

	_ = mgr.ExtractAndStore(ctx, "u1", "Hi, my name is Bob!")
	got, _ := s.Get(ctx, "u1", "user.name")
	if got == nil {
		t.Fatal("expected user.name to be stored")
	}
	if got.Value != "Bob" {
		t.Errorf("expected 'Bob', got %q", got.Value)
	}
}

func TestManager_ExtractAndStore_LocationPattern(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	ctx := context.Background()

	_ = mgr.ExtractAndStore(ctx, "u1", "I live in Tokyo")
	got, _ := s.Get(ctx, "u1", "user.location")
	if got == nil {
		t.Fatal("expected user.location to be stored")
	}
	if got.Value != "Tokyo" {
		t.Errorf("expected 'Tokyo', got %q", got.Value)
	}
}

func TestManager_ExtractAndStore_PreferencePattern(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	ctx := context.Background()

	_ = mgr.ExtractAndStore(ctx, "u1", "I prefer dark mode")
	got, _ := s.Get(ctx, "u1", "preferences.dark_mode")
	if got == nil {
		t.Fatal("expected preference to be stored")
	}
}

func TestManager_ExtractAndStore_NoFacts(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	ctx := context.Background()

	_ = mgr.ExtractAndStore(ctx, "u1", "The weather is nice today")
	list, _ := s.List(ctx, "u1")
	if len(list) != 0 {
		t.Errorf("expected 0 entries, got %d", len(list))
	}
}

func TestManager_HooksFire(t *testing.T) {
	s := NewInMemoryStore()
	mgr := NewManager(s)
	ctx := context.Background()

	extractCalled := false
	mgr.OnExtract = func(_ string, _ int) { extractCalled = true }

	recallCalled := false
	mgr.OnRecall = func(_ string, _ string, _ int) { recallCalled = true }

	_ = s.Set(ctx, "u1", "name", "Alice")
	_ = mgr.ExtractAndStore(ctx, "u1", "my name is Bob")
	_ = mgr.Recall(ctx, "u1", "bob")

	if !extractCalled {
		t.Error("OnExtract hook should have fired")
	}
	if !recallCalled {
		t.Error("OnRecall hook should have fired")
	}
}

func TestExtractHeuristic_ImShort(t *testing.T) {
	facts := extractHeuristic("I'm Alice and I like cats")
	if facts["user.name"] != "Alice" {
		t.Errorf("expected user.name=Alice, got %q", facts["user.name"])
	}
}

func TestExtractHeuristic_RejectsInvalidNames(t *testing.T) {
	facts := extractHeuristic("I'm not sure about that")
	if _, ok := facts["user.name"]; ok {
		t.Error("should not extract 'not' as a name")
	}
}

func TestFirstWord(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hello world", "hello"},
		{"Alice!", "Alice"},
		{"Bob, hi", "Bob"},
		{"single", "single"},
		{"  trimmed", "trimmed"},
	}
	for _, tt := range tests {
		got := firstWord(tt.in)
		if got != tt.want {
			t.Errorf("firstWord(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatMemories_Empty(t *testing.T) {
	if FormatMemories(nil) != "" {
		t.Error("expected empty string for nil")
	}
}
