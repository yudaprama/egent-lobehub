package runtime

import (
	"context"
	"fmt"
	"log"
	"sync"

	"egent-lobehub/agent"
	"egent-lobehub/middleware"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

// Config holds the runtime configuration.
type Config struct {
	AgentName           string
	SystemPrompt        string
	ToolResultMaxLength int
	BaseURL             string
	ModelName           string

	// MergedConfig holds the layered agent config map (if available).
	// When set, it overrides SystemPrompt, ModelName, etc. from the merge.
	MergedConfig map[string]any

	// WorkspaceID scopes agent config (skip user layer when set).
	WorkspaceID string

	// PermissionConfig gates tools by user permission (optional).
	PermissionConfig *middleware.PermissionConfig
}

// Runtime coordinates the LobeHub Eino agent:
//   - Holds the agent + runner lifecycle
//   - Registers and wraps tools with LobeHub middleware
//   - Provides Query/QueryStream entrypoints (LobeHub AgentRuntimeService equivalent)
type Runtime struct {
	cfg     Config
	mu      sync.Mutex
	tools   []tool.BaseTool
	agent   adk.Agent
	runner  *adk.Runner
	started bool
}

// New creates a runtime with the given config. The agent is not built
// until RegisterTools + Start are called (LobeHub builds the agent
// after merging user/agent config and registering plugins).
func New(ctx context.Context, cfg *Config) (*Runtime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("runtime: config is required")
	}
	if cfg.AgentName == "" {
		cfg.AgentName = "LobeHubAgent"
	}
	if cfg.ToolResultMaxLength <= 0 {
		cfg.ToolResultMaxLength = 25000
	}
	return &Runtime{cfg: *cfg}, nil
}

// RegisterTools adds tools to the runtime. The agent is (re)built on Start.
func (r *Runtime) RegisterTools(tools []tool.BaseTool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = append(r.tools, tools...)
	return nil
}

// Tools returns the registered tools.
func (r *Runtime) Tools() []tool.BaseTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tools
}

// Start builds the Eino ChatModelAgent and Runner. Safe to call once.
func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}

	// Resolve merged config if available, falling back to explicit fields.
	systemPrompt := r.cfg.SystemPrompt
	modelName := r.cfg.ModelName
	if r.cfg.MergedConfig != nil {
		if sp, ok := r.cfg.MergedConfig["systemPrompt"].(string); ok && sp != "" {
			systemPrompt = sp
		}
		if m, ok := r.cfg.MergedConfig["model"].(string); ok && m != "" {
			modelName = m
		}
	}

	agentOpts := &agent.AgentOptions{
		Name:                r.cfg.AgentName,
		BaseURL:             r.cfg.BaseURL,
		ModelName:           modelName,
		ToolResultMaxLength: r.cfg.ToolResultMaxLength,
		PermissionConfig:    r.cfg.PermissionConfig,
	}

	ag, err := agent.NewAgent(ctx, &agent.AgentConfig{
		SystemPrompt: systemPrompt,
		Tools:        r.tools,
	}, agentOpts)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}
	r.agent = ag
	r.runner = agent.NewRunner(ctx, ag)
	r.started = true
	log.Printf("runtime started: %d tools, max_len=%d, model=%s", len(r.tools), r.cfg.ToolResultMaxLength, modelName)
	return nil
}

// Close releases runtime resources.
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = false
	return nil
}

// Query runs a single conversation turn and returns the event iterator.
// Mirrors LobeHub's AgentRuntimeService.executeOperation().
func (r *Runtime) Query(ctx context.Context, query string) (*adk.AsyncIterator[*adk.AgentEvent], error) {
	if !r.started {
		return nil, fmt.Errorf("runtime not started")
	}
	return r.runner.Query(ctx, query), nil
}
