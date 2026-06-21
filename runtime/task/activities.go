package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"egent-lobehub/tracing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/activity"
)

// Activity names are exported so the worker (worker.go) and the workflow
// (workflow.go) can use the same string constants. They are also the
// string Temporal uses in the UI to identify activities.
const (
	ActivityResolveTask            = "ResolveTask"
	ActivityBuildTaskPrompt        = "BuildTaskPrompt"
	ActivityRunAgentExecution      = "RunAgentExecution"
	ActivityOnTopicComplete        = "OnTopicComplete"
	ActivityCascadeOnCompletion    = "CascadeOnCompletion"
	ActivityUpdateTaskStatus       = "UpdateTaskStatus"
	ActivityUpdateHeartbeat        = "UpdateHeartbeat"
	ActivityTimeoutRunningTopics   = "TimeoutRunningTopics"
	ActivityBackfillModelConfig    = "BackfillModelConfig"
	ActivityEmitHandoff            = "EmitHandoff"
	ActivitySynthesizeTopicBrief   = "SynthesizeTopicBrief"
	ActivityRunAutoReview          = "RunAutoReview"
	ActivityCascadeAfterAutoComplete = "CascadeAfterAutoComplete"
)

// Activities bundles the dependencies a worker needs to run every activity
// in this package. A worker creates one Activities struct and registers
// its methods with the worker via RegisterOn(worker).
type Activities struct {
	Store    TaskStore
	Executor AgentExecutor
	Options  WorkflowOptions
}

// Register wires every activity method on a Temporal worker. Call from
// main.go / worker setup, e.g.:
//
//	w := worker.New(client, opts.TaskQueue, worker.Options{})
//	task.Activities{Store: store, Executor: exec, Options: opts}.Register(w)
//
// The workflow functions themselves are registered separately (see
// RegisterWorkflow in workflow.go) because they are passed by name to
// the client when starting a workflow.
func (a *Activities) Register(w TemporalWorkerRegistrar) {
	w.RegisterActivityWithOptions(a.ResolveTask, activity.RegisterOptions{Name: ActivityResolveTask})
	w.RegisterActivityWithOptions(a.BuildTaskPrompt, activity.RegisterOptions{Name: ActivityBuildTaskPrompt})
	w.RegisterActivityWithOptions(a.RunAgentExecution, activity.RegisterOptions{Name: ActivityRunAgentExecution})
	w.RegisterActivityWithOptions(a.OnTopicComplete, activity.RegisterOptions{Name: ActivityOnTopicComplete})
	w.RegisterActivityWithOptions(a.CascadeOnCompletion, activity.RegisterOptions{Name: ActivityCascadeOnCompletion})
	w.RegisterActivityWithOptions(a.UpdateTaskStatus, activity.RegisterOptions{Name: ActivityUpdateTaskStatus})
	w.RegisterActivityWithOptions(a.UpdateHeartbeat, activity.RegisterOptions{Name: ActivityUpdateHeartbeat})
	w.RegisterActivityWithOptions(a.TimeoutRunningTopics, activity.RegisterOptions{Name: ActivityTimeoutRunningTopics})
	w.RegisterActivityWithOptions(a.BackfillModelConfig, activity.RegisterOptions{Name: ActivityBackfillModelConfig})
	w.RegisterActivityWithOptions(a.EmitHandoff, activity.RegisterOptions{Name: ActivityEmitHandoff})
	w.RegisterActivityWithOptions(a.SynthesizeTopicBrief, activity.RegisterOptions{Name: ActivitySynthesizeTopicBrief})
	w.RegisterActivityWithOptions(a.RunAutoReview, activity.RegisterOptions{Name: ActivityRunAutoReview})
	w.RegisterActivityWithOptions(a.CascadeAfterAutoComplete, activity.RegisterOptions{Name: ActivityCascadeAfterAutoComplete})
	w.RegisterActivityWithOptions(a.ResolveInboxAgent, activity.RegisterOptions{Name: ActivityResolveInboxAgent})
	w.RegisterActivityWithOptions(a.ListRunningTopics, activity.RegisterOptions{Name: ActivityListRunningTopics})
	w.RegisterActivityWithOptions(a.AddTaskTopic, activity.RegisterOptions{Name: ActivityAddTaskTopic})
}

