package agent

import (
	"context"
	"fmt"
	"log"
	"os"

	"egent-lobehub/middleware"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
)

// AgentConfig holds the agent configuration.
type AgentConfig struct {
	SystemPrompt string
	Tools        []tool.BaseTool
}

// AgentOptions configures the Eino agent with LobeHub middleware.
type AgentOptions struct {
	// PermissionConfig gates tools by user permission (optional).
	PermissionConfig *middleware.PermissionConfig
	// ToolResultMaxLength caps truncated tool output (0 = default 25k).
	ToolResultMaxLength int
	// Name overrides the default agent name.
	Name string
	// BaseURL overrides the LLM gateway URL.
	BaseURL string
	// ModelName overrides the model name.
	ModelName string
}

// NewAgent creates an Eino ChatModelAgent with LobeHub-style middleware.
// Each tool is wrapped with: permission gate → error classification + truncation.
func NewAgent(ctx context.Context, cfg *AgentConfig, opts *AgentOptions) (adk.Agent, error) {
	baseURL := "http://localhost:12000/v1"
	modelName := "custom/glm-5.1"
	agentName := "LobeHubAgent"
	maxLen := middleware.DefaultToolResultMaxLength
	var permCfg *middleware.PermissionConfig

	if opts != nil {
		if opts.BaseURL != "" {
			baseURL = opts.BaseURL
		}
		if opts.ModelName != "" {
			modelName = opts.ModelName
		}
		if opts.Name != "" {
			agentName = opts.Name
		}
		if opts.ToolResultMaxLength > 0 {
			maxLen = opts.ToolResultMaxLength
		}
		permCfg = opts.PermissionConfig
	}

	// Allow env var override
	if v := os.Getenv("PLANO_LLM_GATEWAY"); v != "" {
		baseURL = v
	}
	if v := os.Getenv("MODEL_NAME"); v != "" {
		modelName = v
	}

	chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  "EMPTY",
	})
	if err != nil {
		return nil, fmt.Errorf("create chat model: %w", err)
	}

	// Wrap each tool with LobeHub middleware layers
	wrappedTools := make([]tool.BaseTool, len(cfg.Tools))
	for i, t := range cfg.Tools {
		info, _ := t.Info(ctx)
		name := fmt.Sprintf("tool_%d", i)
		if info != nil {
			name = info.Name
		}
		wrappedTools[i] = middleware.WrapWithMiddleware(t, name, permCfg, maxLen)
		log.Printf("wrapped tool %s with middleware", name)
	}

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        agentName,
		Description: "LobeHub agent with tool execution middleware",
		Instruction: cfg.SystemPrompt,
		Model:       chatModel,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: wrappedTools,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}

	return agent, nil
}

// NewRunner creates an Eino Runner that wraps the agent for query execution.
func NewRunner(ctx context.Context, agent adk.Agent) *adk.Runner {
	return adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: agent,
	})
}
