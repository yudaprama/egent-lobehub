package task

import (
	"context"
	"testing"
	"time"
)

// newTestActivities returns an Activities struct wired to the given
// store and executor. Used as the test fixture for every activity.
func newTestActivities(store TaskStore, exec AgentExecutor) *Activities {
	return &Activities{
		Store:    store,
		Executor: exec,
		Options:  WorkflowOptions{}.WithDefaultsWorkflow(),
	}
}

// WithDefaultsWorkflow is a variant of WithDefaults that returns a
// value rather than mutating in place. Convenience for test fixtures.
func (o WorkflowOptions) WithDefaultsWorkflow() WorkflowOptions {
	o.WithDefaults()
	return o
}

// TestActivities_ResolveTask covers the happy + not-found paths.
func TestActivities_ResolveTask(t *testing.T) {
	store := NewInMemoryStore()
	store.AddTask(newTestTask("agt_1", "do-laundry"))
	a := newTestActivities(store, nil)

	t.Run("found", func(t *testing.T) {
		out, err := a.ResolveTask(context.Background(), ResolveTaskInput{TaskID: "agt_1"})
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}
		if !out.Found {
			t.Fatal("expected Found=true")
		}
		if out.Task.ID != "agt_1" {
			t.Errorf("expected ID=agt_1, got %q", out.Task.ID)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		out, err := a.ResolveTask(context.Background(), ResolveTaskInput{TaskID: "missing"})
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}
		if out.Found {
			t.Error("expected Found=false")
		}
		if out.Task != nil {
			t.Errorf("expected nil Task, got %+v", out.Task)
		}
	})
}

// TestActivities_BuildTaskPrompt covers the prompt assembly logic.
func TestActivities_BuildTaskPrompt(t *testing.T) {
	a := newTestActivities(NewInMemoryStore(), nil)

	out, err := a.BuildTaskPrompt(context.Background(), BuildTaskPromptInput{
		Task:        newTestTask("agt_1", "do-laundry"),
		ExtraPrompt: "be quick",
	})
	if err != nil {
		t.Fatalf("BuildTaskPrompt: %v", err)
	}
	if out.Prompt == "" {
		t.Error("expected non-empty Prompt")
	}
	// Both the extra prompt and the task instruction should be present.
	if !contains(out.Prompt, "be quick") {
		t.Error("expected Prompt to include extra prompt")
	}
	if !contains(out.Prompt, "do-laundry") {
		t.Error("expected Prompt to include task identifier")
	}
}

// TestActivities_RunAgentExecution verifies the activity wires the
// executor correctly and surfaces errors.
func TestActivities_RunAgentExecution(t *testing.T) {
	store := NewInMemoryStore()
	exec := NewMockExecutor(&AgentRunResult{
		OperationID:      "op_1",
		TopicID:          "topic_1",
		ModelUsed:        "gpt-5",
		AssistantContent: "hello world",
	})
	a := newTestActivities(store, exec)

	out, err := a.RunAgentExecution(context.Background(), RunAgentExecutionInput{
		Task: newTestTask("agt_1", "do-laundry"),
		Params: AgentRunParams{
			AgentID: "agt_1",
			Prompt:  "say hi",
			Model:   "gpt-5",
		},
	})
	if err != nil {
		t.Fatalf("RunAgentExecution: %v", err)
	}
	if out.Result.AssistantContent != "hello world" {
		t.Errorf("expected AssistantContent=hello world, got %q", out.Result.AssistantContent)
	}
	if len(exec.RunCalls) != 1 {
		t.Errorf("expected 1 executor call, got %d", len(exec.RunCalls))
	}
	if exec.RunCalls[0].Prompt != "say hi" {
		t.Errorf("expected Prompt=say hi, got %q", exec.RunCalls[0].Prompt)
	}
}

// TestActivities_RunAgentExecution_ErrorPath verifies the activity
// surfaces executor errors.
func TestActivities_RunAgentExecution_ErrorPath(t *testing.T) {
	exec := NewMockExecutor(nil)
	exec.Err = context.DeadlineExceeded
	a := newTestActivities(NewInMemoryStore(), exec)

	_, err := a.RunAgentExecution(context.Background(), RunAgentExecutionInput{
		Task:   newTestTask("agt_1", "do-laundry"),
		Params: AgentRunParams{AgentID: "agt_1"},
	})
	if err == nil {
		t.Fatal("expected error from executor")
	}
}