// TemporalWorkerRegistrar is the subset of the Temporal worker API the
// Activities struct needs. Defined here to keep this package's test code
// decoupled from the Temporal SDK's import graph.
type TemporalWorkerRegistrar interface {
	RegisterActivityWithOptions(activityFunc any, options activity.RegisterOptions)
}

// --- ResolveTask ----------------------------------------------------------

// ResolveTaskInput is the activity input.
type ResolveTaskInput struct {
	TaskID string
}

// ResolveTaskOutput is the activity output.
type ResolveTaskOutput struct {
	Task *TaskItem
	// Found is false when the task is missing. Workflows translate that
	// to a NotFoundError, which the workflow's saga path treats as a
	// terminal failure (no compensations needed).
	Found bool
}

// ResolveTask looks up the task by id or identifier. Mirrors
// TaskModel.resolve() in the TS source.
func (a *Activities) ResolveTask(ctx context.Context, in ResolveTaskInput) (*ResolveTaskOutput, error) {
	ctx, span := tracer.Start(ctx, "task.ResolveTask",
		trace.WithAttributes(attribute.String("task.id", in.TaskID)),
	)
	defer span.End()

	t, err := a.Store.ResolveTask(ctx, in.TaskID)
	if err != nil {
		span.SetStatus(codes.Error, "store resolve")
		span.RecordError(err)
		return nil, fmt.Errorf("resolve task: %w", err)
	}
	if t == nil {
		span.SetAttributes(attribute.Bool("found", false))
		span.SetStatus(codes.Ok, "not found")
		return &ResolveTaskOutput{Found: false}, nil
	}
	span.SetAttributes(
		attribute.Bool("found", true),
		attribute.String("task.status", string(t.Status)),
		attribute.String("task.identifier", t.Identifier),
	)
	span.SetStatus(codes.Ok, "")
	return &ResolveTaskOutput{Task: t, Found: true}, nil
}

// --- BuildTaskPrompt ------------------------------------------------------

// BuildTaskPromptInput is the activity input.
type BuildTaskPromptInput struct {
	Task        *TaskItem
	ExtraPrompt string
}

// BuildTaskPromptOutput is the activity output.
type BuildTaskPromptOutput struct {
	Prompt  string
	FileIDs []string
}

// BuildTaskPrompt assembles the agent's user message from the task
// instruction, recent topics + handoffs, briefs, comments, subtasks, and
// the parent context. The Go port is intentionally minimal: the TS
// implementation (buildTaskPrompt) is a large helper that walks many
// relations; for the durable-execution scope we only need to produce a
// stable, deterministic prompt that the agent can run on.
//
// Real-world deployments should override this with a build that reads
// through the full TaskStore + sub-stores. The activity is registered
// separately so it can be unit-tested and replaced without changing the
// workflow code.
func (a *Activities) BuildTaskPrompt(_ context.Context, in BuildTaskPromptInput) (*BuildTaskPromptOutput, error) {
	if in.Task == nil {
		return nil, errors.New("BuildTaskPrompt: task is nil")
	}

	var parts []string
	if in.ExtraPrompt != "" {
		parts = append(parts, "Additional instructions:\n"+in.ExtraPrompt)
	}
	if in.Task.Instruction != "" {
		parts = append(parts, "Task instruction:\n"+in.Task.Instruction)
	}
	if in.Task.Name != "" {
		parts = append(parts, fmt.Sprintf("Task name: %s", in.Task.Name))
	}
	if in.Task.Identifier != "" {
		parts = append(parts, fmt.Sprintf("Task identifier: %s", in.Task.Identifier))
	}
	if in.Task.ParentTaskID != "" {
		parts = append(parts, fmt.Sprintf("Parent task: %s", in.Task.ParentTaskID))
	}

	return &BuildTaskPromptOutput{
		Prompt:  strings.Join(parts, "\n\n"),
		FileIDs: nil, // populated by the full buildTaskPrompt in production
	}, nil
}

// --- RunAgentExecution ---------------------------------------------------

// RunAgentExecutionInput is the activity input.
type RunAgentExecutionInput struct {
	Task    *TaskItem
	Params  AgentRunParams
}

