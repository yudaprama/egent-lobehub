package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// WorkerConfig is the bundle a caller (typically main.go) passes to
// NewWorker. It binds the durable-execution layer to a concrete store +
// agent executor + Temporal client.
type WorkerConfig struct {
	// Client is the Temporal client the worker connects to. If nil,
	// NewWorker will not start a worker (callers can use
	// StartWithClient to attach to an existing client).
	Client client.Client
	// Store is the persistence layer (TaskStore).
	Store TaskStore
	// Executor is the agent runner. Defaults to NoopExecutor when nil.
	Executor AgentExecutor
	// Options is the workflow options. WithDefaults() is called on it.
	Options WorkflowOptions
	// Logger is the slog logger for the worker. If nil, the default
	// logger is used.
	Logger *slog.Logger
}

// Worker is the in-process Temporal worker. It registers the workflow
// and activities and runs in a goroutine; Stop is called at shutdown.
//
// The worker is intentionally lightweight — it carries no state beyond
// the Temporal client and the registered activity handle. All
// persistence is through the store and Temporal itself.
type Worker struct {
	cfg    WorkerConfig
	w      worker.Worker
	client client.Client
	logger *slog.Logger
	mu     sync.Mutex
	stopped bool
}

// NewWorker constructs the worker. It does not start it — call Start
// explicitly so the caller can register signal handlers first.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Store == nil {
		return nil, errors.New("task: WorkerConfig.Store is required")
	}
	if cfg.Executor == nil {
		cfg.Executor = NoopExecutor{}
	}
	cfg.Options.WithDefaults()
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &Worker{
		cfg:    cfg,
		client: cfg.Client,
		logger: logger,
	}
	return w, nil
}

// Start creates the Temporal worker, registers the workflow and
// activities, and begins polling. It is non-blocking; call Stop at
// shutdown.
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.w != nil {
		return errors.New("task: worker already started")
	}
	if w.client == nil {
		return errors.New("task: WorkerConfig.Client is nil; cannot start worker")
	}
	c := worker.New(w.client, w.cfg.Options.TaskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:      64,
		MaxConcurrentWorkflowTaskExecutionSize: 64,
	})
	// Register the workflow.
	c.RegisterWorkflowWithOptions(TaskRunWorkflow, workflow.RegisterOptions{
		Name: WorkflowTaskRun,
	})
	// Register the activities.
	acts := &Activities{Store: w.cfg.Store, Executor: w.cfg.Executor, Options: w.cfg.Options}
	acts.Register(c)

	w.w = c
	w.logger.Info("task worker: starting",
		"task_queue", w.cfg.Options.TaskQueue,
		"workflow_options", w.cfg.Options.String(),
	)
	return c.Start()
}

// Stop stops the worker. Safe to call multiple times. Returns an error
// only if the underlying Temporal client is unable to close; the worker
// itself is stopped regardless.
func (w *Worker) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return nil
	}
	w.stopped = true
	if w.w != nil {
		w.w.Stop()
	}
	if w.client != nil {
		w.client.Close()
	}
	return nil
}

// Started reports whether the worker is currently running.
func (w *Worker) Started() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w != nil && !w.stopped
}

// StartTaskWorkflow kicks off a TaskRunWorkflow. The caller (typically
// the HTTP handler) gets back the workflow run id immediately — the
// workflow runs asynchronously. Use QueryTaskStatus to poll.
func StartTaskWorkflow(ctx context.Context, c client.Client, params RunTaskParams, opts WorkflowOptions) (client.WorkflowRun, error) {
	if c == nil {
		return nil, errors.New("task: client is nil")
	}
	opts.WithDefaults()
	rp := temporalRetryPolicy(opts)
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:          workflowID(params.TaskID),
		TaskQueue:   opts.TaskQueue,
		RetryPolicy: &rp,
	}, WorkflowTaskRun, params)
	if err != nil {
		return nil, fmt.Errorf("start task workflow: %w", err)
	}
	return run, nil
}

