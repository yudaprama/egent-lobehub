package task

import (
	"testing"
	"time"
)

// TestTaskStatus_IsTerminal covers the terminal-state predicate. Matches
// the TERMINAL_STATUSES set in the TS source.
func TestTaskStatus_IsTerminal(t *testing.T) {
	terminal := []TaskStatus{TaskStatusCompleted, TaskStatusFailed, TaskStatusCanceled}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	nonTerminal := []TaskStatus{TaskStatusBacklog, TaskStatusPaused, TaskStatusScheduled, TaskStatusRunning}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

// TestTaskStatus_Valid covers the validation predicate. We use this to
// reject malformed status strings at the HTTP boundary.
func TestTaskStatus_Valid(t *testing.T) {
	valid := []TaskStatus{
		TaskStatusBacklog, TaskStatusPaused, TaskStatusScheduled,
		TaskStatusRunning, TaskStatusCompleted, TaskStatusFailed, TaskStatusCanceled,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if TaskStatus("bogus").Valid() {
		t.Error("bogus should not be valid")
	}
}

// TestWorkflowOptions_WithDefaults verifies the defaults are applied
// exactly once, even when fields are pre-set.
func TestWorkflowOptions_WithDefaults(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		var o WorkflowOptions
		o.WithDefaults()
		if o.TaskQueue != "lobehub-tasks" {
			t.Errorf("expected default TaskQueue=lobehub-tasks, got %q", o.TaskQueue)
		}
		if o.WorkflowExecutionTimeout != 24*time.Hour {
			t.Errorf("expected default WorkflowExecutionTimeout=24h, got %v", o.WorkflowExecutionTimeout)
		}
		if o.ActivityStartToCloseTimeout != 10*time.Minute {
			t.Errorf("expected default ActivityStartToCloseTimeout=10m, got %v", o.ActivityStartToCloseTimeout)
		}
		if o.AgentExecutionHeartbeatTimeout != 30*time.Second {
			t.Errorf("expected default AgentExecutionHeartbeatTimeout=30s, got %v", o.AgentExecutionHeartbeatTimeout)
		}
		if o.MaxAgentExecutionAttempts != 3 {
			t.Errorf("expected default MaxAgentExecutionAttempts=3, got %d", o.MaxAgentExecutionAttempts)
		}
		if o.InitialRetryInterval != 5*time.Second {
			t.Errorf("expected default InitialRetryInterval=5s, got %v", o.InitialRetryInterval)
		}
		if o.MaxRetryInterval != time.Minute {
			t.Errorf("expected default MaxRetryInterval=1m, got %v", o.MaxRetryInterval)
		}
	})

	t.Run("pre_set_values_preserved", func(t *testing.T) {
		o := WorkflowOptions{
			TaskQueue:                 "custom",
			MaxAgentExecutionAttempts: 7,
		}
		o.WithDefaults()
		if o.TaskQueue != "custom" {
			t.Errorf("expected TaskQueue=custom, got %q", o.TaskQueue)
		}
		if o.MaxAgentExecutionAttempts != 7 {
			t.Errorf("expected MaxAgentExecutionAttempts=7, got %d", o.MaxAgentExecutionAttempts)
		}
	})
}

// TestParseTaskConfig covers the JSON parsing tolerance. Unknown fields
// are stashed in Raw, not dropped.
func TestParseTaskConfig(t *testing.T) {
	t.Run("nil_input", func(t *testing.T) {
		cfg, err := ParseTaskConfig(nil)
		if err != nil {
			t.Fatalf("ParseTaskConfig(nil): %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		cfg, err := ParseTaskConfig([]byte("null"))
		if err != nil {
			t.Fatalf("ParseTaskConfig(null): %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
	})

	t.Run("model_only", func(t *testing.T) {
		raw := []byte(`{"model":"gpt-5","provider":"openai"}`)
		cfg, err := ParseTaskConfig(raw)
		if err != nil {
			t.Fatalf("ParseTaskConfig: %v", err)
		}
		if cfg.Model != "gpt-5" {
			t.Errorf("expected Model=gpt-5, got %q", cfg.Model)
		}
		if cfg.Provider != "openai" {
			t.Errorf("expected Provider=openai, got %q", cfg.Provider)
		}
	})

	t.Run("unknown_fields_kept_in_raw", func(t *testing.T) {
		raw := []byte(`{"model":"gpt-5","provider":"openai","custom_field":"value","nested":{"x":1}}`)
		cfg, err := ParseTaskConfig(raw)
		if err != nil {
			t.Fatalf("ParseTaskConfig: %v", err)
		}
		if cfg.Raw == nil {
			t.Fatal("expected Raw to be populated")
		}
		if v, ok := cfg.Raw["custom_field"].(string); !ok || v != "value" {
			t.Errorf("expected custom_field=value in Raw, got %v", cfg.Raw["custom_field"])
		}
	})

	t.Run("brief_config", func(t *testing.T) {
		raw := []byte(`{"brief":{"mode":"agent"}}`)
		cfg, err := ParseTaskConfig(raw)
		if err != nil {
			t.Fatalf("ParseTaskConfig: %v", err)
		}
		if cfg.Brief == nil || cfg.Brief.Mode != BriefModeAgent {
			t.Errorf("expected Brief.Mode=agent, got %+v", cfg.Brief)
		}
	})

	t.Run("review_config", func(t *testing.T) {
		raw := []byte(`{"review":{"enabled":true,"judgeModel":"gpt-4o","maxIterations":3}}`)
		cfg, err := ParseTaskConfig(raw)
		if err != nil {
			t.Fatalf("ParseTaskConfig: %v", err)
		}
		if cfg.Review == nil || !cfg.Review.Enabled {
			t.Errorf("expected Review.Enabled=true, got %+v", cfg.Review)
		}
		if cfg.Review.JudgeModel != "gpt-4o" {
			t.Errorf("expected JudgeModel=gpt-4o, got %q", cfg.Review.JudgeModel)
		}
		if cfg.Review.MaxIterations != 3 {
			t.Errorf("expected MaxIterations=3, got %d", cfg.Review.MaxIterations)
		}
	})
}

// TestTruncateForLog exercises the helper used in slog calls to keep
// payloads readable.
func TestTruncateForLog(t *testing.T) {
	cases := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hello…"},
		{"whitespace_trimmed", "  hello  ", 10, "hello"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateForLog(tc.input, tc.max)
			if got != tc.want {
				t.Errorf("TruncateForLog(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
			}
		})
	}
}

// TestHeartbeatFailureFuse verifies the constant matches the TS source
// value. We hard-code it (rather than env-overridable) because changing
// it would affect workflow replay semantics — the workflow assumes the
// fuse fires after exactly this many errors.
func TestHeartbeatFailureFuse(t *testing.T) {
	if HeartbeatFailureFuse != 3 {
		t.Errorf("HeartbeatFailureFuse drift: got %d, expected 3 (matches TS source)",
			HeartbeatFailureFuse)
	}
}
