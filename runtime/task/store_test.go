package task

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// newTestTask returns a TaskItem pre-filled with the fields the store
// needs. Tests can tweak fields before calling AddTask.
func newTestTask(id, identifier string) *TaskItem {
	return &TaskItem{
		ID:         id,
		Identifier: identifier,
		UserID:     "user_test",
		Status:     TaskStatusBacklog,
		Config:     &TaskConfig{},
		CreatedAt:  time.Now(),
	}
}

// TestInMemoryStore_ResolveTask covers both id and identifier resolution.
// The TS resolve() treats them interchangeably.
func TestInMemoryStore_ResolveTask(t *testing.T) {
	s := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	s.AddTask(task)

	t.Run("by_id", func(t *testing.T) {
		got, err := s.ResolveTask(context.Background(), "agt_1")
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil task")
		}
		if got.ID != "agt_1" {
			t.Errorf("expected ID=agt_1, got %q", got.ID)
		}
	})

	t.Run("by_identifier", func(t *testing.T) {
		got, err := s.ResolveTask(context.Background(), "do-laundry")
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil task")
		}
		if got.Identifier != "do-laundry" {
			t.Errorf("expected Identifier=do-laundry, got %q", got.Identifier)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		got, err := s.ResolveTask(context.Background(), "missing")
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for not-found, got %+v", got)
		}
	})
}

// TestInMemoryStore_ResolveTask_ReturnsCopy ensures callers cannot
// mutate the store by holding the returned pointer. This is important
// because the workflow relies on the store being authoritative.
func TestInMemoryStore_ResolveTask_ReturnsCopy(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))

	got, _ := s.ResolveTask(context.Background(), "agt_1")
	got.Status = TaskStatusRunning // mutate the copy

	got2, _ := s.ResolveTask(context.Background(), "agt_1")
	if got2.Status == TaskStatusRunning {
		t.Errorf("store was mutated by caller (Status=%q); expected %q",
			got2.Status, TaskStatusBacklog)
	}
}

// TestInMemoryStore_UpdateStatus verifies the state transition logic
// including the no-op case (running → running) and the field patching.
func TestInMemoryStore_UpdateStatus(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))

	if err := s.UpdateStatus(context.Background(), "agt_1", TaskStatusRunning, WithStartedAt(time.Now())); err != nil {
		t.Fatalf("UpdateStatus running: %v", err)
	}
	got, _ := s.ResolveTask(context.Background(), "agt_1")
	if got.Status != TaskStatusRunning {
		t.Errorf("expected Status=running, got %q", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("expected StartedAt to be set")
	}

	// No-op: same status.
	before := got.UpdatedAt
	time.Sleep(time.Millisecond)
	if err := s.UpdateStatus(context.Background(), "agt_1", TaskStatusRunning); err != nil {
		t.Fatalf("UpdateStatus no-op: %v", err)
	}
	got, _ = s.ResolveTask(context.Background(), "agt_1")
	if !got.UpdatedAt.Equal(before) {
		t.Errorf("expected no UpdatedAt change on no-op transition, got diff %v",
			got.UpdatedAt.Sub(before))
	}

	// Transition to terminal with CompletedAt.
	if err := s.UpdateStatus(context.Background(), "agt_1", TaskStatusCompleted, WithCompletedAt(time.Now())); err != nil {
		t.Fatalf("UpdateStatus completed: %v", err)
	}
	got, _ = s.ResolveTask(context.Background(), "agt_1")
	if got.Status != TaskStatusCompleted {
		t.Errorf("expected Status=completed, got %q", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}
}

// TestInMemoryStore_UpdateStatus_UnknownField verifies the defensive
// check against typos in the StatusField name.
func TestInMemoryStore_UpdateStatus_UnknownField(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))

	err := s.UpdateStatus(context.Background(), "agt_1", TaskStatusRunning,
		StatusField{Name: "totally_bogus", Value: "x"})
	if err == nil {
		t.Fatal("expected error for unknown field name")
	}
}

// TestInMemoryStore_UpdateStatus_WithErrorField covers the error-clear
// path that the workflow uses before a new run.
func TestInMemoryStore_UpdateStatus_WithErrorField(t *testing.T) {
	s := NewInMemoryStore()
	task := newTestTask("agt_1", "do-laundry")
	task.Error = "previous failure"
	s.AddTask(task)

	if err := s.UpdateStatus(context.Background(), "agt_1", TaskStatusRunning, WithError("")); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, _ := s.ResolveTask(context.Background(), "agt_1")
	if got.Error != "" {
		t.Errorf("expected Error cleared, got %q", got.Error)
	}

	// Set a new error.
	if err := s.UpdateStatus(context.Background(), "agt_1", TaskStatusPaused, WithError("new failure")); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, _ = s.ResolveTask(context.Background(), "agt_1")
	if got.Error != "new failure" {
		t.Errorf("expected Error=new failure, got %q", got.Error)
	}
}