// QueryTaskStatus queries a running workflow for its current status.
// Returns "not_started" if the workflow has not registered a status
// handler yet, or the raw error from the client.
func QueryTaskStatus(ctx context.Context, c client.Client, workflowID, runID string) (TaskStatus, error) {
	resp, err := c.QueryWorkflow(ctx, workflowID, runID, "status")
	if err != nil {
		return "", fmt.Errorf("query status: %w", err)
	}
	var status TaskStatus
	if err := resp.Get(&status); err != nil {
		return "", fmt.Errorf("decode status: %w", err)
	}
	return status, nil
}

// QueryTaskResult queries a running workflow for its RunTaskResult.
// The result is only populated after the agent-execution activity has
// completed; before that, the workflow returns the zero value.
func QueryTaskResult(ctx context.Context, c client.Client, workflowID, runID string) (RunTaskResult, error) {
	resp, err := c.QueryWorkflow(ctx, workflowID, runID, "result")
	if err != nil {
		return RunTaskResult{}, fmt.Errorf("query result: %w", err)
	}
	var result RunTaskResult
	if err := resp.Get(&result); err != nil {
		return RunTaskResult{}, fmt.Errorf("decode result: %w", err)
	}
	return result, nil
}

// QueryStatusDetail returns the rich status snapshot.
func QueryStatusDetail(ctx context.Context, c client.Client, workflowID, runID string) (TaskWorkflowStatusDetail, error) {
	resp, err := c.QueryWorkflow(ctx, workflowID, runID, "status_detail")
	if err != nil {
		return TaskWorkflowStatusDetail{}, fmt.Errorf("query status_detail: %w", err)
	}
	var detail TaskWorkflowStatusDetail
	if err := resp.Get(&detail); err != nil {
		return TaskWorkflowStatusDetail{}, fmt.Errorf("decode status_detail: %w", err)
	}
	return detail, nil
}

// SignalCancel cancels a running workflow. The workflow's agent
// execution activity is given a chance to clean up; the workflow then
// transitions the task to TaskStatusCanceled.
func SignalCancel(ctx context.Context, c client.Client, workflowID, runID string) error {
	return c.SignalWorkflow(ctx, workflowID, runID, "cancel", nil)
}

// workflowID generates a stable workflow id from the task id so that
// duplicate StartTaskWorkflow calls for the same task are deduplicated
// by Temporal (WorkflowIdReusePolicy default).
func workflowID(taskID string) string {
	return "task-run/" + taskID
}

// temporalRetryPolicy builds a client-side retry policy from the
// workflow options. The workflow itself configures per-activity retry
// via workflow.ActivityOptions; this is the umbrella policy that bounds
// total retry duration.
func temporalRetryPolicy(opts WorkflowOptions) temporal.RetryPolicy {
	return temporal.RetryPolicy{
		InitialInterval:    opts.InitialRetryInterval,
		BackoffCoefficient: 2.0,
		MaximumInterval:    opts.MaxRetryInterval,
		MaximumAttempts:    int32(opts.MaxAgentExecutionAttempts),
	}
}

// --- HTTP handlers --------------------------------------------------------

// RegisterHTTP wires the task HTTP endpoints onto a mux. The endpoints
// mirror LobeHub's task router (`routers/lambda/task.ts`):
//
//   - POST /v1/tasks/run           — start a task workflow
//   - GET  /v1/tasks/{id}/status   — query the status of a task workflow
//   - GET  /v1/tasks/{id}/result   — query the result of a task workflow
//   - POST /v1/tasks/{id}/cancel   — cancel a running task workflow
//   - GET  /v1/tasks/counts        — debug endpoint with counts by status
func (w *Worker) RegisterHTTP(mux *http.ServeMux) {
	mux.HandleFunc("/v1/tasks/run", w.handleRunTask)
	mux.HandleFunc("/v1/tasks/counts", w.handleTaskCounts)
	mux.HandleFunc("/v1/tasks/", w.handleTaskByID)
}

