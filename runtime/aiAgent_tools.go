package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"egent-lobehub/middleware"

	"github.com/cloudwego/eino/components/tool"
)

// ToolSource identifies where a tool came from.
// Mirrors LobeHub's ToolSource enum: 'builtin' | 'mcp' | 'plugin' | 'market' | 'klavis'.
type ToolSource string

const (
	ToolSourceBuiltin ToolSource = "builtin"
	ToolSourceMCP     ToolSource = "mcp"
	ToolSourcePlugin  ToolSource = "plugin"
	ToolSourceMarket  ToolSource = "market"
	ToolSourceKlavis  ToolSource = "klavis"
	ToolSourceMemory  ToolSource = "memory"
	ToolSourceCustom  ToolSource = "custom"
)

// ResolvedTool is a tool with metadata about its source and identifier.
type ResolvedTool struct {
	Tool       tool.BaseTool
	Identifier string
	Source     ToolSource
	Disabled   bool
}

// ToolResolver collects tools from various sources and resolves them
// for an agent run. Mirrors LobeHub's Mecha/AgentToolsEngine.
type ToolResolver struct {
	mu       sync.Mutex
	registry map[string]*ResolvedTool
	permCfg  *middleware.PermissionConfig
}

// NewToolResolver creates a new resolver.
func NewToolResolver() *ToolResolver {
	return &ToolResolver{
		registry: make(map[string]*ResolvedTool),
	}
}

// WithPermissionConfig attaches a permission gate to all resolved tools.
func (r *ToolResolver) WithPermissionConfig(cfg *middleware.PermissionConfig) *ToolResolver {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.permCfg = cfg
	return r
}

// Register adds a tool with the given identifier and source.
func (r *ToolResolver) Register(t tool.BaseTool, identifier string, source ToolSource) error {
	info, err := t.Info(context.Background())
	if err != nil || info == nil {
		return fmt.Errorf("tool %s: invalid info", identifier)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry[info.Name] = &ResolvedTool{
		Tool:       t,
		Identifier: identifier,
		Source:     source,
	}
	return nil
}

// RegisterMany adds multiple tools in one call.
func (r *ToolResolver) RegisterMany(tools map[string]toolWithMeta) error {
	for name, t := range tools {
		if err := r.Register(t.Tool, t.Identifier, t.Source); err != nil {
			return err
		}
		_ = name
	}
	return nil
}

type toolWithMeta struct {
	Tool       tool.BaseTool
	Identifier string
	Source     ToolSource
}

// Disable marks a tool as disabled (via permission gate).
func (r *ToolResolver) Disable(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.registry[name]; ok {
		t.Disabled = true
	}
}

// Resolve returns the final tool list, applying permissions and middleware.
// Returns tools sorted by name for deterministic ordering.
func (r *ToolResolver) Resolve(_ context.Context) []tool.BaseTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for k := range r.registry {
		names = append(names, k)
	}
	sort.Strings(names)

	var tools []tool.BaseTool
	for _, name := range names {
		rt := r.registry[name]
		// Wrap with middleware
		wrapped := middleware.WrapWithMiddleware(
			rt.Tool,
			rt.Identifier,
			r.permCfg,
			middleware.DefaultToolResultMaxLength,
		)
		tools = append(tools, wrapped)
	}
	return tools
}

// ResolveBySource returns tools filtered by source, then wrapped.
func (r *ToolResolver) ResolveBySource(_ context.Context, source ToolSource) []tool.BaseTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var tools []tool.BaseTool
	for _, rt := range r.registry {
		if rt.Source != source {
			continue
		}
		wrapped := middleware.WrapWithMiddleware(
			rt.Tool,
			rt.Identifier,
			r.permCfg,
			middleware.DefaultToolResultMaxLength,
		)
		tools = append(tools, wrapped)
	}
	return tools
}

// ListIdentifiers returns the names of all registered tools.
func (r *ToolResolver) ListIdentifiers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for k := range r.registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Len returns the number of registered tools.
func (r *ToolResolver) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.registry)
}

// Log writes a debug summary of registered tools.
func (r *ToolResolver) Log() {
	slog.Debug("tool resolver",
		"count", r.Len(),
		"identifiers", r.ListIdentifiers(),
	)
}