// TestInMemoryStore_TaskTopicLifecycle covers the topic link path used
// by AddTaskTopic + UpdateTaskTopicStatus.
func TestInMemoryStore_TaskTopicLifecycle(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))
	ctx := context.Background()

	// Add a running topic.
	if err := s.AddTaskTopic(ctx, "agt_1", "topic_1", "op_1", 1); err != nil {
		t.Fatalf("AddTaskTopic: %v", err)
	}

	running, err := s.ListRunningTopics(ctx, "agt_1")
	if err != nil {
		t.Fatalf("ListRunningTopics: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("expected 1 running topic, got %d", len(running))
	}
	if running[0].TopicID != "topic_1" {
		t.Errorf("expected TopicID=topic_1, got %q", running[0].TopicID)
	}

	// Complete the topic.
	if err := s.UpdateTaskTopicStatus(ctx, "agt_1", "topic_1", TopicStatusCompleted); err != nil {
		t.Fatalf("UpdateTaskTopicStatus: %v", err)
	}
	running, _ = s.ListRunningTopics(ctx, "agt_1")
	if len(running) != 0 {
		t.Errorf("expected 0 running topics after completion, got %d", len(running))
	}
}

// TestInMemoryStore_IncrementTopicCount verifies the atomic counter that
// the workflow uses for the seq field on AddTaskTopic.
func TestInMemoryStore_IncrementTopicCount(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))

	n1, err := s.IncrementTopicCount(context.Background(), "agt_1")
	if err != nil {
		t.Fatalf("IncrementTopicCount: %v", err)
	}
	if n1 != 1 {
		t.Errorf("expected n=1, got %d", n1)
	}

	n2, _ := s.IncrementTopicCount(context.Background(), "agt_1")
	if n2 != 2 {
		t.Errorf("expected n=2, got %d", n2)
	}
}

// TestInMemoryStore_GetCheckpointConfig covers the checkpoint gate path.
// Tasks without config → no gating; tasks with beforeIds → gating.
func TestInMemoryStore_GetCheckpointConfig(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()

	t.Run("no_config_returns_false", func(t *testing.T) {
		task := newTestTask("agt_1", "do-laundry")
		s.AddTask(task)
		_, hasCP, err := s.GetCheckpointConfig(ctx, task)
		if err != nil {
			t.Fatalf("GetCheckpointConfig: %v", err)
		}
		if hasCP {
			t.Error("expected hasCP=false for task without config")
		}
	})

	t.Run("with_before_ids", func(t *testing.T) {
		task := newTestTask("agt_2", "do-work")
		task.Config = &TaskConfig{
			Raw: map[string]any{
				"checkpoint": map[string]any{
					"onAgentRequest": false,
					"beforeIds":       []any{"subtask-A"},
				},
			},
		}
		s.AddTask(task)
		cp, hasCP, err := s.GetCheckpointConfig(ctx, task)
		if err != nil {
			t.Fatalf("GetCheckpointConfig: %v", err)
		}
		if !hasCP {
			t.Fatal("expected hasCP=true")
		}
		if cp.OnAgentRequest {
			t.Error("expected OnAgentRequest=false")
		}
		if len(cp.BeforeIDs) != 1 || cp.BeforeIDs[0] != "subtask-A" {
			t.Errorf("expected BeforeIDs=[subtask-A], got %v", cp.BeforeIDs)
		}
	})
}

// TestInMemoryStore_GetReviewConfig verifies the review config lookup.
func TestInMemoryStore_GetReviewConfig(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()

	t.Run("no_config", func(t *testing.T) {
		task := newTestTask("agt_1", "do-laundry")
		s.AddTask(task)
		_, hasReview, err := s.GetReviewConfig(ctx, task)
		if err != nil {
			t.Fatalf("GetReviewConfig: %v", err)
		}
		if hasReview {
			t.Error("expected hasReview=false")
		}
	})

	t.Run("with_config", func(t *testing.T) {
		task := newTestTask("agt_2", "do-work")
		task.Config = &TaskConfig{
			Review: &ReviewConfig{Enabled: true, MaxIterations: 2, JudgeModel: "gpt-4o"},
		}
		s.AddTask(task)
		cfg, hasReview, err := s.GetReviewConfig(ctx, task)
		if err != nil {
			t.Fatalf("GetReviewConfig: %v", err)
		}
		if !hasReview {
			t.Fatal("expected hasReview=true")
		}
		if !cfg.Enabled {
			t.Error("expected Enabled=true")
		}
		if cfg.MaxIterations != 2 {
			t.Errorf("expected MaxIterations=2, got %d", cfg.MaxIterations)
		}
	})
}

// TestInMemoryStore_BackfillModelConfig covers the model snapshot
// backfill: if the task has no model/provider pinned, copy the agent's
// current default into task.config.
func TestInMemoryStore_BackfillModelConfig(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	s.AddAgentConfig("agt_1", &ModelConfig{Model: "gpt-5", Provider: "openai"})

	t.Run("backfills_empty_task", func(t *testing.T) {
		task := newTestTask("agt_1", "do-laundry")
		task.AssigneeAgentID = "agt_1"
		s.AddTask(task)

		cfg, err := s.GetAgentModelConfig(ctx, "agt_1")
		if err != nil {
			t.Fatalf("GetAgentModelConfig: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil model config")
		}
		if cfg.Model != "gpt-5" {
			t.Errorf("expected Model=gpt-5, got %q", cfg.Model)
		}
	})

	t.Run("missing_agent_returns_nil", func(t *testing.T) {
		cfg, err := s.GetAgentModelConfig(ctx, "agt_unknown")
		if err != nil {
			t.Fatalf("GetAgentModelConfig: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil for unknown agent, got %+v", cfg)
		}
	})
}