// TestActivities_OnTopicComplete_ReasonDone covers the happy path:
// topic done → task paused (no review) → cascade runs.
func TestActivities_OnTopicComplete_ReasonDone(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	store.AddTask(task)
	a := newTestActivities(store, nil)

	// Add a running topic so UpdateTaskTopicStatus has something to flip.
	_ = store.AddTaskTopic(context.Background(), "agt_1", "topic_1", "op_1", 1)

	out, err := a.OnTopicComplete(context.Background(), OnTopicCompleteInput{
		Task: task,
		Params: TopicCompleteParams{
			TaskID:               "agt_1",
			TaskIdentifier:       "do-laundry",
			TopicID:              "topic_1",
			OperationID:          "op_1",
			Reason:               ReasonDone,
			LastAssistantContent: "finished",
		},
	})
	if err != nil {
		t.Fatalf("OnTopicComplete: %v", err)
	}
	if out.Result.NewStatus != TaskStatusPaused {
		t.Errorf("expected NewStatus=paused, got %q", out.Result.NewStatus)
	}
	if !out.Result.Terminated && out.Result.NewStatus.IsTerminal() {
		t.Errorf("expected Terminated=true for terminal status %q", out.Result.NewStatus)
	}

	// Topic row should be marked completed.
	topics, _ := store.ListRunningTopics(context.Background(), "agt_1")
	if len(topics) != 0 {
		t.Errorf("expected 0 running topics after completion, got %d", len(topics))
	}
}

// TestActivities_OnTopicComplete_ReasonError covers the error path:
// error → task stays in current status (running) for the next tick,
// or paused if the fuse is blown.
func TestActivities_OnTopicComplete_ReasonError(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	task.Status = TaskStatusRunning
	task.AutomationMode = "heartbeat"
	store.AddTask(task)
	a := newTestActivities(store, nil)

	// First error: should NOT blow the fuse.
	out, err := a.OnTopicComplete(context.Background(), OnTopicCompleteInput{
		Task: task,
		Params: TopicCompleteParams{
			TaskID:         "agt_1",
			TaskIdentifier: "do-laundry",
			Reason:         ReasonError,
			ErrorMessage:   "transient",
		},
	})
	if err != nil {
		t.Fatalf("OnTopicComplete: %v", err)
	}
	// Task should still be in a runnable state.
	if out.Result.NewStatus != TaskStatusRunning {
		t.Errorf("expected NewStatus=running after 1 error, got %q", out.Result.NewStatus)
	}

	// Force the fuse: increment to HeartbeatFailureFuse-1, then
	// trigger one more error.
	task.ConsecutiveErrors = HeartbeatFailureFuse - 1
	out, _ = a.OnTopicComplete(context.Background(), OnTopicCompleteInput{
		Task: task,
		Params: TopicCompleteParams{
			TaskID:         "agt_1",
			TaskIdentifier: "do-laundry",
			Reason:         ReasonError,
			ErrorMessage:   "stuck",
		},
	})
	if !out.Result.Terminated {
		t.Errorf("expected Terminated=true after fuse blow, got %+v", out.Result)
	}
	if out.Result.NewStatus != TaskStatusPaused {
		t.Errorf("expected NewStatus=paused after fuse blow, got %q", out.Result.NewStatus)
	}
}

// TestActivities_OnTopicComplete_ReasonInterrupted covers the cancel path.
func TestActivities_OnTopicComplete_ReasonInterrupted(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.Status = TaskStatusRunning
	store.AddTask(task)
	a := newTestActivities(store, nil)

	out, err := a.OnTopicComplete(context.Background(), OnTopicCompleteInput{
		Task: task,
		Params: TopicCompleteParams{
			TaskID:   "agt_1",
			Reason:   ReasonInterrupted,
		},
	})
	if err != nil {
		t.Fatalf("OnTopicComplete: %v", err)
	}
	if out.Result.NewStatus != TaskStatusPaused {
		t.Errorf("expected NewStatus=paused, got %q", out.Result.NewStatus)
	}
	if !out.Result.Terminated {
		t.Error("expected Terminated=true for interruption")
	}
}

// TestActivities_OnTopicComplete_ScheduleCapReached covers the
// schedule-mode maxExecutions check.
func TestActivities_OnTopicComplete_ScheduleCapReached(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	task.AutomationMode = "schedule"
	task.TotalTopics = 5
	task.ScheduleStartedAt = timePtr(time.Now().Add(-1 * time.Hour))
	task.Config = &TaskConfig{Schedule: &ScheduleConfig{MaxExecutions: 5}}
	store.AddTask(task)
	a := newTestActivities(store, nil)

	out, err := a.OnTopicComplete(context.Background(), OnTopicCompleteInput{
		Task: task,
		Params: TopicCompleteParams{
			TaskID: "agt_1",
			Reason: ReasonDone,
		},
	})
	if err != nil {
		t.Fatalf("OnTopicComplete: %v", err)
	}
	if out.Result.NewStatus != TaskStatusCompleted {
		t.Errorf("expected NewStatus=completed after cap, got %q", out.Result.NewStatus)
	}
	if !out.Result.Terminated {
		t.Error("expected Terminated=true at cap")
	}
}

