package eino

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsvc "egent-lobehub/mcp"
)

func TestMCPTool_Info_NoSchema(t *testing.T) {
	mgr := mcpsvc.NewManager()
	tool := &MCPTool{
		manager:    mgr,
		serverName: "test",
		mcpTool: &mcp.Tool{
			Name:        "test_tool",
			Description: "A test tool",
		},
	}

	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "test_tool" {
		t.Errorf("name = %q, want %q", info.Name, "test_tool")
	}
	if info.Desc != "A test tool" {
		t.Errorf("desc = %q, want %q", info.Desc, "A test tool")
	}
	if info.ParamsOneOf != nil {
		t.Error("expected nil ParamsOneOf for tool without InputSchema")
	}
}

func TestMCPTool_Info_WithSchema(t *testing.T) {
	mgr := mcpsvc.NewManager()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query",
			},
		},
		"required": []any{"query"},
	}

	tool := &MCPTool{
		manager:    mgr,
		serverName: "test",
		mcpTool: &mcp.Tool{
			Name:        "search",
			Description: "Search something",
			InputSchema: schema,
		},
	}

	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "search" {
		t.Errorf("name = %q, want %q", info.Name, "search")
	}
	if info.ParamsOneOf == nil {
		t.Error("expected non-nil ParamsOneOf for tool with InputSchema")
	}
}

func TestMCPTool_Info_InvalidSchema(t *testing.T) {
	mgr := mcpsvc.NewManager()
	tool := &MCPTool{
		manager:    mgr,
		serverName: "test",
		mcpTool: &mcp.Tool{
			Name:        "bad_schema",
			Description: "Bad schema tool",
			InputSchema: "not-a-valid-schema-object",
		},
	}

	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "bad_schema" {
		t.Errorf("name = %q, want %q", info.Name, "bad_schema")
	}
}

func TestMCPTool_InvokableRun_InvalidArgs(t *testing.T) {
	mgr := mcpsvc.NewManager()
	tool := &MCPTool{
		manager:    mgr,
		serverName: "test",
		mcpTool: &mcp.Tool{
			Name: "test_tool",
		},
	}

	_, err := tool.InvokableRun(context.Background(), "not-json")
	if err == nil {
		t.Error("expected error for invalid JSON args")
	}
}

func TestMCPTool_InvokableRun_ServerNotFound(t *testing.T) {
	mgr := mcpsvc.NewManager()
	tool := &MCPTool{
		manager:    mgr,
		serverName: "nonexistent",
		mcpTool: &mcp.Tool{
			Name: "test_tool",
		},
	}

	_, err := tool.InvokableRun(context.Background(), `{"key": "value"}`)
	if err == nil {
		t.Error("expected error when server not found")
	}
}

func TestFormatMCPResult(t *testing.T) {
	tests := []struct {
		name   string
		result *mcp.CallToolResult
		want   string
	}{
		{"nil result", nil, ""},
		{"empty content", &mcp.CallToolResult{}, ""},
		{"text content", &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "hello"},
			},
		}, "hello"},
		{"multiple texts", &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "line1"},
				&mcp.TextContent{Text: "line2"},
			},
		}, "line1\nline2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatMCPResult(tt.result)
			if tt.want != "" && got != tt.want {
				t.Errorf("formatMCPResult() = %q, want %q", got, tt.want)
			}
			if tt.want == "" && got != "" {
				if _, err := json.Marshal(tt.result); err != nil {
					return
				}
			}
		})
	}
}

func TestBuildTools_EmptyManager(t *testing.T) {
	mgr := mcpsvc.NewManager()
	tools := BuildTools(mgr)
	if len(tools) != 0 {
		t.Errorf("expected 0 tools from empty manager, got %d", len(tools))
	}
}
