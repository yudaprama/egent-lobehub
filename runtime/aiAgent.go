package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"egent-lobehub/agent"
	"egent-lobehub/config"
	"egent-lobehub/memory"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ExecAgentParams maps to LobeHub InternalExecAgentParams.
type ExecAgentParams struct {
	AgentID  string
	Slug     string
	Prompt   string
	UserID   string
	Model    string
	Provider string

	// Layered config sources (DEFAULT → server → user → agent).
	DefaultsConfig map[string]any
	ServerConfig   map[string]any
	UserConfig     map[string]any
	AgentConfig    map[string]any

	// Extra tools beyond those in the config.
	ExtraTools []tool.BaseTool

	// Memory
	MemoryMgr *memory.Manager

	// Workspace scoping (skips user layer in config merge).
	WorkspaceID string

	TimeNow func() time.Time
}

// ExecAgentResult maps to LobeHub ExecAgentResult.
// Events is the raw event iterator — callers consume it for streaming
// or use CollectResult() for buffered output.
type ExecAgentResult struct {
	Events    *adk.AsyncIterator[*adk.AgentEvent]
	AgentID   string
	MessageID string
	ModelUsed string
}

// CollectResult reads all events from an iterator and joins assistant text.
// Use this for non-streaming consumption of ExecAgentResult.Events.
func CollectResult(iter *adk.AsyncIterator[*adk.AgentEvent]) string {
	var parts []string
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			slog.Warn("agent event error", "error", event.Err)
			continue
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				continue
			}
			if msg.Role == schema.Assistant && msg.Content != "" {
				parts = append(parts, msg.Content)
			}
		}
	}
	return strings.Join(parts, "")
}

// AiAgentService is the Go equivalent of LobeHub's AiAgentService.
// Orchestrates: config merge → context building → agent creation → execution.
type AiAgentService struct {
	runtime       *Runtime
	toolRegistrar ToolRegistrar
	memoryMgr     *memory.Manager
}

// ToolRegistrar is the minimal interface the service needs to register tools.
type ToolRegistrar interface {
	RegisterTools(tools []tool.BaseTool) error
}

// NewAiAgentService creates a new agent execution service.
func NewAiAgentService(rt *Runtime, tr ToolRegistrar, mm *memory.Manager) *AiAgentService {
	return &AiAgentService{
		runtime:       rt,
		toolRegistrar: tr,
		memoryMgr:     mm,
	}
}

// ExecAgent runs the full agent pipeline and returns the event iterator:
//   1. Merge layered agent config
//   2. Build context with memory injection
//   3. Resolve tools
//   4. Create agent and run query
//   5. Return iterator — caller decides streaming vs buffered consumption
func (s *AiAgentService) ExecAgent(ctx context.Context, params ExecAgentParams) (*ExecAgentResult, error) {
	// 1. Merge layered agent config
	merged := config.MergeAgentConfig(
		params.DefaultsConfig,
		params.ServerConfig,
		params.UserConfig,
		params.AgentConfig,
		params.WorkspaceID,
	)
	if merged == nil {
		return nil, fmt.Errorf("agent config not found")
	}
	agentCfg, err := config.FromMap(merged)
	if err != nil {
		return nil, fmt.Errorf("parse merged config: %w", err)
	}

	resolvedID := agentCfg.ID
	if resolvedID == "" {
		resolvedID = params.AgentID
	}
	slog.Info("execAgent: starting",
		"id", resolvedID,
		"model", agentCfg.Model,
		"prompt", params.Prompt[:min(50, len(params.Prompt))],
	)

	// 2. Build system prompt with context injection
	systemPrompt := agentCfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = "You are a helpful AI assistant."
	}

	// Inject user memory into system prompt
	var memoryBlock string
	if s.memoryMgr != nil {
		memoryBlock = s.memoryMgr.Recall(ctx, params.UserID, params.Prompt)
	}
	if memoryBlock != "" {
		systemPrompt = fmt.Sprintf("%s\n\n%s", systemPrompt, memoryBlock)
		slog.Debug("injected memory context", "bytes", len(memoryBlock))
	}

	// 3. Resolve context from prompt attachments
	promptContext := buildPromptContext(params.Prompt)

	// 4. Build tool list: config tools + extra tools (memory tools, etc.)
	tools, err := s.resolveTools(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("resolve tools: %w", err)
	}

	// 5. Build the agent config
	ac := &agent.AgentConfig{
		SystemPrompt: systemPrompt,
		Tools:        tools,
	}

	// Resolve model name with override support
	modelName := params.Model
	if modelName == "" {
		modelName = agentCfg.Model
	}

	opts := &agent.AgentOptions{
		Name:      fmt.Sprintf("agent-%s", resolvedID),
		ModelName: modelName,
		BaseURL:   os.Getenv("PLANO_LLM_GATEWAY"),
	}

	// 6. Create the agent and runner
	ag, err := agent.NewAgent(ctx, ac, opts)
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}
	r := agent.NewRunner(ctx, ag)

	// 7. Build the query
	query := buildQuery(systemPrompt, promptContext, params.Prompt)

	// 8. Execute — return the iterator for caller-driven consumption
	iter := r.Query(ctx, query)
	return &ExecAgentResult{
		Events:    iter,
		AgentID:   resolvedID,
		ModelUsed: modelName,
	}, nil
}

// resolveTools builds the full tool list for execution.
func (s *AiAgentService) resolveTools(ctx context.Context, params ExecAgentParams) ([]tool.BaseTool, error) {
	// For now, return extra tools + memory tools.
	// TODO: add builtin tool resolution, MCP tools, plugin tools, skills.
	tools := make([]tool.BaseTool, 0, len(params.ExtraTools)+4)
	tools = append(tools, params.ExtraTools...)

	// Add memory tools if manager is available
	if s.memoryMgr != nil {
		memTools := s.memoryMgr.AllTools()
		tools = append(tools, memTools...)
	}

	return tools, nil
}

// buildPromptContext extracts context from the user's prompt (file IDs, URLs, etc.).
func buildPromptContext(prompt string) string {
	// Simple extractor: look for any URLs in the prompt.
	lines := strings.Split(prompt, "\n")
	var ctxLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
			ctxLines = append(ctxLines, trimmed)
		}
	}
	return strings.Join(ctxLines, "\n")
}

// buildQuery constructs the agent input from system prompt + context + user message.
func buildQuery(systemPrompt, context, userPrompt string) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	b.WriteString("\n\n")
	if context != "" {
		b.WriteString("User provided context:\n")
		b.WriteString(context)
		b.WriteString("\n\n")
	}
	b.WriteString(userPrompt)
	return b.String()
}
