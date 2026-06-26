package palace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"egent-lobehub/memory"
)

// SearchTool exposes [Store.Search] as an Eino agent tool. It performs
// semantic (or recency-fallback) recall over the user's structured
// "memory palace" — the five-layer user_memories_* store — and is the Go
// replacement for the LobeHub toolSearchMemory tRPC procedure.
//
// The user_id is read from context via memory.UserIDFromContext, the same
// key the agent runtime sets per request (see runtime/aiAgent.go). This
// matches the knowledge_search tool wiring exactly.
type SearchTool struct {
	store Store
}

// NewSearchTool wraps a palace [Store] as an Eino tool. A nil store
// yields a tool that reports "not configured" rather than panicking, so
// the agent can run without the memory palace.
func NewSearchTool(store Store) *SearchTool {
	return &SearchTool{store: store}
}

func (t *SearchTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "memory_palace_search",
		Desc: "Search the user's long-term memory palace (identity, activities, contexts, " +
			"experiences, preferences) by semantic similarity. Use this to recall who the user " +
			"is, what they've done, their ongoing situations, and their stated preferences. " +
			"Prefer this over generic memory lookups for rich personal context.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Desc:     "Natural-language description of what to recall (e.g. 'dietary preferences', 'current projects').",
				Type:     schema.String,
				Required: true,
			},
			"layers": {
				Desc:     "Optional comma-separated layer filter: identity, activity, context, experience, preference.",
				Type:     schema.String,
				Required: false,
			},
			"limit": {
				Desc:     "Max results to return. Defaults to 10, max 100.",
				Type:     schema.Integer,
				Required: false,
			},
		}),
	}, nil
}

func (t *SearchTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	if t == nil || t.store == nil {
		return "Memory palace is not configured.", nil
	}
	var args struct {
		Query  string `json:"query"`
		Layers string `json:"layers"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	userID := memory.UserIDFromContext(ctx)
	if userID == "" {
		return "", fmt.Errorf("no user_id in context")
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}

	in := SearchInput{Query: args.Query, Limit: args.Limit}
	if s := strings.TrimSpace(args.Layers); s != "" {
		for _, l := range strings.Split(s, ",") {
			if l = strings.TrimSpace(l); l != "" {
				in.Layers = append(in.Layers, l)
			}
		}
	}

	results, err := t.store.Search(ctx, userID, in)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("No memories found matching: %s", args.Query), nil
	}
	return formatSearchResults(results), nil
}

// formatSearchResults renders results as a compact, model-friendly list.
func formatSearchResults(results []SearchResult) string {
	var b strings.Builder
	for i, r := range results {
		text := r.Summary
		if text == "" {
			text = r.Title
		}
		if text == "" {
			text = r.Details
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, r.MemoryLayer, text)
		if r.Score > 0 {
			fmt.Fprintf(&b, " (relevance %.2f)", r.Score)
		}
		if len(r.Tags) > 0 {
			fmt.Fprintf(&b, " — tags: %s", strings.Join(r.Tags, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// compile-time check that SearchTool is an Eino invokable tool.
var _ tool.InvokableTool = (*SearchTool)(nil)
