// Package task implements durable task execution for LobeHub using Temporal.
//
// This package is the Go equivalent of LobeHub's TypeScript TaskRunnerService
// + TaskLifecycleService pair (apps/server/src/services/taskRunner/ and
// apps/server/src/services/taskLifecycle/). The original implementation runs
// in a Next.js request handler — once a request returns, the task lifecycle
// is held in memory and there is no durable recovery path if the process
// dies mid-execution.
//
// This port moves the task lifecycle into Temporal workflows. The key
// differences are:
//
//   - Activities are retryable. A flaky LLM call no longer fails the whole
//     task — Temporal retries it with exponential backoff until success or
//     the configured maximum elapsed time.
//   - The workflow state is persisted by Temporal (Cassandra / MySQL /
//     PostgreSQL backend). If the worker process is killed mid-execution,
//     the workflow resumes from the last completed activity on the next
//     worker startup.
//   - The workflow is a saga: compensations are registered as work proceeds
//     and executed in reverse order on terminal failure. This gives the
//     "atomic multi-step task" guarantee that the TS code approximates with
//     ad-hoc try/catch rollback.
//   - The workflow exposes query + signal handlers for status, pause, and
//     cancel — which is the durable equivalent of the TS hooks
//     (beforeCompact, afterCompact, etc.) for a long-running run.
//
// LobeHub TS source files mapped here:
//
//   - taskRunner/index.ts           → workflow.go (TaskWorkflow)
//   - taskRunner/heartbeatTick.ts   → activities.go (RunAgentExecution activity + heartbeats)
//   - taskRunner/scheduleTick.ts    → activities.go (RunScheduleTick activity)
//   - taskLifecycle/index.ts        → activities.go (OnTopicComplete activity)
package task

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TaskStatus mirrors the LobeHub status enum from packages/types.
// Values map 1:1 to the string representations stored in the database.
type TaskStatus string

const (
	// TaskStatusBacklog is the initial state of a newly created task.
	TaskStatusBacklog TaskStatus = "backlog"
	// TaskStatusPaused is set when the task awaits human review or has been
	// interrupted. It is the resting state for non-automation tasks after
	// a run completes without auto-approval.
	TaskStatusPaused TaskStatus = "paused"
	// TaskStatusScheduled is set for automation tasks (heartbeat / schedule
	// mode) after a run completes successfully — the next tick will re-arm.
	TaskStatusScheduled TaskStatus = "scheduled"
	// TaskStatusRunning is set while an agent execution is in flight.
	TaskStatusRunning TaskStatus = "running"
	// TaskStatusCompleted is terminal — the task ran to completion and (if
	// review was enabled) was auto-approved.
	TaskStatusCompleted TaskStatus = "completed"
	// TaskStatusFailed is terminal — the workflow exhausted retries.
	TaskStatusFailed TaskStatus = "failed"
	// TaskStatusCanceled is terminal — the user explicitly canceled the run.
	TaskStatusCanceled TaskStatus = "canceled"
)

// IsTerminal reports whether the status is terminal (no further transitions
// are valid). Mirrors TERMINAL_STATUSES in the TS source.
func (s TaskStatus) IsTerminal() bool {
	switch s {
	case TaskStatusCompleted, TaskStatusFailed, TaskStatusCanceled:
		return true
	}
	return false
}

// Valid reports whether s is a known status value.
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskStatusBacklog, TaskStatusPaused, TaskStatusScheduled,
		TaskStatusRunning, TaskStatusCompleted, TaskStatusFailed,
		TaskStatusCanceled:
		return true
	}
	return false
}

// TopicStatus mirrors the topic lifecycle state stored in task_topics.
type TopicStatus string

const (
	TopicStatusRunning   TopicStatus = "running"
	TopicStatusCompleted TopicStatus = "completed"
	TopicStatusFailed    TopicStatus = "failed"
	TopicStatusCanceled  TopicStatus = "canceled"
)

// CompleteReason mirrors the LobeHub topic-completion reason enum. Values
// come from the agent runtime's onComplete hook (event.reason) and are
// passed through to OnTopicComplete.
type CompleteReason string