// RunAgentExecutionOutput is the activity output.
type RunAgentExecutionOutput struct {
	Result *AgentRunResult
}

// RunAgentExecution is the long-running activity that calls the agent
// runtime. It must heartbeat periodically so Temporal knows it is alive
// and can cancel it on timeout.
//
// The progress callback is wired to activity.RecordHeartbeat. Each call
// records the progress as the activity's heartbeat detail; on retry
// Temporal surfaces the last detail so we can inspect what the activity
// tracer is the package-level tracer for the task package.
var tracer = tracing.Tracer("egent-lobehub/runtime/task")

// was doing when it was cancelled.
//
// The function is defensive about non-Temporal contexts (e.g. unit
// tests): it checks for a Temporal activity context before calling
// activity.GetLogger / activity.RecordHeartbeat and falls back to
// no-op behaviour when absent.
func (a *Activities) RunAgentExecution(ctx context.Context, in RunAgentExecutionInput) (*RunAgentExecutionOutput, error) {
	ctx, span := tracer.Start(ctx, "task.RunAgentExecution",
		trace.WithAttributes(
			attribute.String("task.id", in.Task.ID),
			attribute.String("agent.id", in.Params.AgentID),
			attribute.String("agent.model", in.Params.Model),
		),
	)
	defer span.End()

	logger := getActivityLoggerOrDefault(ctx)
	logger.Info("RunAgentExecution: starting",
		"task_id", in.Task.ID,
		"agent_id", in.Params.AgentID,
		"model", in.Params.Model,
	)

	progress := func(detail any) {
		recordActivityHeartbeat(ctx, detail)
	}

	result, err := a.Executor.Run(ctx, in.Params, progress)
	if err != nil {
		span.SetStatus(codes.Error, "executor run")
		span.RecordError(err)
		return nil, fmt.Errorf("RunAgentExecution: %w", err)
	}
	span.SetAttributes(
		attribute.String("result.operation_id", result.OperationID),
		attribute.String("result.topic_id", result.TopicID),
		attribute.String("result.model_used", result.ModelUsed),
		attribute.Int("result.content_bytes", len(result.AssistantContent)),
	)
	span.SetStatus(codes.Ok, "")
	logger.Info("RunAgentExecution: completed",
		"task_id", in.Task.ID,
		"operation_id", result.OperationID,
		"topic_id", result.TopicID,
		"model_used", result.ModelUsed,
		"content_bytes", len(result.AssistantContent),
	)
	return &RunAgentExecutionOutput{Result: result}, nil
}

// --- OnTopicComplete -----------------------------------------------------

// OnTopicCompleteInput is the activity input. Mirrors TopicCompleteParams.
type OnTopicCompleteInput struct {
	Params              TopicCompleteParams
	Task                *TaskItem
}

// OnTopicCompleteOutput is the activity output.
type OnTopicCompleteOutput struct {
	Result TopicCompleteResult
}