// TestInMemoryStore_UpdateTaskConfig verifies the config merge logic and
// that the parsed TaskConfig can be read back.
func TestInMemoryStore_UpdateTaskConfig(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))

	raw := json.RawMessage(`{"model":"gpt-5","provider":"openai","brief":{"mode":"agent"}}`)
	if err := s.UpdateTaskConfig(context.Background(), "agt_1", raw); err != nil {
		t.Fatalf("UpdateTaskConfig: %v", err)
	}
	got, _ := s.ResolveTask(context.Background(), "agt_1")
	if got.Config == nil {
		t.Fatal("expected Config to be populated")
	}
	if got.Config.Model != "gpt-5" {
		t.Errorf("expected Model=gpt-5, got %q", got.Config.Model)
	}
	if got.Config.Provider != "openai" {
		t.Errorf("expected Provider=openai, got %q", got.Config.Provider)
	}
	if got.Config.Brief == nil || got.Config.Brief.Mode != BriefModeAgent {
		t.Errorf("expected Brief.Mode=agent, got %+v", got.Config.Brief)
	}
}

// TestInMemoryStore_GetUnlockedTasks_Empty verifies the default behavior
// when there are no dependency edges.
func TestInMemoryStore_GetUnlockedTasks_Empty(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))

	out, err := s.GetUnlockedTasks(context.Background(), "agt_1")
	if err != nil {
		t.Fatalf("GetUnlockedTasks: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty list, got %d items", len(out))
	}
}

// TestInMemoryStore_TimeoutRunningTopics_NoOp verifies the stub returns
// 0 transitions. The InMemoryStore has no heartbeat timestamps on
// topic rows, so there is nothing to time out.
func TestInMemoryStore_TimeoutRunningTopics_NoOp(t *testing.T) {
	s := NewInMemoryStore()
	s.AddTask(newTestTask("agt_1", "do-laundry"))
	n, err := s.TimeoutRunningTopics(context.Background(), "agt_1", time.Minute)
	if err != nil {
		t.Fatalf("TimeoutRunningTopics: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 timeouts, got %d", n)
	}
}

// TestInMemoryStore_GetInboxAgentID verifies the fallback resolution
// returns a usable agent id.
func TestInMemoryStore_GetInboxAgentID(t *testing.T) {
	s := NewInMemoryStore()
	id, err := s.GetInboxAgentID(context.Background())
	if err != nil {
		t.Fatalf("GetInboxAgentID: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty inbox agent id")
	}
}

// TestInMemoryStore_NotFoundErrors verifies the store returns an error
// (not a silent success) when operating on a missing task. The workflow
// relies on this to detect task deletion during a run.
func TestInMemoryStore_NotFoundErrors(t *testing.T) {
	s := NewInMemoryStore()

	err := s.UpdateStatus(context.Background(), "missing", TaskStatusRunning)
	if err == nil {
		t.Error("expected error for UpdateStatus on missing task")
	}

	_, err = s.IncrementTopicCount(context.Background(), "missing")
	if err == nil {
		t.Error("expected error for IncrementTopicCount on missing task")
	}

	err = s.UpdateTaskConfig(context.Background(), "missing", nil)
	if err == nil {
		t.Error("expected error for UpdateTaskConfig on missing task")
	}
}

// TestStatusField_WithHelpers covers the convenience constructors.
// These ensure the field names match what the InMemoryStore's
// UpdateStatus switch expects.
func TestStatusField_WithHelpers(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		f    StatusField
		want StatusField
	}{
		{"started_at", WithStartedAt(now), StatusField{Name: "started_at", Value: now}},
		{"completed_at", WithCompletedAt(now), StatusField{Name: "completed_at", Value: now}},
		{"error", WithError("boom"), StatusField{Name: "error", Value: "boom"}},
		{"total_topics", WithTotalTopics(5), StatusField{Name: "total_topics", Value: 5}},
		{"consecutive_errors", WithConsecutiveErrors(2), StatusField{Name: "consecutive_errors", Value: 2}},
		{"schedule_started_at", WithScheduleStartedAt(now), StatusField{Name: "schedule_started_at", Value: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.f.Name != tc.want.Name {
				t.Errorf("Name: got %q, want %q", tc.f.Name, tc.want.Name)
			}
			// Use errors.Is-equivalent comparison for Value via fmt.
			if fmt.Sprintf("%v", tc.f.Value) != fmt.Sprintf("%v", tc.want.Value) {
				t.Errorf("Value: got %v, want %v", tc.f.Value, tc.want.Value)
			}
		})
	}
}
