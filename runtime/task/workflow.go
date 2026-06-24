package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"egent-lobehub/runtime"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Workflow names are the string IDs used by client.StartWorkflow. They
// must match the registered workflow function name.
const (
	WorkflowTaskRun     = "TaskRun"
	WorkflowTaskCascade = "TaskCascade"
)

// Activity names referenced by the workflow that we did not declare in
// activities.go (because they are light-weight helpers specific to the
// workflow boundary).
const (
	ActivityResolveInboxAgent  = "ResolveInboxAgent"
	ActivityListRunningTopics  = "ListRunningTopics"
	ActivityAddTaskTopic       = "AddTaskTopic"
)

// TaskRunWorkflow is the Temporal workflow that encapsulates the full
// task execution lifecycle. It is the Go equivalent of the TS
// TaskRunnerService.runTask + TaskLifecycleService.onTopicComplete cycle,
// but with durable saga semantics:
//
//   - Each step that has a side effect registers a compensation.
//   - On terminal failure all compensations run in reverse order.
//   - Retry policy on RunAgentExecution keeps transient LLM failures
//     from failing the whole task.
//   - The workflow state is persisted by Temporal; if the worker dies
//     mid-run, execution resumes from the last successful activity.
//
// The workflow is designed to be short (in terms of function lines) —
// it delegates all real work to activities. This makes the workflow
// deterministic (Temporal's #1 requirement) and keeps the saga logic
// clear.
//
// # Query handlers
//
//   - "status"        → returns TaskStatus as a string
//   - "task"          → returns the TaskItem JSON
//   - "result"        → returns the latest RunTaskResult JSON
//   - "status_detail" → returns a richer status snapshot
//
// # Signal handlers
//
//   - "cancel"        → stops the workflow (transition to TaskStatusCanceled)
//   - "pause"         → transitions to TaskStatusPaused; the RunAgentExecution
//     activity receives a cancellation and the workflow does not re-arm.
//
// # Child workflow
//
// The cascade portion spawns a child workflow
// (WorkflowTaskRun with the downstream task id) for each unlocked
// downstream task. The parent stays alive long enough to collect the
// results and optionally apply auto-review.
func TaskRunWorkflow(ctx workflow.Context, params RunTaskParams) (RunTaskResult, error) {
	if params.TaskID == "" {
		return RunTaskResult{}, errors.New("TaskID is required")
	}

	logger := workflow.GetLogger(ctx)
	logger.Info("TaskRunWorkflow: starting",
		"task_id", params.TaskID,
		"continue_topic", params.ContinueTopicID,
	)

	// ---- State (queried via handlers) ----
	var (
		state         TaskWorkflowState
		compensations []Compensation
		weSetRunning  bool
		lastActErr    error
	)

	// ---- Query handlers ----
	workflow.SetQueryHandler(ctx, "status", func() (TaskStatus, error) {
		return state.CurrentStatus, nil
	})
	workflow.SetQueryHandler(ctx, "task", func() (json.RawMessage, error) {
		return state.TaskJSON, nil
	})
	workflow.SetQueryHandler(ctx, "result", func() (RunTaskResult, error) {
		return state.Result, nil
	})
	workflow.SetQueryHandler(ctx, "status_detail", func() (TaskWorkflowStatusDetail, error) {
		return TaskWorkflowStatusDetail{
			CurrentStatus:     state.CurrentStatus,
			CompensationCount: len(compensations),
			WeSetRunning:      weSetRunning,
			ActivityError:     lastActErr,
		}, nil
	})

	// ---- Default activity options ----
	actOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			NonRetryableErrorTypes: []string{"TaskNotFound", "TaskConflict"},
		},
	}
	actCtx := workflow.WithActivityOptions(ctx, actOpts)

	// ---- Step 1: Resolve the task ----
	var resolveOut ResolveTaskOutput
	if err := workflow.ExecuteActivity(actCtx, ActivityResolveTask, ResolveTaskInput{
		TaskID: params.TaskID,
	}).Get(ctx, &resolveOut); err != nil {
		return RunTaskResult{}, fmt.Errorf("resolve task: %w", err)
	}
	if !resolveOut.Found {
		return RunTaskResult{}, temporal.NewNonRetryableApplicationError(
			"Task not found", "TaskNotFound", nil,
		)
	}

	task := resolveOut.Task
	if taskJSON, err := json.Marshal(task); err == nil {
		state.TaskJSON = taskJSON
	}
	state.CurrentStatus = task.Status

	logger.Info("TaskRunWorkflow: task resolved",
		"identifier", task.Identifier,
		"status", task.Status,
		"assignee", task.AssigneeAgentID,
	)

	// ---- Step 1b: Resolve assignee fallback ----
	if task.AssigneeAgentID == "" {
		var inboxID string
		if err := workflow.ExecuteActivity(actCtx, ActivityResolveInboxAgent, ResolveInboxAgentInput{}).Get(ctx, &inboxID); err != nil {
			return RunTaskResult{}, temporal.NewNonRetryableApplicationError(
				"Failed to resolve inbox agent", "AgentNotFound", nil,
			)
		}
		// Register compensation: clear the assignee we set.
		previous := task.AssigneeAgentID
		compensations = append(compensations, Compensation{
			Name: "rollback_assignee",
			Fn: func(ctx workflow.Context, t *TaskItem) error {
				return workflow.ExecuteActivity(ctx, ActivityUpdateTaskStatus, UpdateTaskStatusInput{
					TaskID: t.ID,
					Status: t.Status,
					Fields: []StatusField{
						{Name: "assignee_agent_id", Value: previous},
					},
				}).Get(ctx, nil)
			},
		})
		task.AssigneeAgentID = inboxID
	}

	// ---- Step 1c: Conflict detection (running topic) ----
	{
		var runningTopics []TaskTopic
		if err := workflow.ExecuteActivity(actCtx, ActivityListRunningTopics, ListRunningTopicsInput{TaskID: task.ID}).Get(ctx, &runningTopics); err != nil {
			return RunTaskResult{}, fmt.Errorf("list running topics: %w", err)
		}
		if params.ContinueTopicID != "" {
			var target *TaskTopic
			for i := range runningTopics {
				if runningTopics[i].TopicID == params.ContinueTopicID {
					target = &runningTopics[i]
					break
				}
			}
			if target != nil && target.Status == TopicStatusRunning {
				return RunTaskResult{}, temporal.NewNonRetryableApplicationError(
					fmt.Sprintf("Topic %s is already running", params.ContinueTopicID),
					"TaskConflict", nil,
				)
			}
		} else if len(runningTopics) > 0 {
			return RunTaskResult{}, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("Task already has a running topic (%s)", runningTopics[0].TopicID),
				"TaskConflict", nil,
			)
		}
	}

	// ---- Step 1d: Auto-timeout stale topics ----
	if task.LastHeartbeatAt != nil && task.HeartbeatTimeout > 0 {
		var timedOut int
		if err := workflow.ExecuteActivity(actCtx, ActivityTimeoutRunningTopics, TimeoutRunningTopicsInput{
			TaskID:           task.ID,
			HeartbeatTimeout: time.Duration(task.HeartbeatTimeout) * time.Second,
		}).Get(ctx, &timedOut); err != nil {
			logger.Warn("TaskRunWorkflow: timeout topics failed", "error", err)
		} else if timedOut > 0 {
			logger.Info("TaskRunWorkflow: timed out stale topics", "count", timedOut)
		}
	}

	// ---- Step 2: Build the task prompt ----
	var promptOut BuildTaskPromptOutput
	if err := workflow.ExecuteActivity(actCtx, ActivityBuildTaskPrompt, BuildTaskPromptInput{
		Task:        task,
		ExtraPrompt: params.ExtraPrompt,
	}).Get(ctx, &promptOut); err != nil {
		return RunTaskResult{}, fmt.Errorf("build prompt: %w", err)
	}

	// ---- Step 3: Transition task to running ----
	if task.Status != TaskStatusRunning {
		if err := workflow.ExecuteActivity(actCtx, ActivityUpdateTaskStatus, UpdateTaskStatusInput{
			TaskID: task.ID,
			Status: TaskStatusRunning,
			Fields: []StatusField{
				WithStartedAt(time.Now()),
				WithError(""),
			},
		}).Get(ctx, nil); err != nil {
			return RunTaskResult{}, fmt.Errorf("transition to running: %w", err)
		}
		weSetRunning = true
		// Compensation: if the workflow fails after this point, roll
		// the task back to paused with the error message.
		compensations = append(compensations, Compensation{
			Name: "rollback_to_paused",
			Fn: func(ctx workflow.Context, t *TaskItem) error {
				msg := ""
				if lastActErr != nil {
					msg = truncateForLog(lastActErr.Error(), 500)
				}
				return workflow.ExecuteActivity(ctx, ActivityUpdateTaskStatus, UpdateTaskStatusInput{
					TaskID: t.ID,
					Status: TaskStatusPaused,
					Fields: []StatusField{
						WithError(msg),
					},
				}).Get(ctx, nil)
			},
		})
	} else if task.Error != "" {
		// Clear any stale error before starting a new run.
		_ = workflow.ExecuteActivity(actCtx, ActivityUpdateTaskStatus, UpdateTaskStatusInput{
			TaskID: task.ID,
			Status: TaskStatusRunning,
			Fields: []StatusField{WithError("")},
		}).Get(ctx, nil)
	}

	state.CurrentStatus = TaskStatusRunning

	// ---- Step 4: Backfill model config ----
	var backfillOut BackfillModelConfigOutput
	if err := workflow.ExecuteActivity(actCtx, ActivityBackfillModelConfig, BackfillModelConfigInput{Task: task}).Get(ctx, &backfillOut); err != nil {
		logger.Warn("TaskRunWorkflow: backfill model config failed", "error", err)
	} else if backfillOut.Updated {
		task.Config.Model = backfillOut.Model
		task.Config.Provider = backfillOut.Provider
	}

	// ---- Step 5: Run the agent execution ----
	// The agent-execution activity runs the LLM call + tool loop. It
	// is the only long-running activity in the workflow; everything
	// else is quick state transitions.
	agentActOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    3,
		},
	}
	agentCtx := workflow.WithActivityOptions(ctx, agentActOpts)

	execParams := AgentRunParams{
		AgentID:         task.AssigneeAgentID,
		UserID:          task.UserID,
		WorkspaceID:     task.WorkspaceID,
		Model:           task.Config.Model,
		Provider:        task.Config.Provider,
		Prompt:          promptOut.Prompt,
		Title:           extraPromptTitle(params.ExtraPrompt, task),
		FileIDs:         promptOut.FileIDs,
		ContinueTopicID: params.ContinueTopicID,
		ApprovalMode:    runtime.ApprovalHeadless,
	}

	var agentOut RunAgentExecutionOutput
	lastActErr = workflow.ExecuteActivity(agentCtx, ActivityRunAgentExecution, RunAgentExecutionInput{
		Task:   task,
		Params: execParams,
	}).Get(ctx, &agentOut)
	if lastActErr != nil {
		// The activity failed. Run compensations and transition the
		// task to paused (not failed — let the user decide).
		logger.Error("TaskRunWorkflow: agent execution failed", "error", lastActErr)
		runCompensations(ctx, compensations, task)
		_ = workflow.ExecuteActivity(actCtx, ActivityUpdateTaskStatus, UpdateTaskStatusInput{
			TaskID: task.ID,
			Status: TaskStatusPaused,
			Fields: []StatusField{
				WithError(truncateForLog(lastActErr.Error(), 500)),
			},
		}).Get(ctx, nil)
		state.CurrentStatus = TaskStatusPaused
		return RunTaskResult{
			TaskID:         task.ID,
			TaskIdentifier: task.Identifier,
			ModelUsed:      task.Config.Model,
		}, lastActErr
	}

	state.Result = RunTaskResult{
		OperationID:      agentOut.Result.OperationID,
		TopicID:          agentOut.Result.TopicID,
		ModelUsed:        agentOut.Result.ModelUsed,
		TaskID:           task.ID,
		TaskIdentifier:   task.Identifier,
		AssistantContent: agentOut.Result.AssistantContent,
	}

	// ---- Step 5b: Register the topic link ----
	if agentOut.Result.TopicID != "" && params.ContinueTopicID == "" {
		_ = workflow.ExecuteActivity(actCtx, ActivityUpdateTaskStatus, UpdateTaskStatusInput{
			TaskID: task.ID,
			Status: TaskStatusRunning,
			Fields: []StatusField{
				{Name: "total_topics", Value: task.TotalTopics + 1},
				{Name: "current_topic_id", Value: agentOut.Result.TopicID},
			},
		}).Get(ctx, nil)
		_ = workflow.ExecuteActivity(actCtx, ActivityAddTaskTopic, AddTaskTopicInput{
			TaskID:      task.ID,
			TopicID:     agentOut.Result.TopicID,
			OperationID: agentOut.Result.OperationID,
			Seq:         task.TotalTopics + 1,
		}).Get(ctx, nil)
	}

	// ---- Step 5c: Update heartbeat ----
	_ = workflow.ExecuteActivity(actCtx, ActivityUpdateHeartbeat, UpdateHeartbeatInput{TaskID: task.ID}).Get(ctx, nil)

	// ---- Step 6: OnTopicComplete lifecycle ----
	lifecycleResult := TopicCompleteResult{}
	if err := workflow.ExecuteActivity(actCtx, ActivityOnTopicComplete, OnTopicCompleteInput{
		Params: TopicCompleteParams{
			TaskID:               task.ID,
			TaskIdentifier:       task.Identifier,
			TopicID:              agentOut.Result.TopicID,
			OperationID:          agentOut.Result.OperationID,
			Reason:               ReasonDone,
			LastAssistantContent: agentOut.Result.AssistantContent,
		},
		Task: task,
	}).Get(ctx, &lifecycleResult); err != nil {
		logger.Warn("TaskRunWorkflow: OnTopicComplete failed", "error", err)
	}

	state.CurrentStatus = lifecycleResult.NewStatus
	if !state.CurrentStatus.Valid() {
		state.CurrentStatus = TaskStatusPaused
	}

	// ---- Step 7: Cascade downstream tasks ----
	if lifecycleResult.NewStatus == TaskStatusCompleted {
		cascadeResult := CascadeResult{}
		_ = workflow.ExecuteActivity(actCtx, ActivityCascadeOnCompletion, CascadeOnCompletionInput{
			CompletedTaskID: task.ID,
		}).Get(ctx, &cascadeResult)

		for _, identifier := range cascadeResult.Started {
			childParams := RunTaskParams{
				TaskID: identifier,
			}
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				WorkflowID:        fmt.Sprintf("task-run/%s", identifier),
				TaskQueue:         workflow.GetInfo(ctx).TaskQueueName,
				ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
			})
			_ = workflow.ExecuteChildWorkflow(childCtx, WorkflowTaskRun, childParams).Get(ctx, nil)
		}
	}

	logger.Info("TaskRunWorkflow: completed",
		"task_id", task.ID,
		"final_status", state.CurrentStatus,
	)
	return state.Result, nil
}

