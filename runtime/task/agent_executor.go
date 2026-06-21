package task

import (
	"context"
	"errors"
	"sync"
	"time"

	"egent-lobehub/runtime"
)

// AgentExecutor is the boundary between the Temporal workflow layer and the
// underlying agent runtime. The workflow never calls the Eino runtime
// directly — it always goes through this interface so that:
//
//  1. Activities (which are run by Temporal's worker) can call back into
//     the Eino runtime without coupling the workflow code to runtime types.
//  2. Tests can substitute a deterministic fake executor.
//
// In production, the executor is backed by runtime.AiAgentService.ExecAgent
// + runtime.CollectResult. The activity wraps each call with
// activity.RecordHeartbeat to keep Temporal informed of progress.
type AgentExecutor interface {
	// Run executes one agent turn against the resolved agent + prompt.
	// The implementation must call the progress callback at least once
	// per HeartbeatInterval (typically every message event) to keep the
	// Temporal activity alive.
	//
	// Returns:
	//   - run: the ExecAgentResult (with the iterator consumed)
	//   - content: the assistant's final message text
	//   - err: non-nil when the agent invocation fails
	Run(ctx context.Context, params AgentRunParams, progress ProgressCallback) (*AgentRunResult, error)

	// Interrupt cancels a running operation by its operation id. Best-effort;
	// if the operation has already completed, this is a no-op.
	Interrupt(ctx context.Context, operationID string) error
}

// AgentRunParams is the request to AgentExecutor.Run. It mirrors the
// fields the workflow computes from the task, store, and merged config.
type AgentRunParams struct {
	// AgentID is the resolved assignee. May be a slug (string identifier)
	// or a database id.
	AgentID string
	// UserID is the user that owns the task.
	UserID string
	// WorkspaceID scopes the agent config (skips the user layer on
	// merge). Empty for personal scope.
	WorkspaceID string
	// Model is the model snapshot to pin this run to. Backfilled from
	// the agent's current default if empty.
	Model string
	// Provider is the provider snapshot (e.g. "openai").
	Provider string
	// Prompt is the assembled user-message text. Built by
	// BuildTaskPromptActivity.
	Prompt string
	// Title is the topic title for this turn.
	Title string
	// FileIDs are attachments extracted from comments / dependencies.
	FileIDs []string
	// ContinueTopicID, when set, resumes a previous topic instead of
	// creating a new one.
	ContinueTopicID string
	// ApprovalMode controls the user-intervention gate. Defaults to
	// "headless" (auto-approve). The LobeHub task runner always uses
	// headless mode because approval flows are surfaced at the topic
	// level, not the task level.
	ApprovalMode runtime.ApprovalMode
}

// AgentRunResult is the successful outcome of AgentExecutor.Run.
type AgentRunResult struct {
	// OperationID is the Eino agent operation id. Used by Interrupt and
	// stored on task_topics.operationId.
	OperationID string
	// TopicID is the new (or continued) LobeHub topic id.
	TopicID string
	// ModelUsed is the model the agent actually ran on (after fallback
	// and snapshot backfill).
	ModelUsed string
	// AssistantContent is the final assistant message text. Passed to
	// the OnTopicComplete activity so it can feed the handoff / brief
	// synthesis LLM calls.
	AssistantContent string
}

// ProgressCallback is called by AgentExecutor.Run periodically to keep
// the Temporal activity alive (Tempo cancels activities that don't
// heartbeat within the configured timeout).
//
// The callback is wired to `activity.RecordHeartbeat(ctx, payload)` at
// runtime. Tests pass a no-op or a synchronised counter.
type ProgressCallback func(payload any)

// NoopProgress is a progress callback that does nothing. Used by the
// in-memory executor and tests.
func NoopProgress(_ any) {}

// --- Production executor --------------------------------------------------