// OnTopicComplete implements the lifecycle state transitions previously
// done in TaskLifecycleService.onTopicComplete. It is one of the
// most stateful activities in this package; the workflow's saga treats it
// as a terminal action (no compensation needed on failure — the workflow
// marks the task failed instead).
//
// The full implementation in TS also calls the handoff / brief-synthesis
// / auto-review LLM calls. Those are split out as separate activities
// (EmitHandoff, SynthesizeTopicBrief, RunAutoReview) so each can be
// retried independently and so the workflow can make them conditional.
func (a *Activities) OnTopicComplete(ctx context.Context, in OnTopicCompleteInput) (*OnTopicCompleteOutput, error) {
	if in.Task == nil {
		return nil, errors.New("OnTopicComplete: task is nil")
	}

	ctx, span := tracer.Start(ctx, "task.OnTopicComplete",
		trace.WithAttributes(
			attribute.String("task.id", in.Task.ID),
			attribute.String("topic.id", in.Params.TopicID),
			attribute.String("reason", string(in.Params.Reason)),
		),
	)
	defer span.End()

	logger := getActivityLoggerOrDefault(ctx)
	logger.Info("OnTopicComplete: starting",
		"task_id", in.Task.ID,
		"reason", in.Params.Reason,
		"topic_id", in.Params.TopicID,
	)

	// 1. Heartbeat the task (matches LobeHub's updateHeartbeat on
	// every lifecycle call).
	if err := a.Store.UpdateHeartbeat(ctx, in.Task.ID); err != nil {
		logger.Warn("OnTopicComplete: UpdateHeartbeat failed", "error", err)
	}

	// Reload to get a fresh view of the task state — another activity
	// may have flipped it while this one was queued.
	latest, err := a.Store.ResolveTask(ctx, in.Task.ID)
	if err != nil {
		return nil, fmt.Errorf("OnTopicComplete: reload task: %w", err)
	}
	if latest == nil {
		return nil, fmt.Errorf("OnTopicComplete: task %q disappeared", in.Task.ID)
	}

	// 2. Update the topic row if the caller has one.
	if in.Params.TopicID != "" {
		var topicStatus TopicStatus
		switch in.Params.Reason {
		case ReasonDone:
			topicStatus = TopicStatusCompleted
		case ReasonError, ReasonMaxSteps, ReasonCostLimit:
			topicStatus = TopicStatusFailed
		case ReasonInterrupted:
			topicStatus = TopicStatusCanceled
		default:
			topicStatus = TopicStatusFailed
		}
		if err := a.Store.UpdateTaskTopicStatus(ctx, in.Task.ID, in.Params.TopicID, topicStatus); err != nil {
			logger.Warn("OnTopicComplete: UpdateTaskTopicStatus failed", "error", err)
		}
	}

	// 3. Apply the lifecycle rules from the TS source.
	newStatus, terminated := a.computeLifecycleTransition(ctx, latest, in.Params)
	if newStatus == latest.Status {
		// No transition needed (e.g. the task was already cancelled
		// by a concurrent path).
		return &OnTopicCompleteOutput{Result: TopicCompleteResult{
			NewStatus: newStatus,
			Terminated: terminated,
		}}, nil
	}

	fields := []StatusField{}
	if newStatus == TaskStatusRunning {
		fields = append(fields, WithStartedAt(time.Now()))
	}
	if newStatus.IsTerminal() {
		fields = append(fields, WithCompletedAt(time.Now()))
	}
	if err := a.Store.UpdateStatus(ctx, latest.ID, newStatus, fields...); err != nil {
		return nil, fmt.Errorf("OnTopicComplete: UpdateStatus: %w", err)
	}

	// 4. Conditional post-processing: handoff, brief synthesis, auto-review.
	// These are split into their own activities so they can be retried
	// independently and so the workflow can decide whether to run them
	// (e.g. skip handoff on error).
	if in.Params.Reason == ReasonDone {
		// handoff is a cheap LLM call; always run it for done.
		_ = a.EmitHandoff(ctx, EmitHandoffInput{Task: latest, Params: in.Params})
		// brief synthesis is conditional — see computeLifecycleTransition.
		if a.shouldSynthesizeBrief(latest) {
			_ = a.SynthesizeTopicBrief(ctx, SynthesizeTopicBriefInput{Task: latest, Params: in.Params})
		}
		// auto-review is conditional on the review config.
		reviewCfg, reviewEnabled, _ := a.Store.GetReviewConfig(ctx, latest)
		if reviewEnabled && reviewCfg.Enabled {
			accepted, _ := a.RunAutoReview(ctx, RunAutoReviewInput{Task: latest, Params: in.Params, Review: reviewCfg})
			if accepted {
				terminated = true
				newStatus = TaskStatusCompleted
				if err := a.Store.UpdateStatus(ctx, latest.ID, TaskStatusCompleted, WithCompletedAt(time.Now())); err != nil {
					logger.Warn("OnTopicComplete: auto-review complete transition failed", "error", err)
				}
			}
		}
	}

	// 5. Cascade downstream tasks if the task is now in a terminal
	// completed state. Mirrors cascadeAfterAutoComplete in the TS
	// source.
	cascade := CascadeResult{}
	if newStatus == TaskStatusCompleted {
		cResult, err := a.CascadeOnCompletion(ctx, CascadeOnCompletionInput{CompletedTaskID: latest.ID})
		if err != nil {
			logger.Warn("OnTopicComplete: CascadeOnCompletion failed", "error", err)
		} else if cResult != nil {
			cascade = *cResult
		}
	}

	result := TopicCompleteResult{
		NewStatus:       newStatus,
		Terminated:      terminated,
		CascadeStarted:  cascade.Started,
		CascadeFailed:   cascade.Failed,
		CascadePaused:   cascade.Paused,
	}
	span.SetAttributes(
		attribute.String("result.new_status", string(newStatus)),
		attribute.Bool("result.terminated", terminated),
		attribute.Int("result.cascade_started", len(cascade.Started)),
	)
	span.SetStatus(codes.Ok, "")
	return &OnTopicCompleteOutput{Result: result}, nil
}