// TestActivities_CascadeOnCompletion_Empty covers the no-downstream
// path: returns an empty CascadeResult without error.
func TestActivities_CascadeOnCompletion_Empty(t *testing.T) {
	store := NewInMemoryStore()
	a := newTestActivities(store, nil)

	out, err := a.CascadeOnCompletion(context.Background(), CascadeOnCompletionInput{
		CompletedTaskID: "agt_1",
	})
	if err != nil {
		t.Fatalf("CascadeOnCompletion: %v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil result")
	}
	if len(out.Started) != 0 || len(out.Failed) != 0 || len(out.Paused) != 0 {
		t.Errorf("expected empty result, got %+v", out)
	}
}

// TestActivities_UpdateTaskStatus covers the pass-through to the store.
func TestActivities_UpdateTaskStatus(t *testing.T) {
	store := NewInMemoryStore()
	store.AddTask(newTestTask("agt_1", "do-laundry"))
	a := newTestActivities(store, nil)

	if err := a.UpdateTaskStatus(context.Background(), UpdateTaskStatusInput{
		TaskID: "agt_1",
		Status: TaskStatusRunning,
		Fields: []StatusField{WithStartedAt(time.Now())},
	}); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	got, _ := store.ResolveTask(context.Background(), "agt_1")
	if got.Status != TaskStatusRunning {
		t.Errorf("expected Status=running, got %q", got.Status)
	}
}

// TestActivities_UpdateHeartbeat covers the pass-through.
func TestActivities_UpdateHeartbeat(t *testing.T) {
	store := NewInMemoryStore()
	store.AddTask(newTestTask("agt_1", "do-laundry"))
	a := newTestActivities(store, nil)

	if err := a.UpdateHeartbeat(context.Background(), UpdateHeartbeatInput{TaskID: "agt_1"}); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	got, _ := store.ResolveTask(context.Background(), "agt_1")
	if got.LastHeartbeatAt == nil {
		t.Error("expected LastHeartbeatAt to be set")
	}
}

// TestActivities_BackfillModelConfig_NoBackfill covers the case where
// the task already has a model pinned.
func TestActivities_BackfillModelConfig_NoBackfill(t *testing.T) {
	store := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.Config = &TaskConfig{Model: "gpt-5", Provider: "openai"}
	store.AddTask(task)
	a := newTestActivities(store, nil)

	out, err := a.BackfillModelConfig(context.Background(), BackfillModelConfigInput{Task: task})
	if err != nil {
		t.Fatalf("BackfillModelConfig: %v", err)
	}
	if out.Updated {
		t.Error("expected Updated=false (task already pinned)")
	}
	if out.Model != "gpt-5" {
		t.Errorf("expected Model=gpt-5, got %q", out.Model)
	}
}

// TestActivities_BackfillModelConfig_Backfills covers the backfill
// path: the task has no model pinned, the agent has a default.
func TestActivities_BackfillModelConfig_Backfills(t *testing.T) {
	store := NewInMemoryStore()
	store.AddAgentConfig("agt_1", &ModelConfig{Model: "gpt-5", Provider: "openai"})
	task := newTestTask("agt_1", "do-laundry")
	task.AssigneeAgentID = "agt_1"
	store.AddTask(task)
	a := newTestActivities(store, nil)

	out, err := a.BackfillModelConfig(context.Background(), BackfillModelConfigInput{Task: task})
	if err != nil {
		t.Fatalf("BackfillModelConfig: %v", err)
	}
	if !out.Updated {
		t.Error("expected Updated=true")
	}
	if out.Model != "gpt-5" {
		t.Errorf("expected Model=gpt-5, got %q", out.Model)
	}
	got, _ := store.ResolveTask(context.Background(), "agt_1")
	if got.Config == nil || got.Config.Model != "gpt-5" {
		t.Errorf("expected task.config.model=gpt-5 after backfill, got %+v", got.Config)
	}
}

// TestActivities_EmitHandoff_EmptyContent covers the no-op path: when
// there's no assistant content, handoff is skipped.
func TestActivities_EmitHandoff_EmptyContent(t *testing.T) {
	a := newTestActivities(NewInMemoryStore(), nil)
	task := newTestTask("agt_1", "do-laundry")

	if err := a.EmitHandoff(context.Background(), EmitHandoffInput{
		Task:   task,
		Params: TopicCompleteParams{LastAssistantContent: ""},
	}); err != nil {
		t.Fatalf("EmitHandoff: %v", err)
	}
}