// RuntimeExecutor is an AgentExecutor backed by the Eino runtime. It
// holds a reference to the runtime.AiAgentService (which itself holds the
// Eino runner, tool resolver, and memory manager).
type RuntimeExecutor struct {
	mu       sync.Mutex
	svc      *runtime.AiAgentService
	interrupt map[string]context.CancelFunc // operationID → cancel
}

// NewRuntimeExecutor wraps a runtime.AiAgentService as an AgentExecutor.
func NewRuntimeExecutor(svc *runtime.AiAgentService) *RuntimeExecutor {
	return &RuntimeExecutor{
		svc:       svc,
		interrupt: make(map[string]context.CancelFunc),
	}
}

// Run implements AgentExecutor. It calls ExecAgent and consumes the
// returned iterator synchronously (the workflow layer handles streaming
// via a different mechanism — a long-lived query/signal subscription).
func (e *RuntimeExecutor) Run(ctx context.Context, params AgentRunParams, progress ProgressCallback) (*AgentRunResult, error) {
	if progress == nil {
		progress = NoopProgress
	}
	if e.svc == nil {
		return nil, errors.New("RuntimeExecutor: AiAgentService is nil")
	}

	// The runtime.AiAgentService.ExecAgent signature takes ExecAgentParams
	// and returns an iterator. We consume it here so the activity sees a
	// simple (content, err) result.
	execParams := runtime.ExecAgentParams{
		AgentID:    params.AgentID,
		UserID:     params.UserID,
		Model:      params.Model,
		Provider:   params.Provider,
		Prompt:     params.Prompt,
		WorkspaceID: params.WorkspaceID,
	}

	result, err := e.svc.ExecAgent(ctx, execParams)
	if err != nil {
		return nil, err
	}

	// Stream events, calling progress on each.
	content := runtime.CollectResult(result.Events)
	progress(map[string]any{"phase": "completed", "bytes": len(content)})

	return &AgentRunResult{
		OperationID:      "", // runtime currently doesn't expose operation id
		TopicID:          "", // topic id is created by the TS BFF today
		ModelUsed:        result.ModelUsed,
		AssistantContent: content,
	}, nil
}

// Interrupt implements AgentExecutor. Cancels the agent context associated
// with the operation id, if any.
func (e *RuntimeExecutor) Interrupt(_ context.Context, operationID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel, ok := e.interrupt[operationID]; ok {
		cancel()
		delete(e.interrupt, operationID)
	}
	return nil
}

// --- Mock executor (for tests) --------------------------------------------

// MockExecutor is a deterministic AgentExecutor used in tests. Each Run
// call returns the configured Result + Err; calls are recorded for
// assertions.
type MockExecutor struct {
	mu          sync.Mutex
	RunCalls    []AgentRunParams
	Interrupts  []string
	Result      *AgentRunResult
	Err         error
	Delay       time.Duration // optional, to simulate long-running work
}

// NewMockExecutor returns a MockExecutor that returns the given result.
func NewMockExecutor(result *AgentRunResult) *MockExecutor {
	return &MockExecutor{Result: result}
}

// Run implements AgentExecutor.
func (m *MockExecutor) Run(_ context.Context, params AgentRunParams, progress ProgressCallback) (*AgentRunResult, error) {
	m.mu.Lock()
	m.RunCalls = append(m.RunCalls, params)
	m.mu.Unlock()

	if m.Delay > 0 {
		// Simulate periodic heartbeats during the delay.
		ticker := time.NewTicker(m.Delay / 4)
		defer ticker.Stop()
		deadline := time.Now().Add(m.Delay)
		for {
			<-ticker.C
			if progress != nil {
				progress("tick")
			}
			if time.Now().After(deadline) {
				break
			}
		}
	} else if progress != nil {
		progress("done")
	}

	if m.Err != nil {
		return nil, m.Err
	}
	return m.Result, nil
}

// Interrupt implements AgentExecutor.
func (m *MockExecutor) Interrupt(_ context.Context, operationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Interrupts = append(m.Interrupts, operationID)
	return nil
}