func (w *Worker) handleRunTask(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if w.client == nil {
		http.Error(rw, "task: Temporal client not configured", http.StatusServiceUnavailable)
		return
	}
	var req RunTaskParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if req.TaskID == "" {
		http.Error(rw, "taskId is required", http.StatusBadRequest)
		return
	}

	// Pre-assign the topic + operation ids before the (async) workflow starts,
	// so the caller gets them back synchronously to attach a stream. The
	// executor inside the workflow uses these instead of minting its own. For
	// a continued topic the topic id is reused; only the operation id is fresh.
	req.TopicID = resolveTopicID("", req.ContinueTopicID)
	req.OperationID = newOperationID()

	run, err := StartTaskWorkflow(r.Context(), w.client, req, w.cfg.Options)
	if err != nil {
		http.Error(rw, fmt.Sprintf("start workflow: %v", err), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"taskId":      req.TaskID,
		"topicId":     req.TopicID,
		"operationId": req.OperationID,
		"workflowId":  run.GetID(),
		"runId":       run.GetRunID(),
		"status":      "started",
	})
}

// handleTaskByID dispatches /v1/tasks/{id}/{action} style requests.
// Path format:
//
//	GET  /v1/tasks/{id}/status   → status query
//	GET  /v1/tasks/{id}/result   → result query
//	POST /v1/tasks/{id}/cancel   → cancel signal
func (w *Worker) handleTaskByID(rw http.ResponseWriter, r *http.Request) {
	if w.client == nil {
		http.Error(rw, "task: Temporal client not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/")
	if len(parts) != 2 {
		http.Error(rw, "expected /v1/tasks/{id}/{action}", http.StatusBadRequest)
		return
	}
	taskID, action := parts[0], parts[1]
	wfID := workflowID(taskID)

	switch action {
	case "status":
		if r.Method != http.MethodGet {
			http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status, err := QueryTaskStatus(r.Context(), w.client, wfID, "")
		if err != nil || !status.Valid() {
			if w.cfg.Store != nil {
				if task, terr := w.cfg.Store.ResolveTask(r.Context(), taskID); terr == nil && task != nil && task.Status.Valid() {
					status = task.Status
					err = nil
				}
			}
		}
		if err != nil {
			http.Error(rw, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
			return
		}
		if !status.Valid() {
			status = TaskStatusPaused
		}
		writeJSON(rw, map[string]any{"taskId": taskID, "status": status})
	case "result":
		if r.Method != http.MethodGet {
			http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := QueryTaskResult(r.Context(), w.client, wfID, "")
		if err != nil {
			http.Error(rw, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, result)
	case "cancel":
		if r.Method != http.MethodPost {
			http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := SignalCancel(r.Context(), w.client, wfID, ""); err != nil {
			http.Error(rw, fmt.Sprintf("cancel: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(rw, map[string]any{"taskId": taskID, "status": "cancel_requested"})
	default:
		http.Error(rw, fmt.Sprintf("unknown action %q", action), http.StatusNotFound)
	}
}

func (w *Worker) handleTaskCounts(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counts, err := w.cfg.Store.GetTaskCountsByStatus(r.Context())
	if err != nil {
		http.Error(rw, fmt.Sprintf("counts: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, counts)
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

// NoopExecutor is the AgentExecutor used when none is configured. It
// returns an empty result with no error — useful for unit tests that
// only exercise the workflow shape.
type NoopExecutor struct{}

// Run implements AgentExecutor.
func (NoopExecutor) Run(_ context.Context, _ AgentRunParams, _ ProgressCallback) (*AgentRunResult, error) {
	return &AgentRunResult{
		AssistantContent: "",
	}, nil
}

// Interrupt implements AgentExecutor.
func (NoopExecutor) Interrupt(_ context.Context, _ string) error { return nil }