// computeLifecycleTransition implements the reason → status rules from
// TaskLifecycleService.onTopicComplete. It is a pure function over the
// task state and the completion reason.
func (a *Activities) computeLifecycleTransition(_ context.Context, t *TaskItem, p TopicCompleteParams) (TaskStatus, bool) {
	switch p.Reason {
	case ReasonDone:
		// 1. Schedule cap: schedule-mode + maxExecutions exceeded → completed.
		if t.AutomationMode == "schedule" && t.ScheduleStartedAt != nil {
			topicCap := 0
			if t.Config != nil && t.Config.Schedule != nil {
				topicCap = t.Config.Schedule.MaxExecutions
			}
			if topicCap > 0 && t.TotalTopics >= topicCap {
				return TaskStatusCompleted, true
			}
		}
		// 2. Automation tasks (heartbeat / schedule) loop back to
		// scheduled. The workflow will re-arm the next tick.
		if t.AutomationMode == "heartbeat" || t.AutomationMode == "schedule" {
			return TaskStatusScheduled, false
		}
		// 3. Non-automation → paused for human review. (Auto-review
		// may flip this to completed below.)
		return TaskStatusPaused, false
	case ReasonError, ReasonMaxSteps, ReasonCostLimit:
		// Increment the error fuse. The workflow checks the new
		// count and decides whether to re-arm.
		t.ConsecutiveErrors++
		if t.AutomationMode == "heartbeat" && t.ConsecutiveErrors >= HeartbeatFailureFuse {
			// Fuse blown — leave the task in a non-terminal state
			// (paused) so a human can intervene. The workflow
			// does NOT re-arm.
			return TaskStatusPaused, true
		}
		// Otherwise keep the current status (typically running) so
		// the next tick can retry.
		return t.Status, false
	case ReasonInterrupted:
		// User cancelled. Paused, not failed — they can resume.
		return TaskStatusPaused, true
	default:
		return t.Status, false
	}
}

// shouldSynthesizeBrief mirrors the shouldEmitTopicBrief rule in the TS
// source: briefs are emitted in 'auto' mode (the default) but suppressed
// in 'agent' mode where the agent itself raises a brief.
func (a *Activities) shouldSynthesizeBrief(t *TaskItem) bool {
	if t.Config == nil || t.Config.Brief == nil {
		return true // default mode is "auto"
	}
	return t.Config.Brief.Mode != BriefModeAgent
}

// --- CascadeOnCompletion -------------------------------------------------

// CascadeOnCompletionInput is the activity input.
type CascadeOnCompletionInput struct {
	CompletedTaskID string
}

