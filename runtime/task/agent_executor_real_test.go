package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"egent-lobehub/runtime"
)

// TestRuntimeExecutor_RealRunAgentExecution_NonEmptyContent proves the
// Step-1 task wiring (TASK_API_WIRING_PLAN.md items 1a–1c) end-to-end.
//
// Every other test in this package drives RunAgentExecution with a
// MockExecutor, so the suite proves the activity/workflow *plumbing* but
// NOT that the REAL RuntimeExecutor — the one wired in main.go — actually
// reaches AiAgentService.ExecAgent and returns content. This test closes
// that hole by wiring the real executor:
//
//	Gap A — Executor is the real RuntimeExecutor over a real AiAgentService,
//	        so RunAgentExecution actually calls ExecAgent (previously the
//	        noop returned empty output even on "completed" tasks).
//	Gap C — RuntimeExecutor.Run fills the layered config itself when no
//	        explicit AgentConfig is supplied, so config.MergeAgentConfig
//	        returns non-nil (no "agent config not found").
//
// The LLM is neutralised with no MuninnDB and no live provider: ExecAgent
// is nil-safe for memory (Recall is skipped when memoryMgr==nil) and never
// dereferences runtime/toolRegistrar on this path, so NewAiAgentService
// (nil, nil, nil) is sufficient. The Eino openai ChatModel reads its base
// URL from PLANO_LLM_GATEWAY, which we point at an httptest server that
// returns a canned assistant message. This is the no-infra equivalent of
// the plan's §6 "1d" live check (which needs Temporal + MuninnDB + a model
// key). The Temporal workflow orchestration layer is already covered by the
// mock-based workflow tests; this targets the real-executor boundary.
func TestRuntimeExecutor_RealRunAgentExecution_NonEmptyContent(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []string // captured upstream requests, for failure diagnosis
	)

	// Stub OpenAI-compatible gateway. The Eino openai ChatModel POSTs a
	// chat-completion request to {PLANO_LLM_GATEWAY}/chat/completions; it
	// may stream or not, so we probe the body and answer both shapes.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, fmt.Sprintf("PATH=%s STREAM=%v BODY=%s",
			r.URL.Path, bodyHasStreamTrue(body), truncateReq(string(body))))
		mu.Unlock()

		const content = "hello from stub model"

		if bodyHasStreamTrue(body) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			sse := func(choices map[string]any) {
				b, _ := json.Marshal(map[string]any{
					"id":      "chatcmpl-stub",
					"object":  "chat.completion.chunk",
					"model":   "stub",
					"choices": []any{choices},
				})
				fmt.Fprintf(w, "data: %s\n\n", b)
				if fl != nil {
					fl.Flush()
				}
			}
			sse(map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil})
			sse(map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil})
			sse(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"})
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl-stub",
			"object": "chat.completion",
			"model":  "stub",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer stub.Close()

	// Route the Eino openai ChatModel at the stub gateway.
	t.Setenv("PLANO_LLM_GATEWAY", stub.URL)

	// Real service + real executor, gap-C minimal path: no config layers are
	// supplied, so RuntimeExecutor.Run synthesises the agent layer itself.
	svc := runtime.NewAiAgentService(nil, nil, nil)
	exec := NewRuntimeExecutor(svc, nil)

	store := NewInMemoryStore()
	a := newTestActivities(store, exec)

	out, err := a.RunAgentExecution(context.Background(), RunAgentExecutionInput{
		Task: newTestTask("agt_real_1", "summarize-the-news"),
		Params: AgentRunParams{
			AgentID: "agt_real_1",
			UserID:  "user_real_1",
			Model:   "stub-model",
			Prompt:  "Say hello.",
		},
	})
	if err != nil {
		t.Fatalf("RunAgentExecution via real executor failed: %v\n%s", err, dumpReqs(&mu, &reqs))
	}
	if out == nil || out.Result == nil || strings.TrimSpace(out.Result.AssistantContent) == "" {
		t.Fatalf("expected non-empty assistantContent from real executor, got: %+v\n%s", out, dumpReqs(&mu, &reqs))
	}
	t.Logf("real executor assistantContent=%q modelUsed=%q",
		out.Result.AssistantContent, out.Result.ModelUsed)
}

// bodyHasStreamTrue reports whether the request body asks for streaming.
func bodyHasStreamTrue(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

func truncateReq(s string) string {
	const max = 600
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func dumpReqs(mu *sync.Mutex, reqs *[]string) string {
	mu.Lock()
	defer mu.Unlock()
	if len(*reqs) == 0 {
		return "(no upstream requests received — the model client never reached the stub)"
	}
	return "upstream requests seen:\n" + strings.Join(*reqs, "\n")
}