// TestActivities_EmitHandoff_WithContent covers the normal path: a real
// implementation would call the LLM and persist the result; the stub
// just logs and returns nil.
func TestActivities_EmitHandoff_WithContent(t *testing.T) {
	a := newTestActivities(NewInMemoryStore(), nil)
	task := newTestTask("agt_1", "do-laundry")

	if err := a.EmitHandoff(context.Background(), EmitHandoffInput{
		Task:   task,
		Params: TopicCompleteParams{LastAssistantContent: "some text"},
	}); err != nil {
		t.Fatalf("EmitHandoff: %v", err)
	}
}

// TestActivities_SynthesizeTopicBrief covers the brief synthesis path.
func TestActivities_SynthesizeTopicBrief(t *testing.T) {
	a := newTestActivities(NewInMemoryStore(), nil)
	task := newTestTask("agt_1", "do-laundry")

	if err := a.SynthesizeTopicBrief(context.Background(), SynthesizeTopicBriefInput{
		Task:   task,
		Params: TopicCompleteParams{},
	}); err != nil {
		t.Fatalf("SynthesizeTopicBrief: %v", err)
	}
}

// TestActivities_RunAutoReview_StubsReturnFalse covers the stub return:
// accepted=false. Production deployments replace this body with a real
// judge-model call.
func TestActivities_RunAutoReview_StubsReturnFalse(t *testing.T) {
	a := newTestActivities(NewInMemoryStore(), nil)
	task := newTestTask("agt_1", "do-laundry")

	accepted, err := a.RunAutoReview(context.Background(), RunAutoReviewInput{
		Task:   task,
		Params: TopicCompleteParams{LastAssistantContent: "ok"},
		Review: ReviewConfig{Enabled: true, JudgeModel: "gpt-4o"},
	})
	if err != nil {
		t.Fatalf("RunAutoReview: %v", err)
	}
	if accepted {
		t.Error("expected accepted=false from stub")
	}
}

// TestActivities_ResolveInboxAgent covers the inbox agent resolution.
func TestActivities_ResolveInboxAgent(t *testing.T) {
	store := NewInMemoryStore()
	a := newTestActivities(store, nil)

	id, err := a.ResolveInboxAgent(context.Background(), ResolveInboxAgentInput{})
	if err != nil {
		t.Fatalf("ResolveInboxAgent: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty agent id")
	}
}

// TestActivities_ListRunningTopics covers the topic listing path.
func TestActivities_ListRunningTopics(t *testing.T) {
	store := NewInMemoryStore()
	store.AddTask(newTestTask("agt_1", "do-laundry"))
	_ = store.AddTaskTopic(context.Background(), "agt_1", "topic_1", "op_1", 1)
	_ = store.AddTaskTopic(context.Background(), "agt_1", "topic_2", "op_2", 2)
	store.UpdateTaskTopicStatus(context.Background(), "agt_1", "topic_2", TopicStatusCompleted)
	a := newTestActivities(store, nil)

	topics, err := a.ListRunningTopics(context.Background(), ListRunningTopicsInput{TaskID: "agt_1"})
	if err != nil {
		t.Fatalf("ListRunningTopics: %v", err)
	}
	if len(topics) != 1 || topics[0].TopicID != "topic_1" {
		t.Errorf("expected 1 running topic=topic_1, got %+v", topics)
	}
}

// TestActivities_AddTaskTopic covers the topic link creation path.
func TestActivities_AddTaskTopic(t *testing.T) {
	store := NewInMemoryStore()
	store.AddTask(newTestTask("agt_1", "do-laundry"))
	a := newTestActivities(store, nil)

	if err := a.AddTaskTopic(context.Background(), AddTaskTopicInput{
		TaskID:      "agt_1",
		TopicID:     "topic_new",
		OperationID: "op_new",
		Seq:         1,
	}); err != nil {
		t.Fatalf("AddTaskTopic: %v", err)
	}
	topics, _ := store.ListRunningTopics(context.Background(), "agt_1")
	if len(topics) != 1 || topics[0].TopicID != "topic_new" {
		t.Errorf("expected 1 running topic=topic_new, got %+v", topics)
	}
}

// contains is a string contains helper to keep the tests dependency-free.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// timePtr returns a pointer to t. Convenience for test fixtures.
func timePtr(t time.Time) *time.Time { return &t }