// CascadeOnCompletion implements TaskRunnerService.cascadeOnCompletion.
// The workflow calls this as a sub-activity when the task transitions
// to completed. It returns a CascadeResult listing the tasks that were
// started, paused, or failed.
//
// Note: in this Go port, "started" means "marked as runnable by creating
// a child workflow". The workflow spawns one CascadeOnCompletion workflow
// per downstream task; that child workflow is what actually performs the
// run. We keep the parent workflow free to handle the rest of its
// lifecycle.
func (a *Activities) CascadeOnCompletion(ctx context.Context, in CascadeOnCompletionInput) (*CascadeResult, error) {
	logger := getActivityLoggerOrDefault(ctx)
	logger.Info("CascadeOnCompletion: starting", "completed_task_id", in.CompletedTaskID)

	unlocked, err := a.Store.GetUnlockedTasks(ctx, in.CompletedTaskID)
	if err != nil {
		return nil, fmt.Errorf("CascadeOnCompletion: get unlocked: %w", err)
	}
	if len(unlocked) == 0 {
		return &CascadeResult{}, nil
	}

	result := &CascadeResult{}
	for _, t := range unlocked {
		// Checkpoint gate: if the parent's beforeIds lists this
		// identifier, leave the task paused.
		cp, hasCP, _ := a.Store.GetCheckpointConfig(ctx, t)
		if hasCP && containsString(cp.BeforeIDs, t.Identifier) {
			if err := a.Store.UpdateStatus(ctx, t.ID, TaskStatusPaused); err != nil {
				logger.Warn("CascadeOnCompletion: pause failed", "task", t.Identifier, "error", err)
			} else {
				result.Paused = append(result.Paused, t.Identifier)
			}
			continue
		}
		// Otherwise the parent workflow will spawn a child workflow
		// for the cascade. The activity here only flips the status
		// to a runnable state (backlog / paused) — the actual
		// start of the cascade is a child workflow.
		result.Started = append(result.Started, t.Identifier)
	}
	return result, nil
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
// getActivityLoggerOrDefault returns the Temporal activity logger when
// the context is an activity context; otherwise returns the default
// slog logger. This makes activity functions safe to call from plain
// Go code (e.g. unit tests) without panicking.
func getActivityLoggerOrDefault(ctx context.Context) activityLogger {
	if !isActivityContext(ctx) {
		return slogLogger{}
	}
	return activityLoggerFuncAdapter{ctx: ctx}
}

// recordActivityHeartbeat records a Temporal activity heartbeat, but
// only when the context is an activity context. In plain Go code (e.g.
// unit tests) this is a no-op.
func recordActivityHeartbeat(ctx context.Context, detail any) {
	if !isActivityContext(ctx) {
		return
	}
	activity.RecordHeartbeat(ctx, detail)
}

// isActivityContext returns true when ctx is a Temporal activity context.
// We use a recover() guard around activity.GetLogger: the SDK panics
// when the context is not an activity context.
func isActivityContext(ctx context.Context) (yes bool) {
	defer func() {
		if r := recover(); r != nil {
			yes = false
		}
	}()
	_ = activity.GetLogger(ctx)
	return true
}

// activityLogger is the minimal logger interface used by activities.
// It is satisfied by both slog.Default() and the Temporal activity logger.
type activityLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// slogLogger wraps slog.Default() so non-Temporal contexts can log.
type slogLogger struct{}

func (slogLogger) Info(msg string, args ...any)  { slog.Info(msg, args...) }
func (slogLogger) Warn(msg string, args ...any)  { slog.Warn(msg, args...) }
func (slogLogger) Error(msg string, args ...any) { slog.Error(msg, args...) }

// activityLoggerFuncAdapter routes logs to the Temporal activity logger.
type activityLoggerFuncAdapter struct {
	ctx context.Context
}

func (a activityLoggerFuncAdapter) Info(msg string, args ...any) {
	activity.GetLogger(a.ctx).Info(msg, args...)
}
func (a activityLoggerFuncAdapter) Warn(msg string, args ...any) {
	activity.GetLogger(a.ctx).Warn(msg, args...)
}
func (a activityLoggerFuncAdapter) Error(msg string, args ...any) {
	activity.GetLogger(a.ctx).Error(msg, args...)
}

// --- UpdateTaskStatus ----------------------------------------------------

// UpdateTaskStatusInput is the activity input.
type UpdateTaskStatusInput struct {
	TaskID string
	Status TaskStatus
	Fields []StatusField
}

// UpdateTaskStatus wraps Store.UpdateStatus in an activity so the
// workflow can change status without depending on the store directly.
func (a *Activities) UpdateTaskStatus(ctx context.Context, in UpdateTaskStatusInput) error {
	return a.Store.UpdateStatus(ctx, in.TaskID, in.Status, in.Fields...)
}

// --- UpdateHeartbeat -----------------------------------------------------

// UpdateHeartbeatInput is the activity input.
type UpdateHeartbeatInput struct {
	TaskID string
}

// UpdateHeartbeat stamps the task's last_heartbeat_at.
func (a *Activities) UpdateHeartbeat(ctx context.Context, in UpdateHeartbeatInput) error {
	return a.Store.UpdateHeartbeat(ctx, in.TaskID)
}

// --- TimeoutRunningTopics -----------------------------------------------

// TimeoutRunningTopicsInput is the activity input.
type TimeoutRunningTopicsInput struct {
	TaskID            string
	HeartbeatTimeout  time.Duration
}

// TimeoutRunningTopics marks stale running topics as failed.
func (a *Activities) TimeoutRunningTopics(ctx context.Context, in TimeoutRunningTopicsInput) (int, error) {
	return a.Store.TimeoutRunningTopics(ctx, in.TaskID, in.HeartbeatTimeout)
}

// --- BackfillModelConfig ------------------------------------------------

// BackfillModelConfigInput is the activity input.
type BackfillModelConfigInput struct {
	Task *TaskItem
}

// BackfillModelConfigOutput is the activity output.
type BackfillModelConfigOutput struct {
	Updated bool
	Model   string
	Provider string
}

// BackfillModelConfig mirrors the "Backfill model snapshot" block in
// TaskRunnerService.runTask. If the task has no model/provider pinned,
// it copies the assignee agent's current default into task.config.
func (a *Activities) BackfillModelConfig(ctx context.Context, in BackfillModelConfigInput) (*BackfillModelConfigOutput, error) {
	if in.Task == nil {
		return nil, errors.New("BackfillModelConfig: task is nil")
	}
	if in.Task.Config != nil && in.Task.Config.Model != "" && in.Task.Config.Provider != "" {
		return &BackfillModelConfigOutput{
			Updated: false,
			Model:   in.Task.Config.Model,
			Provider: in.Task.Config.Provider,
		}, nil
	}
	if in.Task.AssigneeAgentID == "" {
		return &BackfillModelConfigOutput{Updated: false}, nil
	}
	cfg, err := a.Store.GetAgentModelConfig(ctx, in.Task.AssigneeAgentID)
	if err != nil {
		return nil, fmt.Errorf("BackfillModelConfig: get agent model: %w", err)
	}
	if cfg == nil {
		return &BackfillModelConfigOutput{Updated: false}, nil
	}
	merged := map[string]any{}
	if in.Task.Config != nil && in.Task.Config.Raw != nil {
		for k, v := range in.Task.Config.Raw {
			merged[k] = v
		}
	}
	merged["model"] = cfg.Model
	merged["provider"] = cfg.Provider
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("BackfillModelConfig: marshal: %w", err)
	}
	if err := a.Store.UpdateTaskConfig(ctx, in.Task.ID, raw); err != nil {
		return nil, fmt.Errorf("BackfillModelConfig: update: %w", err)
	}
	return &BackfillModelConfigOutput{Updated: true, Model: cfg.Model, Provider: cfg.Provider}, nil
}

