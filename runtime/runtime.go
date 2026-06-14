package runtime

import (
	"context"
	"fmt"
	"log"
	"sync"

	"egent-lobehub/agent"

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

	ag, err := agent.NewAgent(ctx, &agent.AgentConfig{
		SystemPrompt: r.cfg.SystemPrompt,
		Tools:        r.tools,
	}, &agent.AgentOptions{
		Name:               r.cfg.AgentName,
		BaseURL:            r.cfg.BaseURL,
		ModelName:          r.cfg.ModelName,
		ToolResultMaxLength: r.cfg.ToolResultMaxLength,
	})
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}
	r.agent = ag
	r.runner = agent.NewRunner(ctx, ag)
	r.started = true
	log.Printf("runtime started: %d tools registered, max_len=%d", len(r.tools), r.cfg.ToolResultMaxLength)
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
