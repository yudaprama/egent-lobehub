package runtime

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type stubTool struct {
	name string
	desc string
}

func (s *stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: s.desc}, nil
}

func (s *stubTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	return "stub", nil
}

func newStub(name string) tool.BaseTool {
	return &stubTool{name: name, desc: "stub " + name}
}

func TestContextBuilder_BuildsDefaultPrompt(t *testing.T) {
	b := NewContextBuilder("Be helpful.")
	got := b.Build()
	if got != "Be helpful.\n\n" {
		t.Errorf("expected default prompt, got %q", got)
	}
}

func TestContextBuilder_WithMemory(t *testing.T) {
	b := NewContextBuilder("core")
	b.WithMemory("[User Memory]\n- name: Alice")
	got := b.Build()
	if !contains(got, "[User Memory]") {
		t.Error("expected memory header in prompt")
	}
	if !contains(got, "name: Alice") {
		t.Error("expected memory content in prompt")
	}
}

func TestContextBuilder_WithAllBlocks(t *testing.T) {
	b := NewContextBuilder("core")
	b.WithMemory("memory")
	b.WithPersona("developer who likes Go")
	b.WithDocuments("file content here")
	b.WithSkillHints([]string{"image-generation: enabled"})
	b.WithExtraBlock("Custom", "custom content")
	got := b.Build()
	for _, s := range []string{"core", "User Persona", "memory", "Attached Documents", "Available Skills", "Custom"} {
		if !contains(got, s) {
			t.Errorf("expected %q in prompt, got: %q", s, got)
		}
	}
}

func TestContextBuilder_EmptyMemorySkipped(t *testing.T) {
	b := NewContextBuilder("core").WithMemory("")
	if contains(b.Build(), "[User Memory]") {
		t.Error("empty memory should not appear")
	}
}

func TestBuildPrompt_SkipsEmpty(t *testing.T) {
	got := BuildPrompt([]PromptSection{
		{Label: "A", Content: "a-content"},
		{Label: "B", Content: ""},
		{Label: "C", Content: "c-content"},
	})
	if !contains(got, "a-content") || !contains(got, "c-content") {
		t.Errorf("empty sections should be skipped, got: %q", got)
	}
	if contains(got, "[B]") {
		t.Error("empty section label should not appear")
	}
}

func TestTruncatePrompt_LongerThanLimit(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	got := TruncatePrompt(string(long), 50)
	if len(got) <= 50 {
		t.Errorf("truncated output should exceed 50 due to notice, got %d", len(got))
	}
	if !contains(got, "Prompt truncated") {
		t.Error("expected truncation notice")
	}
}

func TestTruncatePrompt_ShorterThanLimit(t *testing.T) {
	got := TruncatePrompt("short", 100)
	if got != "short" {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestToolResolver_RegisterAndResolve(t *testing.T) {
	r := NewToolResolver()
	_ = r.Register(newStub("tool_a"), "tool_a", ToolSourceBuiltin)
	_ = r.Register(newStub("tool_b"), "tool_b", ToolSourceMCP)

	if r.Len() != 2 {
		t.Errorf("expected 2 tools, got %d", r.Len())
	}

	tools := r.Resolve(context.Background())
	if len(tools) != 2 {
		t.Errorf("expected 2 resolved tools, got %d", len(tools))
	}
}

func TestToolResolver_DeterministicOrder(t *testing.T) {
	r := NewToolResolver()
	_ = r.Register(newStub("zebra"), "zebra", ToolSourceBuiltin)
	_ = r.Register(newStub("apple"), "apple", ToolSourceBuiltin)
	_ = r.Register(newStub("monkey"), "monkey", ToolSourceBuiltin)

	names := r.ListIdentifiers()
	want := []string{"apple", "monkey", "zebra"}
	if len(names) != len(want) {
		t.Fatalf("expected %d tools, got %d", len(want), len(names))
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("position %d: want %q, got %q", i, n, names[i])
		}
	}
}

func TestToolResolver_ResolveBySource(t *testing.T) {
	r := NewToolResolver()
	_ = r.Register(newStub("builtin_1"), "b1", ToolSourceBuiltin)
	_ = r.Register(newStub("mcp_1"), "m1", ToolSourceMCP)
	_ = r.Register(newStub("mcp_2"), "m2", ToolSourceMCP)

	mcpTools := r.ResolveBySource(context.Background(), ToolSourceMCP)
	if len(mcpTools) != 2 {
		t.Errorf("expected 2 mcp tools, got %d", len(mcpTools))
	}
}

func TestToolResolver_RegisterInvalidFails(t *testing.T) {
	r := NewToolResolver()
	bad := &badTool{}
	err := r.Register(bad, "bad", ToolSourceBuiltin)
	if err == nil {
		t.Error("expected error from invalid tool")
	}
}

type badTool struct{}

func (b *badTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return nil, nil
}

func (b *badTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	return "", nil
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
