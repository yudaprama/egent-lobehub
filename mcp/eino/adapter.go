package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpsvc "egent-lobehub/mcp"
)

// MCPTool wraps an MCP tool as an Eino InvokableTool.
// One instance per (server, tool) pair.
type MCPTool struct {
	manager    *mcpsvc.Manager
	serverName string
	mcpTool    *mcp.Tool
}

var _ tool.InvokableTool = (*MCPTool)(nil)

// Info returns the tool's metadata for Eino's tool registration.
func (t *MCPTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{
		Name: t.mcpTool.Name,
		Desc: t.mcpTool.Description,
	}
	if t.mcpTool.InputSchema != nil {
		raw, err := json.Marshal(t.mcpTool.InputSchema)
		if err != nil {
			return info, nil
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			return info, nil
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&s)
	}
	return info, nil
}

// InvokableRun executes the MCP tool via the manager.
func (t *MCPTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args map[string]any
	if argumentsInJSON != "" && argumentsInJSON != "{}" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("mcp: parse arguments for %s: %w", t.mcpTool.Name, err)
		}
	}

	result, err := t.manager.CallTool(ctx, t.serverName, t.mcpTool.Name, args)
	if err != nil {
		return "", fmt.Errorf("mcp: call %s/%s: %w", t.serverName, t.mcpTool.Name, err)
	}

	return formatMCPResult(result), nil
}

// formatMCPResult extracts text content from MCP tool result.
func formatMCPResult(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var parts []string
	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(result)
	return string(b)
}

// BuildTools creates Eino tools from all tools across all connected MCP servers.
func BuildTools(manager *mcpsvc.Manager) []tool.BaseTool {
	var tools []tool.BaseTool
	for serverName, conn := range manager.GetServers() {
		for _, mcpTool := range conn.Tools {
			tools = append(tools, &MCPTool{
				manager:    manager,
				serverName: serverName,
				mcpTool:    mcpTool,
			})
		}
	}
	return tools
}