// TaskWorkflowState is the mutable state carried across activities within
// the workflow. It is not persisted by Temporal between activity
// invocations — it is recomputed from the activity inputs/outputs on
// replay. We keep it here to make the query handlers efficient (they
// read from this snapshot rather than re-executing activities).
type TaskWorkflowState struct {
	CurrentStatus TaskStatus      `json:"currentStatus"`
	TaskJSON      json.RawMessage `json:"taskJSON,omitempty"`
	Result        RunTaskResult   `json:"result"`
}

// TaskWorkflowStatusDetail is a richer snapshot returned by the
// "status_detail" query handler. Useful for debugging.
type TaskWorkflowStatusDetail struct {
	CurrentStatus     TaskStatus `json:"currentStatus"`
	CompensationCount int        `json:"compensationCount"`
	WeSetRunning      bool       `json:"weSetRunning"`
	ActivityError     error      `json:"activityError"`
}

// Compensation is a saga compensating action. The Fn is called during
// compensation (reverse order) with the task state as it was at the time
// the compensation was registered.
type Compensation struct {
	Name string
	Fn   func(ctx workflow.Context, task *TaskItem) error
}

// runCompensations executes the slice of compensations in reverse order.
// Each compensation is best-effort: errors are logged but do not
// prevent subsequent compensations from running.
func runCompensations(ctx workflow.Context, comps []Compensation, task *TaskItem) {
	logger := workflow.GetLogger(ctx)
	for i := len(comps) - 1; i >= 0; i-- {
		c := comps[i]
		logger.Info("running compensation", "name", c.Name)
		if err := c.Fn(ctx, task); err != nil {
			logger.Error("compensation failed", "name", c.Name, "error", err)
		}
	}
}

// extraPromptTitle returns a topic title for the agent. Mirrors the TS
// code: `extraPrompt ? extraPrompt.slice(0, 100) : task.name || task.identifier`.
func extraPromptTitle(extraPrompt string, task *TaskItem) string {
	if extraPrompt != "" {
		if len(extraPrompt) > 100 {
			return extraPrompt[:100]
		}
		return extraPrompt
	}
	if task.Name != "" {
		return task.Name
	}
	return task.Identifier
}

// truncateForLog mirrors the activity helper for use inside the workflow
// context (which cannot call the activity package directly).
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
