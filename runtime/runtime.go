package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"egent-lobehub/agent"
	"egent-lobehub/keyvault"
	"egent-lobehub/lock"
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

	// Lock is the Redis-backed distributed edit lock (optional). When nil,
	// lock operations are no-ops (fail-open).
	Lock *lock.Mutex

	// KeyVault encrypts/decrypts secrets at rest (optional). When nil,
	// encryption/decryption are passthroughs (fail-open).
	KeyVault *keyvault.Encryptor
}

// Runtime coordinates the LobeHub Eino agent:
//   - Holds the agent + runner lifecycle
//   - Registers tools via ToolResolver (single resolution path)
//   - Provides Query entrypoint
//   - Exposes EditLock and KeyVault to HTTP handlers
type Runtime struct {
	cfg      Config
	mu       sync.Mutex
	resolver *ToolResolver
	agent    adk.Agent
	runner   *adk.Runner
	started  bool
	editLock *lock.Mutex
	keyVault *keyvault.Encryptor
}

// New creates a runtime with the given config. The agent is not built
// until RegisterTools + Start are called.
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
	r := &Runtime{
		cfg:      *cfg,
		editLock: cfg.Lock,
		keyVault: cfg.KeyVault,
	}
	r.resolver = NewToolResolver()
	if cfg.PermissionConfig != nil {
		r.resolver.WithPermissionConfig(cfg.PermissionConfig)
	}
	return r, nil
}

// EditLock returns the distributed edit lock (may be nil/disabled).
// Callers should treat a nil *lock.Mutex as fail-open.
func (r *Runtime) EditLock() *lock.Mutex {
	return r.editLock
}

// KeyVault returns the at-rest encryptor (may be nil/disabled).
// Callers should treat a nil *keyvault.Encryptor as fail-open
// passthrough.
func (r *Runtime) KeyVault() *keyvault.Encryptor {
	return r.keyVault
}

// Resolver returns the underlying ToolResolver for direct registration
// (e.g. Register(tool, identifier, source)).
func (r *Runtime) Resolver() *ToolResolver {
	return r.resolver
}

// RegisterTools adds tools to the runtime via the ToolResolver.
// Tools are tagged as ToolSourceBuiltin. The agent is (re)built on Start.
func (r *Runtime) RegisterTools(tools []tool.BaseTool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		if err := r.resolver.Register(t, info.Name, ToolSourceBuiltin); err != nil {
			return fmt.Errorf("register tool %s: %w", info.Name, err)
		}
	}
	return nil
}

// Tools returns the resolved (wrapped) tool list.
func (r *Runtime) Tools() []tool.BaseTool {
	return r.resolver.Resolve(context.Background())
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

	// Resolve tools through ToolResolver (wrapping happens here)
	tools := r.resolver.Resolve(ctx)

	agentOpts := &agent.AgentOptions{
		Name:      r.cfg.AgentName,
		BaseURL:   r.cfg.BaseURL,
		ModelName: modelName,
	}

	ag, err := agent.NewAgent(ctx, &agent.AgentConfig{
		SystemPrompt: systemPrompt,
		Tools:        tools,
	}, agentOpts)
	if err != nil {
		return fmt.Errorf("build agent: %w", err)
	}
	r.agent = ag
	r.runner = agent.NewRunner(ctx, ag)
	r.started = true
	slog.Info("runtime started",
		"tools", len(tools),
		"max_len", r.cfg.ToolResultMaxLength,
		"model", modelName,
	)
	return nil
}

// Close releases runtime resources.
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = false
	return nil
}

// Started reports whether the runtime has been started and is ready
// to accept queries.
func (r *Runtime) Started() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}
// Query runs a single conversation turn and returns the event iterator.
func (r *Runtime) Query(ctx context.Context, query string) (*adk.AsyncIterator[*adk.AgentEvent], error) {
	if !r.started {
		return nil, fmt.Errorf("runtime not started")
	}
	return r.runner.Query(ctx, query), nil
}
