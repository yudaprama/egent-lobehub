package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

var _ testsuite.TestWorkflowEnvironment // reference the import even if not used

// newTestWorkflowEnv returns a fresh Temporal test environment with the
// workflow + activities pre-registered. The environment runs the
// workflow synchronously in memory — no Temporal server required.
func newTestWorkflowEnv(t *testing.T, store TaskStore, exec AgentExecutor) (*testsuite.TestWorkflowEnvironment, *WorkflowOptions) {
	t.Helper()
	opts := WorkflowOptions{TaskQueue: "test"}
	opts.WithDefaults()
	suite := new(testsuite.WorkflowTestSuite)
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(TaskRunWorkflow, workflow.RegisterOptions{Name: WorkflowTaskRun})
	acts := &Activities{Store: store, Executor: exec, Options: opts}
	acts.Register(env)
	return env, &opts
}

// TestTaskRunWorkflow_HappyPath covers the full happy path: a backlog
// task with an assignee, no review, no cascade. The workflow should
// end with the task in TaskStatusPaused (no auto-review → awaiting
// human).
func TestTaskRunWorkflow_HappyPath(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	task.Status = TaskStatusBacklog
	store.AddTask(task)

	exec := NewMockExecutor(&AgentRunResult{
		OperationID:      "op_1",
		TopicID:          "topic_1",
		ModelUsed:        "gpt-5",
		AssistantContent: "all done",
	})

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{TaskID: "agt_1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	var result RunTaskResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if result.TopicID != "topic_1" {
		t.Errorf("expected TopicID=topic_1, got %q", result.TopicID)
	}
	if result.ModelUsed != "gpt-5" {
		t.Errorf("expected ModelUsed=gpt-5, got %q", result.ModelUsed)
	}
	if result.AssistantContent != "all done" {
		t.Errorf("expected AssistantContent=all done, got %q", result.AssistantContent)
	}

	var status TaskStatus
	resp, err := env.QueryWorkflow("status")
	if err != nil {
		t.Fatalf("query status: %v", err)
	}
	if err := resp.Get(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status != TaskStatusPaused {
		t.Errorf("expected status=paused, got %q", status)
	}

	// Verify the executor was called.
	if len(exec.RunCalls) != 1 {
		t.Errorf("expected 1 executor call, got %d", len(exec.RunCalls))
	}
}

// TestTaskRunWorkflow_TaskNotFound covers the not-found path: a
// non-existent task id should produce a non-retryable application error.
func TestTaskRunWorkflow_TaskNotFound(t *testing.T) {
	store := NewInMemoryStore()
	exec := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{TaskID: "missing"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected error for not-found task")
	}
	if !strings.Contains(err.Error(), "Task not found") {
		t.Errorf("expected error to mention 'Task not found', got %v", err)
	}
	// Verify the executor was NOT called.
	if len(exec.RunCalls) != 0 {
		t.Errorf("expected 0 executor calls, got %d", len(exec.RunCalls))
	}
}

// TestTaskRunWorkflow_AlreadyRunning covers the conflict path: a
// running topic exists. Should produce a non-retryable conflict error.
func TestTaskRunWorkflow_AlreadyRunning(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	task.Status = TaskStatusBacklog
	store.AddTask(task)
	// Add a running topic to trigger the conflict.
	_ = store.AddTaskTopic(context.Background(), "agt_1", "topic_running", "op_running", 1)

	exec := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{TaskID: "agt_1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "already has a running topic") {
		t.Errorf("expected error to mention 'already has a running topic', got %v", err)
	}
}

// TestTaskRunWorkflow_ContinuePath covers the continue-from-topic path:
// the workflow should not transition the task to running again (it's
// already running) and should link the new run to the existing topic.
func TestTaskRunWorkflow_ContinuePath(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	task.Status = TaskStatusRunning // already running
	store.AddTask(task)

	exec := NewMockExecutor(&AgentRunResult{
		OperationID:      "op_2",
		TopicID:          "topic_existing",
		ModelUsed:        "gpt-5",
		AssistantContent: "continued",
	})

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{
		TaskID:         "agt_1",
		ContinueTopicID: "topic_existing",
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
}

// TestTaskRunWorkflow_AgentExecutionFailure covers the saga rollback
// path: the executor fails, the workflow should compensate and
// transition the task to paused.
func TestTaskRunWorkflow_AgentExecutionFailure(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	task.Status = TaskStatusBacklog
	store.AddTask(task)

	exec := NewMockExecutor(nil)
	exec.Err = errors.New("LLM rate limit")

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{TaskID: "agt_1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow error from executor failure")
	}

	// After compensation, the task should be in paused state.
	got, _ := store.ResolveTask(context.Background(), "agt_1")
	if got.Status != TaskStatusPaused {
		t.Errorf("expected Status=paused after saga rollback, got %q", got.Status)
	}
	if got.Error == "" {
		t.Error("expected Error to be set after saga rollback")
	}
}

// TestTaskRunWorkflow_InboxFallback covers the case where the task has
// no assignee: the workflow should resolve the inbox agent and use it.
func TestTaskRunWorkflow_InboxFallback(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "" // no assignee
	store.AddTask(task)

	exec := NewMockExecutor(&AgentRunResult{
		OperationID:      "op_1",
		TopicID:          "topic_1",
		ModelUsed:        "gpt-5",
		AssistantContent: "ok",
	})

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{TaskID: "agt_1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(exec.RunCalls) != 1 {
		t.Errorf("expected 1 executor call after fallback, got %d", len(exec.RunCalls))
	}
	if exec.RunCalls[0].AgentID == "" {
		t.Error("expected fallback agent id to be set")
	}
}

// TestTaskRunWorkflow_RequiredTaskID covers the parameter validation:
// an empty TaskID should fail synchronously.
func TestTaskRunWorkflow_RequiredTaskID(t *testing.T) {
	store := NewInMemoryStore()
	exec := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})

	env, _ := newTestWorkflowEnv(t, store, exec)
	env.ExecuteWorkflow(WorkflowTaskRun, RunTaskParams{TaskID: ""})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected error for empty task id")
	}
}