const (
	// ReasonDone — agent finished normally (assistant message + finish_reason=stop).
	ReasonDone CompleteReason = "done"
	// ReasonError — agent threw an exception or returned a tool error.
	ReasonError CompleteReason = "error"
	// ReasonInterrupted — user canceled or runtime interrupted the execution.
	ReasonInterrupted CompleteReason = "interrupted"
	// ReasonMaxSteps — agent hit the configured step / cost ceiling.
	ReasonMaxSteps CompleteReason = "max_steps"
	// ReasonCostLimit — agent hit the configured cost ceiling.
	ReasonCostLimit CompleteReason = "cost_limit"
)

// RunTaskParams is the Go equivalent of LobeHub's RunTaskParams. The
// `ContinueTopicID` and `ExtraPrompt` fields map 1:1.
//
// This is the input to both the StartTaskWorkflow HTTP handler and the
// TaskWorkflow Temporal workflow itself.
type RunTaskParams struct {
	// TaskID is the database id OR the human-readable identifier of the
	// task. Mirrors the TS implementation which resolves the same field
	// against TaskModel.resolve.
	TaskID string
	// ContinueTopicID, when set, points at a non-running topic to resume.
	// If the topic is still running, the workflow returns a Conflict error.
	ContinueTopicID string
	// ExtraPrompt augments the task instruction with an ad-hoc prefix
	// (e.g. "focus on the second milestone").
	ExtraPrompt string
}

// RunTaskResult mirrors the TS RunTaskResult. It extends ExecAgentResult
// with task-specific identifiers and the resulting topic ID.
type RunTaskResult struct {
	// OperationID is the Eino agent operation id. Maps to the onComplete
	// hook's `event.operationId` and is used to interrupt a running agent
	// from a separate request.
	OperationID string
	// TopicID is the LobeHub topic created for this run. Stored in
	// task_topics.operationId + the topic row itself.
	TopicID string
	// AgentID is the resolved assignee agent. May differ from the task's
	// stored assigneeAgentId if the inbox fallback fired.
	AgentID string
	// ModelUsed is the model the agent actually ran on (after fallback
	// resolution and the task.config snapshot backfill).
	ModelUsed string
	// TaskID is the canonical task id (not the identifier).
	TaskID string
	// TaskIdentifier is the human-readable identifier.
	TaskIdentifier string
}

// TopicCompleteParams maps to the TS TopicCompleteParams. Carries the data
// needed by the lifecycle activity to transition the task state and run
// post-processing (handoff, brief synthesis, auto-review).
type TopicCompleteParams struct {
	// TaskID is the task to transition.
	TaskID string
	// TaskIdentifier mirrors the TS interface — passed through for
	// logging and is also what the TS side passes to QStash webhook
	// callbacks so the production pipeline can re-deliver the call.
	TaskIdentifier string
	// TopicID is optional — when a topic row exists for this completion
	// the lifecycle can update its status alongside the task's.
	TopicID string
	// OperationID matches the Eino agent operation id.
	OperationID string
	// Reason drives the state transition. See CompleteReason constants.
	Reason CompleteReason
	// LastAssistantContent is the final assistant message from the agent.
	// Used by the handoff and brief-synthesis LLM calls.
	LastAssistantContent string
	// ErrorMessage is set when Reason is ReasonError / ReasonInterrupted.
	ErrorMessage string
}

// TopicCompleteResult is returned by the OnTopicComplete activity. The
// `Terminated` flag tells the calling workflow whether the task has reached
// a terminal state (completed) and should not be re-armed by the heartbeat
// loop.
type TopicCompleteResult struct {
	// Terminated is true when the task has reached TaskStatusCompleted
	// (or failed). The workflow will not re-arm.
	Terminated bool
	// NewStatus is the task's post-transition status.
	NewStatus TaskStatus
	// CascadeStarted lists identifiers of downstream tasks that were
	// kicked off as part of the completion cascade. Mirrors
	// CascadeResult.started.
	CascadeStarted []string
	// CascadeFailed mirrors CascadeResult.failed.
	CascadeFailed []CascadeFailure
	// CascadePaused mirrors CascadeResult.paused.
	CascadePaused []string
}