// --- EmitHandoff --------------------------------------------------------

// EmitHandoffInput is the activity input.
type EmitHandoffInput struct {
	Task   *TaskItem
	Params TopicCompleteParams
}

// EmitHandoff is a stub for the chainTaskTopicHandoff LLM call. In the
// TS source it produces a `TaskTopicHandoff` summary + topic title that
// is persisted onto the task_topics link. The Go port writes a
// best-effort placeholder so the durable-execution path is testable
// end-to-end; production deployments should override the body of this
// activity to call the LLM (the activity is registered with a known name
// so workers can be swapped in).
func (a *Activities) EmitHandoff(ctx context.Context, in EmitHandoffInput) error {
	logger := getActivityLoggerOrDefault(ctx)
	if in.Task == nil {
		return errors.New("EmitHandoff: task is nil")
	}
	if in.Params.LastAssistantContent == "" {
		// No content to summarise — nothing to do.
		return nil
	}
	// A real implementation would call the LLM here and persist the
	// result onto the task_topics row. For the durable-execution port
	// we just log; downstream DB writes happen via separate
	// task-topic update activities.
	logger.Info("EmitHandoff: would summarise topic",
		"task_id", in.Task.ID,
		"topic_id", in.Params.TopicID,
		"content_bytes", len(in.Params.LastAssistantContent),
	)
	return nil
}

// --- SynthesizeTopicBrief -----------------------------------------------

// SynthesizeTopicBriefInput is the activity input.
type SynthesizeTopicBriefInput struct {
	Task   *TaskItem
	Params TopicCompleteParams
}