// TestExtraPromptTitle covers the title helper.
func TestExtraPromptTitle(t *testing.T) {
	task := &TaskItem{Identifier: "task-id", Name: "Task Name"}

	t.Run("with_extra_prompt", func(t *testing.T) {
		got := extraPromptTitle("do something interesting", task)
		if got != "do something interesting" {
			t.Errorf("expected extra prompt, got %q", got)
		}
	})

	t.Run("extra_prompt_too_long", func(t *testing.T) {
		long := strings.Repeat("x", 200)
		got := extraPromptTitle(long, task)
		if len(got) != 100 {
			t.Errorf("expected length 100, got %d", len(got))
		}
	})

	t.Run("no_extra_uses_name", func(t *testing.T) {
		got := extraPromptTitle("", task)
		if got != "Task Name" {
			t.Errorf("expected name, got %q", got)
		}
	})

	t.Run("no_extra_no_name_uses_identifier", func(t *testing.T) {
		task.Name = ""
		got := extraPromptTitle("", task)
		if got != "task-id" {
			t.Errorf("expected identifier, got %q", got)
		}
	})
}

// TestRunCompensations covers the saga compensation execution order.
// Compensations are executed in reverse order of registration.
func TestRunCompensations(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	store.AddTask(task)
	exec := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})
	env, _ := newTestWorkflowEnv(t, store, exec)

	// Track execution order via a slice in the closure.
	var order []string
	comps := []Compensation{
		{Name: "first", Fn: func(_ workflow.Context, _ *TaskItem) error {
			order = append(order, "first")
			return nil
		}},
		{Name: "second", Fn: func(_ workflow.Context, _ *TaskItem) error {
			order = append(order, "second")
			return nil
		}},
		{Name: "third", Fn: func(_ workflow.Context, _ *TaskItem) error {
			order = append(order, "third")
			return nil
		}},
	}

	// Run inside a workflow context so workflow.Context is satisfied.
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		runCompensations(ctx, comps, task)
		return nil
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	// Expected order: third, second, first (reverse of registration).
	if len(order) != 3 {
		t.Fatalf("expected 3 compensations to run, got %d", len(order))
	}
	if order[0] != "third" || order[1] != "second" || order[2] != "first" {
		t.Errorf("expected reverse order [third, second, first], got %v", order)
	}
}

// TestRunCompensations_BestEffort verifies compensations continue to
// run even if one fails. This is critical for saga semantics — a failed
// compensation must not prevent subsequent ones from cleaning up.
func TestRunCompensations_BestEffort(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	store.AddTask(task)
	exec := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})
	env, _ := newTestWorkflowEnv(t, store, exec)

	var order []string
	comps := []Compensation{
		{Name: "first", Fn: func(_ workflow.Context, _ *TaskItem) error {
			order = append(order, "first")
			return nil
		}},
		{Name: "failing", Fn: func(_ workflow.Context, _ *TaskItem) error {
			order = append(order, "failing")
			return errors.New("compensation failed")
		}},
		{Name: "third", Fn: func(_ workflow.Context, _ *TaskItem) error {
			order = append(order, "third")
			return nil
		}},
	}

	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		runCompensations(ctx, comps, task)
		return nil
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}

	// All three should have run, even though the middle one failed.
	if len(order) != 3 {
		t.Errorf("expected 3 compensations to run, got %d (%v)", len(order), order)
	}
}

// TestWorkflowID covers the workflow id generation helper. We use a
// stable, predictable id so duplicate StartTaskWorkflow calls for the
// same task are deduplicated by Temporal.
func TestWorkflowID(t *testing.T) {
	if got := workflowID("agt_1"); got != "task-run/agt_1" {
		t.Errorf("expected task-run/agt_1, got %q", got)
	}
	if got := workflowID("do-laundry"); got != "task-run/do-laundry" {
		t.Errorf("expected task-run/do-laundry, got %q", got)
	}
}

// TestTemporalRetryPolicy covers the retry policy construction from
// workflow options. The values are passed to client.StartWorkflowOptions
// and bound the total retry budget.
func TestTemporalRetryPolicy(t *testing.T) {
	opts := WorkflowOptions{
		InitialRetryInterval: 2 * time.Second,
		MaxRetryInterval:     30 * time.Second,
		MaxAgentExecutionAttempts: 5,
	}
	rp := temporalRetryPolicy(opts)
	if rp.InitialInterval != 2*time.Second {
		t.Errorf("InitialInterval: got %v, want 2s", rp.InitialInterval)
	}
	if rp.MaximumInterval != 30*time.Second {
		t.Errorf("MaximumInterval: got %v, want 30s", rp.MaximumInterval)
	}
	if rp.MaximumAttempts != 5 {
		t.Errorf("MaximumAttempts: got %d, want 5", rp.MaximumAttempts)
	}
	if rp.BackoffCoefficient != 2.0 {
		t.Errorf("BackoffCoefficient: got %v, want 2.0", rp.BackoffCoefficient)
	}
}
