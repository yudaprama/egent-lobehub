package agent

import (
	"context"
	"fmt"
	"os"

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

// AgentOptions configures the Eino agent.
type AgentOptions struct {
	// Name overrides the default agent name.
	Name string
	// BaseURL overrides the LLM gateway URL.
	BaseURL string
	// ModelName overrides the model name.
	ModelName string
}

// NewAgent creates an Eino ChatModelAgent. Tools should be pre-wrapped
// with middleware by the caller (Runtime uses ToolResolver for this).
func NewAgent(ctx context.Context, cfg *AgentConfig, opts *AgentOptions) (adk.Agent, error) {
	baseURL := "http://localhost:12000/v1"
	modelName := "custom/glm-5.1"
	agentName := "LobeHubAgent"

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

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        agentName,
		Description: "LobeHub agent with tool execution middleware",
		Instruction: cfg.SystemPrompt,
		Model:       chatModel,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: cfg.Tools,
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