// CascadeFailure mirrors the TS CascadeResult.failed entry.
type CascadeFailure struct {
	Identifier string `json:"identifier"`
	Error      string `json:"error"`
}

// CascadeResult mirrors the TS CascadeResult type. Returned by
// CascadeOnCompletionActivity and surfaced in the HTTP response.
type CascadeResult struct {
	Failed  []CascadeFailure `json:"failed"`
	Paused  []string         `json:"paused"`
	Started []string         `json:"started"`
}

// BriefMode is the value of task.config.brief.mode. "auto" is the default
// and synthesises briefs programmatically; "agent" is a legacy escape hatch
// that re-enables the createBrief tool.
type BriefMode string

const (
	BriefModeAuto  BriefMode = "auto"
	BriefModeAgent BriefMode = "agent"
)

// TaskConfig is a permissive view of the task.config JSON blob. The TS
// implementation casts through `as Record<string, unknown>`; we surface the
// fields we actually consume and ignore the rest.
type TaskConfig struct {
	Model    string                 `json:"model,omitempty"`
	Provider string                 `json:"provider,omitempty"`
	Brief    *BriefConfig           `json:"brief,omitempty"`
	Review   *ReviewConfig          `json:"review,omitempty"`
	Schedule *ScheduleConfig        `json:"schedule,omitempty"`
	Raw      map[string]any         `json:"-"` // for fields we don't model
}

// BriefConfig mirrors task.config.brief.
type BriefConfig struct {
	Mode BriefMode `json:"mode,omitempty"`
}

// ReviewConfig mirrors task.config.review. When Enabled, the lifecycle
// runs the judge model after each successful topic completion and only
// marks the task complete if the judge accepts.
type ReviewConfig struct {
	Enabled       bool   `json:"enabled"`
	MaxIterations int    `json:"maxIterations,omitempty"`
	JudgeModel    string `json:"judgeModel,omitempty"`
	JudgeProvider string `json:"judgeProvider,omitempty"`
}

// ScheduleConfig mirrors task.config.schedule. The MaxExecutions field
// acts as a defense-in-depth cap (the lifecycle checks the actual topic
// count too).
type ScheduleConfig struct {
	MaxExecutions int `json:"maxExecutions,omitempty"`
}

// TaskItem is the in-memory representation of a task row. It is loaded by
// ResolveTaskActivity and passed through the workflow. Only the fields the
// workflow and its activities consume are modelled.
type TaskItem struct {
	ID                 string       `json:"id"`
	Identifier         string       `json:"identifier"`
	UserID             string       `json:"userId"`
	WorkspaceID        string       `json:"workspaceId,omitempty"`
	Name               string       `json:"name"`
	Instruction        string       `json:"instruction"`
	Status             TaskStatus   `json:"status"`
	Error              string       `json:"error,omitempty"`
	AssigneeAgentID    string       `json:"assigneeAgentId,omitempty"`
	ParentTaskID       string       `json:"parentTaskId,omitempty"`
	CurrentTopicID     string       `json:"currentTopicId,omitempty"`
	TotalTopics        int          `json:"totalTopics"`
	LastHeartbeatAt    *time.Time   `json:"lastHeartbeatAt,omitempty"`
	HeartbeatTimeout   int          `json:"heartbeatTimeout,omitempty"`   // seconds
	HeartbeatInterval  int          `json:"heartbeatInterval,omitempty"`  // seconds
	SchedulePattern    string       `json:"schedulePattern,omitempty"`
	AutomationMode     string       `json:"automationMode,omitempty"`     // "heartbeat" | "schedule" | ""
	ConsecutiveErrors  int          `json:"consecutiveErrors,omitempty"`
	ScheduleStartedAt  *time.Time   `json:"scheduleStartedAt,omitempty"`
	Config             *TaskConfig  `json:"config,omitempty"`
	CreatedAt          time.Time    `json:"createdAt"`
	UpdatedAt          time.Time    `json:"updatedAt"`
	StartedAt          *time.Time   `json:"startedAt,omitempty"`
	CompletedAt        *time.Time   `json:"completedAt,omitempty"`
}

