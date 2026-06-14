package middleware

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ConnectorToolPermission represents the permission level for a connector tool.
// Mirrors LobeHub's ConnectorToolPermission enum from database schemas.
type ConnectorToolPermission string

const (
	// PermissionEnabled means the tool can be called freely.
	PermissionEnabled ConnectorToolPermission = "enabled"

	// PermissionNeedsApproval means the tool requires human intervention before execution.
	PermissionNeedsApproval ConnectorToolPermission = "needs_approval"

	// PermissionDisabled means the tool cannot be called at all.
	PermissionDisabled ConnectorToolPermission = "disabled"
)

// PermissionChecker is a function type that checks tool permissions.
// Returns the permission level, or empty string if no permission entry exists.
type PermissionChecker func(ctx context.Context, identifier, toolName string) (ConnectorToolPermission, error)

// PermissionConfig holds the configuration for tool permission gating.
type PermissionConfig struct {
	// Checker is the function used to verify tool permissions.
	// If nil, all tools are allowed (no permission checking).
	Checker PermissionChecker

	// DisabledTools is a static set of tool identifiers that are always blocked.
	// Used as a fallback when no Checker is provided.
	DisabledTools map[string]bool
}

// PermissionGate wraps a tool.BaseTool with permission checking.
// If a tool is disabled, it returns a standardized blocked response instead
// of executing the tool. This mirrors LobeHub's ToolExecutionService permission gate.
type PermissionGate struct {
	inner      tool.BaseTool
	identifier string
	config     *PermissionConfig
}

// NewPermissionGate creates a permission-wrapped tool.
func NewPermissionGate(inner tool.BaseTool, identifier string, config *PermissionConfig) *PermissionGate {
	return &PermissionGate{
		inner:      inner,
		identifier: identifier,
		config:     config,
	}
}

// Info returns the tool info from the wrapped tool.
func (g *PermissionGate) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return g.inner.Info(ctx)
}

// InvokableRun checks permissions before executing the wrapped tool.
func (g *PermissionGate) InvokableRun(ctx context.Context, argumentsJSON string, opts ...tool.Option) (string, error) {
	if g.isDisabled(ctx) {
		return BuildBlockedToolResponse(g.toolName()), nil
	}

	invokable, ok := g.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("wrapped tool %s is not invokable", g.identifier)
	}

	return invokable.InvokableRun(ctx, argumentsJSON, opts...)
}

// IsInvokable checks if the wrapped tool is invokable.
func (g *PermissionGate) IsInvokable() bool {
	_, ok := g.inner.(tool.InvokableTool)
	return ok
}

// IsStreamable checks if the wrapped tool is streamable.
func (g *PermissionGate) IsStreamable() bool {
	_, ok := g.inner.(tool.StreamableTool)
	return ok
}

// toolName extracts the tool name from the tool info.
func (g *PermissionGate) toolName() string {
	info, err := g.inner.Info(context.Background())
	if err != nil || info == nil {
		return g.identifier
	}
	return info.Name
}

// isDisabled checks whether the tool should be blocked from execution.
func (g *PermissionGate) isDisabled(ctx context.Context) bool {
	if g.config == nil {
		return false
	}

	// Check static disabled list first
	if g.config.DisabledTools != nil && g.config.DisabledTools[g.identifier] {
		return true
	}

	// Check via permission checker function
	if g.config.Checker != nil {
		perm, err := g.config.Checker(ctx, g.identifier, g.toolName())
		if err != nil {
			// Never block execution due to checker error (mirrors LobeHub behavior)
			return false
		}
		return perm == PermissionDisabled
	}

	return false
}

// BuildBlockedToolResponse creates a standardized blocked-tool response.
// Ported from lobehub/src/libs/mcp/connectorPermissionCheck.ts
func BuildBlockedToolResponse(toolName string) string {
	return fmt.Sprintf(
		"The tool %q has been disabled by the user and cannot be executed. "+
			"Please inform the user that this tool is currently disabled and can be re-enabled in Settings > Connectors.",
		toolName,
	)
}

// WrapToolsWithPermission wraps a list of tools with permission gating.
// Tools that are not in the identifierMap will pass through unwrapped.
func WrapToolsWithPermission(tools []tool.BaseTool, identifierMap map[string]string, config *PermissionConfig) []tool.BaseTool {
	if config == nil {
		return tools
	}

	wrapped := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			wrapped[i] = t
			continue
		}

		identifier, ok := identifierMap[info.Name]
		if !ok {
			identifier = info.Name
		}

		wrapped[i] = NewPermissionGate(t, identifier, config)
	}

	return wrapped
}

// SimplePermissionChecker creates a PermissionChecker from a static map.
// This is useful for testing or when permissions are configured at startup.
func SimplePermissionChecker(perms map[string]ConnectorToolPermission) PermissionChecker {
	var mu sync.RWMutex
	return func(_ context.Context, identifier, _ string) (ConnectorToolPermission, error) {
		mu.RLock()
		defer mu.RUnlock()
		if perm, ok := perms[identifier]; ok {
			return perm, nil
		}
		return "", nil
	}
}