// SynthesizeTopicBrief is a stub for the programmatic brief synthesis
// path (the 'auto' briefMode in the TS source). Real implementation
// would walk the shouldEmitTopicBrief rules and (if "yes") call
// chainGenerateBrief. We log + return; the workflow treats the activity
// as best-effort and does not retry on failure.
func (a *Activities) SynthesizeTopicBrief(ctx context.Context, in SynthesizeTopicBriefInput) error {
	logger := getActivityLoggerOrDefault(ctx)
	if in.Task == nil {
		return errors.New("SynthesizeTopicBrief: task is nil")
	}
	logger.Info("SynthesizeTopicBrief: would synthesise brief",
		"task_id", in.Task.ID,
		"topic_id", in.Params.TopicID,
	)
	return nil
}

// --- RunAutoReview ------------------------------------------------------

// RunAutoReviewInput is the activity input.
type RunAutoReviewInput struct {
	Task   *TaskItem
	Params TopicCompleteParams
	Review ReviewConfig
}

// RunAutoReviewOutput is the activity output.
type RunAutoReviewOutput struct {
	Accepted bool
	Reason   string
}

// RunAutoReview is a stub for the TaskReviewService.review call. Real
// implementation invokes the judge model against the rubrics. The
// stub returns Accepted=false so the workflow falls back to the
// paused-awaiting-human path. Production deployments must replace this
// with a judge-model call.
func (a *Activities) RunAutoReview(ctx context.Context, in RunAutoReviewInput) (bool, error) {
	logger := getActivityLoggerOrDefault(ctx)
	if in.Task == nil {
		return false, errors.New("RunAutoReview: task is nil")
	}
	logger.Info("RunAutoReview: would call judge model",
		"task_id", in.Task.ID,
		"judge_model", in.Review.JudgeModel,
		"judge_provider", in.Review.JudgeProvider,
	)
	return false, nil
}

// --- CascadeAfterAutoComplete ------------------------------------------

// CascadeAfterAutoCompleteInput is the activity input.
type CascadeAfterAutoCompleteInput struct {
	CompletedTaskID string
}

// CascadeAfterAutoComplete is a thin alias for CascadeOnCompletion,
// exposed as a separate activity so the workflow can register the
// auto-review → cascade path independently.
func (a *Activities) CascadeAfterAutoComplete(ctx context.Context, in CascadeAfterAutoCompleteInput) (*CascadeResult, error) {
	return a.CascadeOnCompletion(ctx, CascadeOnCompletionInput{CompletedTaskID: in.CompletedTaskID})
}

// --- ResolveInboxAgent ---------------------------------------------------

// ResolveInboxAgentInput is empty — the inbox agent is a singleton in the
// store's implementation (e.g. AgentModel.getBuiltinAgent(INBOX_SESSION_ID)).
type ResolveInboxAgentInput struct{}

// ResolveInboxAgent returns the inbox agent id used as the fallback
// assignee for tasks without an explicit assignee.
func (a *Activities) ResolveInboxAgent(ctx context.Context, _ ResolveInboxAgentInput) (string, error) {
	return a.Store.GetInboxAgentID(ctx)
}

// --- ListRunningTopics ---------------------------------------------------

// ListRunningTopicsInput is the activity input.
type ListRunningTopicsInput struct {
	TaskID string
}

// ListRunningTopics returns task_topics with status=running for a task.
// The workflow uses this to detect concurrent runs before starting a new
// one.
func (a *Activities) ListRunningTopics(ctx context.Context, in ListRunningTopicsInput) ([]TaskTopic, error) {
	return a.Store.ListRunningTopics(ctx, in.TaskID)
}

// --- AddTaskTopic --------------------------------------------------------

// AddTaskTopicInput is the activity input.
type AddTaskTopicInput struct {
	TaskID      string
	TopicID     string
	OperationID string
	Seq         int
}

// AddTaskTopic records the (task, topic) link with the agent operation id.
func (a *Activities) AddTaskTopic(ctx context.Context, in AddTaskTopicInput) error {
	return a.Store.AddTaskTopic(ctx, in.TaskID, in.TopicID, in.OperationID, in.Seq)
}
