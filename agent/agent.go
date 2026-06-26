package agent

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
)

// Identity headers carried from the egent's incoming request to the outbound
// Plano model call. The auth edge injects x-arch-actor-id on the way in; the
// egent forwards it so brightstaff can stamp billing.actor_id on the LLM span.
//
// Outbound target is Plano's loopback-only internal ingress (:12010), which
// trusts callers by network position plus a static x-arch-internal-key header
// (PLANO_INTERNAL_KEY) — no per-hop Oathkeeper/Talos round-trip. The client's
// Talos Authorization is intentionally NOT forwarded: :12010 does not validate it.
const (
	actorIDHeader     = "x-arch-actor-id"
	internalKeyHeader = "x-arch-internal-key"
)

type ctxActorIDKey struct{}

// ContextWithForwardedHeaders stashes the incoming actor id so the model
// client's transport can re-apply it on the outbound :12010 call. The second
// argument (the client's Authorization) is accepted for call-site compatibility
// but no longer forwarded — see the note above.
func ContextWithForwardedHeaders(ctx context.Context, actorID, _ string) context.Context {
	return context.WithValue(ctx, ctxActorIDKey{}, actorID)
}

// forwardingTransport stamps the internal static key and the propagated actor
// id onto the outbound Plano :12010 call.
type forwardingTransport struct {
	base        http.RoundTripper
	internalKey string
}

func (t *forwardingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.internalKey != "" {
		req.Header.Set(internalKeyHeader, t.internalKey)
	}
	if v, _ := req.Context().Value(ctxActorIDKey{}).(string); v != "" {
		req.Header.Set(actorIDHeader, v)
	}
	return t.base.RoundTrip(req)
}

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
	baseURL := "http://localhost:12010/v1"
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
		// Stamp the internal static key + forward x-arch-actor-id onto the Plano
		// :12010 (internal ingress) call.
		HTTPClient: &http.Client{Transport: &forwardingTransport{
			base:        http.DefaultTransport.(*http.Transport).Clone(),
			internalKey: os.Getenv("PLANO_INTERNAL_KEY"),
		}},
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
		Agent:           agent,
		EnableStreaming: true,
	})
}