// HeartbeatFailureFuse mirrors HEARTBEAT_FAILURE_FUSE in the TS source.
// After this many consecutive ReasonError completions, the workflow
// surfaces the task for human attention instead of re-arming the next tick.
const HeartbeatFailureFuse = 3

// WorkflowOptions holds the Temporal task queue + retry policy for the
// task workflows. Read at worker startup. Defaults are applied for any
// zero-valued field.
type WorkflowOptions struct {
	// TaskQueue is the Temporal task queue name. Defaults to "lobehub-tasks".
	TaskQueue string
	// WorkflowExecutionTimeout caps a single workflow execution.
	// Defaults to 24h (most long-running agent tasks complete in minutes,
	// but heartbeat-mode workflows can run for days).
	WorkflowExecutionTimeout time.Duration
	// ActivityStartToCloseTimeout caps a single activity attempt.
	// Defaults to 10m.
	ActivityStartToCloseTimeout time.Duration
	// AgentExecutionHeartbeatTimeout is the heartbeat interval for the
	// long-running RunAgentExecution activity. The activity is expected
	// to call RecordHeartbeat at least this often; if it doesn't, Temporal
	// cancels the activity and retries it. Defaults to 30s.
	AgentExecutionHeartbeatTimeout time.Duration
	// MaxAgentExecutionAttempts caps the number of times the
	// RunAgentExecution activity will be retried by Temporal. After
	// this is exhausted the workflow transitions the task to TaskStatusFailed.
	// Defaults to 3.
	MaxAgentExecutionAttempts int32
	// InitialRetryInterval is the first backoff interval between activity
	// retries. Defaults to 5s.
	InitialRetryInterval time.Duration
	// MaxRetryInterval caps the exponential backoff. Defaults to 1m.
	MaxRetryInterval time.Duration
}

// WithDefaults fills in zero-valued fields with sensible defaults.
func (o *WorkflowOptions) WithDefaults() {
	if o.TaskQueue == "" {
		o.TaskQueue = "lobehub-tasks"
	}
	if o.WorkflowExecutionTimeout == 0 {
		o.WorkflowExecutionTimeout = 24 * time.Hour
	}
	if o.ActivityStartToCloseTimeout == 0 {
		o.ActivityStartToCloseTimeout = 10 * time.Minute
	}
	if o.AgentExecutionHeartbeatTimeout == 0 {
		o.AgentExecutionHeartbeatTimeout = 30 * time.Second
	}
	if o.MaxAgentExecutionAttempts == 0 {
		o.MaxAgentExecutionAttempts = 3
	}
	if o.InitialRetryInterval == 0 {
		o.InitialRetryInterval = 5 * time.Second
	}
	if o.MaxRetryInterval == 0 {
		o.MaxRetryInterval = time.Minute
	}
}

// String returns a short, log-friendly description.
func (o WorkflowOptions) String() string {
	return fmt.Sprintf("TaskQueue=%s ExecTimeout=%s ActivityTimeout=%s HeartbeatTimeout=%s MaxAttempts=%d",
		o.TaskQueue, o.WorkflowExecutionTimeout, o.ActivityStartToCloseTimeout,
		o.AgentExecutionHeartbeatTimeout, o.MaxAgentExecutionAttempts)
}

// ParseTaskConfig unmarshals a JSON blob into a TaskConfig, tolerating
// unknown fields (the TS side uses `as Record<string, unknown>` casts).
func ParseTaskConfig(raw json.RawMessage) (*TaskConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return &TaskConfig{}, nil
	}
	var c TaskConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse task.config: %w", err)
	}
	// Stash the raw map for fields we don't explicitly model.
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err == nil {
		c.Raw = rawMap
	}
	return &c, nil
}

// TruncateForLog returns a log-friendly representation of a possibly long
// string. Used for prompt and content fields in the slog calls.
func TruncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
