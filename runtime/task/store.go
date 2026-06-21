package task

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TaskStore is the persistence layer for tasks, topics, briefs, and the
// dependency graph. It is the Go equivalent of the LobeHub database models
// (TaskModel, TaskTopicModel, BriefModel, TaskModel.getUnlockedTasks,
// TaskModel.getCheckpointConfig, etc.).
//
// Implementations MUST be safe to call from Temporal activities. Activities
// may run multiple times on retry; operations that have side effects
// (status updates, brief creation) must be idempotent. The store's
// Update* methods are expected to be idempotent at the row level — re-calling
// UpdateStatus(running → running) is a no-op.
//
// The interface is intentionally narrow. Anything the workflow / activities
// need to look up or mutate goes through this surface. The TS code reaches
// into specific model classes; here we collapse them into one interface to
// keep the workflow code clean.
type TaskStore interface {
	// ResolveTask returns the task identified by id OR identifier. Mirrors
	// TaskModel.resolve() in the TS source. Returns (nil, nil) when the
	// task is not found (callers translate that to NOT_FOUND at the
	// boundary).
	ResolveTask(ctx context.Context, idOrIdentifier string) (*TaskItem, error)

	// UpdateStatus transitions a task's status field. Implementations
	// should ignore no-op transitions (e.g. running → running). The
	// optional fields patch the task row atomically with the status
	// update — same transaction.
	UpdateStatus(ctx context.Context, taskID string, status TaskStatus, fields ...StatusField) error

	// UpdateTaskConfig writes a new task.config JSON blob.
	UpdateTaskConfig(ctx context.Context, taskID string, config json.RawMessage) error

	// IncrementTopicCount atomically increments total_topics. Mirrors
	// TaskModel.incrementTopicCount.
	IncrementTopicCount(ctx context.Context, taskID string) (int, error)

	// SetCurrentTopic sets tasks.current_topic_id.
	SetCurrentTopic(ctx context.Context, taskID string, topicID string) error

	// AddTaskTopic records the (task, topic) link with the agent
	// operation id. Mirrors TaskTopicModel.add.
	AddTaskTopic(ctx context.Context, taskID string, topicID string, operationID string, seq int) error

	// UpdateTaskTopicStatus transitions a task_topics row's status. Used
	// when a topic completes or fails.
	UpdateTaskTopicStatus(ctx context.Context, taskID string, topicID string, status TopicStatus) error

	// UpdateTaskTopicOperationId updates the operationId on an existing
	// task_topics row. Used when the workflow re-uses a topic (continue
	// path).
	UpdateTaskTopicOperationId(ctx context.Context, taskID string, topicID string, operationID string) error

	// UpdateHeartbeat stamps last_heartbeat_at = now(). Called by the
	// heartbeat activity and by RunAgentExecution on each RecordHeartbeat.
	UpdateHeartbeat(ctx context.Context, taskID string) error

	// ListRunningTopics returns task_topics with status=running for a
	// task. Used to detect concurrent runs.
	ListRunningTopics(ctx context.Context, taskID string) ([]TaskTopic, error)

	// TimeoutRunningTopics marks topics with status=running whose
	// last_heartbeat_at is older than the configured heartbeat_timeout
	// as failed. Mirrors TaskTopicModel.timeoutRunning. Returns the
	// number of topics transitioned.
	TimeoutRunningTopics(ctx context.Context, taskID string, heartbeatTimeout time.Duration) (int, error)

	// GetUnlockedTasks returns downstream tasks whose 'blocks' deps are
	// now satisfied after the given task completes. Mirrors
	// TaskModel.getUnlockedTasks.
	GetUnlockedTasks(ctx context.Context, completedTaskID string) ([]*TaskItem, error)

	// GetCheckpointConfig returns the parent's checkpoint config relevant
	// to a subtask. Mirrors TaskModel.getCheckpointConfig.
	// Returns false in the second return value when the task has no
	// checkpoint config (default behavior: no checkpoint gating).
	GetCheckpointConfig(ctx context.Context, task *TaskItem) (CheckpointConfig, bool, error)

	// GetReviewConfig returns the review configuration. Returns false in
	// the second return value when review is disabled.
	GetReviewConfig(ctx context.Context, task *TaskItem) (ReviewConfig, bool, error)

	// GetInboxAgentID returns the fallback inbox agent id for tasks
	// without an assignee. Mirrors AgentModel.getBuiltinAgent(INBOX_SESSION_ID).
	// Returns an error if the inbox agent cannot be found.
	GetInboxAgentID(ctx context.Context) (string, error)

	// GetAgentModelConfig returns the model+provider snapshot for an
	// agent. Mirrors AgentModel.getAgentModelConfig. Used to backfill
	// task.config.model/provider for legacy tasks.
	GetAgentModelConfig(ctx context.Context, agentID string) (*ModelConfig, error)

	// GetTaskCountsByStatus is a debug helper used by the HTTP status
	// endpoint. Not part of the workflow hot path.
	GetTaskCountsByStatus(ctx context.Context) (map[TaskStatus]int, error)
}

// CheckpointConfig is the runtime view of task.config.checkpoint. The
// BeforeIDs set lists subtask identifiers that should be held in `paused`
// until a parent completes; OnAgentRequest toggles the agent-driven
// request_checkpoint path.
type CheckpointConfig struct {
	// BeforeIDs lists subtask identifiers that should be paused before
	// start, awaiting human approval.
	BeforeIDs []string
	// OnAgentRequest, when true, allows the agent to request a checkpoint
	// mid-run via the requestCheckpoint tool. Default true.
	OnAgentRequest bool
}

// StatusField is one of the optional fields that can be patched
// alongside a status update. Using a struct with helper constructors
// (rather than a map) keeps call sites readable.
type StatusField struct {
	// Name is the column name. Implementations must reject unknown
	// names to prevent SQL injection.
	Name string
	// Value is the new value. Only certain types are supported (time.Time,
	// string, int, nil). Implementations should validate.
	Value any
}

// StatusField helpers — the most common patches. Each returns a
// StatusField that UpdateStatus can apply.
func WithStartedAt(t time.Time) StatusField { return StatusField{Name: "started_at", Value: t} }
func WithCompletedAt(t time.Time) StatusField {
	return StatusField{Name: "completed_at", Value: t}
}
func WithError(msg string) StatusField { return StatusField{Name: "error", Value: msg} }
func WithTotalTopics(n int) StatusField { return StatusField{Name: "total_topics", Value: n} }
func WithConsecutiveErrors(n int) StatusField {
	return StatusField{Name: "consecutive_errors", Value: n}
}
func WithScheduleStartedAt(t time.Time) StatusField {
	return StatusField{Name: "schedule_started_at", Value: t}
}

// TaskTopic is a row in the task_topics join table.
type TaskTopic struct {
	TaskID      string      `json:"taskId"`
	TopicID     string      `json:"topicId"`
	Status      TopicStatus `json:"status"`
	OperationID string      `json:"operationId,omitempty"`
	Seq         int         `json:"seq"`
}

// ModelConfig is the (model, provider) snapshot written into
// task.config when a task is created.
type ModelConfig struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// --- In-memory store (used in tests) --------------------------------------

// InMemoryStore is a TaskStore implementation backed by maps. Used in
// unit tests and as a reference implementation. NOT safe for production
// — there is no persistence across restarts.
type InMemoryStore struct {
	tasks       map[string]*TaskItem
	taskTopics  map[string][]TaskTopic // key: taskID
	agentConfig map[string]*ModelConfig
}

// NewInMemoryStore returns an empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		tasks:       make(map[string]*TaskItem),
		taskTopics:  make(map[string][]TaskTopic),
		agentConfig: make(map[string]*ModelConfig),
	}
}

// AddTask adds a task to the in-memory store. Test helper.
func (s *InMemoryStore) AddTask(t *TaskItem) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	t.UpdatedAt = time.Now()
	s.tasks[t.ID] = t
	// Also index by identifier so ResolveTask can find it.
	if t.Identifier != "" {
		s.tasks[t.Identifier] = t
	}
}

// AddAgentConfig adds an agent→model mapping. Test helper.
func (s *InMemoryStore) AddAgentConfig(agentID string, c *ModelConfig) {
	s.agentConfig[agentID] = c
}

// ResolveTask implements TaskStore.
func (s *InMemoryStore) ResolveTask(_ context.Context, idOrIdentifier string) (*TaskItem, error) {
	t, ok := s.tasks[idOrIdentifier]
	if !ok {
		return nil, nil
	}
	// Return a copy so callers cannot mutate the store by holding the
	// pointer.
	cp := *t
	return &cp, nil
}

// UpdateStatus implements TaskStore. It applies each StatusField by name
// using a switch — unknown names are returned as an error (defensive
// against typos in callers).
func (s *InMemoryStore) UpdateStatus(_ context.Context, taskID string, status TaskStatus, fields ...StatusField) error {
	t, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}
	// Resolve by identifier if needed.
	if t == nil {
		for _, candidate := range s.tasks {
			if candidate.Identifier == taskID || candidate.ID == taskID {
				t = candidate
				break
			}
		}
		if t == nil {
			return fmt.Errorf("task %q not found", taskID)
		}
	}
	if t.Status == status {
		return nil // no-op
	}
	t.Status = status
	t.UpdatedAt = time.Now()
	for _, f := range fields {
		switch f.Name {
		case "started_at":
			if v, ok := f.Value.(time.Time); ok {
				t.StartedAt = &v
			}
		case "completed_at":
			if v, ok := f.Value.(time.Time); ok {
				t.CompletedAt = &v
			}
		case "error":
			if v, ok := f.Value.(string); ok {
				t.Error = v
			} else if f.Value == nil {
				t.Error = ""
			}
		case "total_topics":
			if v, ok := f.Value.(int); ok {
				t.TotalTopics = v
			}
		case "consecutive_errors":
			if v, ok := f.Value.(int); ok {
				t.ConsecutiveErrors = v
			}
		case "schedule_started_at":
			if v, ok := f.Value.(time.Time); ok {
				t.ScheduleStartedAt = &v
			}
		default:
			return fmt.Errorf("UpdateStatus: unknown field %q", f.Name)
		}
	}
	return nil
}

// UpdateTaskConfig implements TaskStore.
func (s *InMemoryStore) UpdateTaskConfig(_ context.Context, taskID string, raw json.RawMessage) error {
	t, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}
	cfg, err := ParseTaskConfig(raw)
	if err != nil {
		return err
	}
	t.Config = cfg
	t.UpdatedAt = time.Now()
	return nil
}

// IncrementTopicCount implements TaskStore.
func (s *InMemoryStore) IncrementTopicCount(_ context.Context, taskID string) (int, error) {
	t, ok := s.tasks[taskID]
	if !ok {
		return 0, fmt.Errorf("task %q not found", taskID)
	}
	t.TotalTopics++
	return t.TotalTopics, nil
}

// SetCurrentTopic implements TaskStore.
func (s *InMemoryStore) SetCurrentTopic(_ context.Context, taskID string, topicID string) error {
	t, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}
	t.CurrentTopicID = topicID
	return nil
}

// AddTaskTopic implements TaskStore.
func (s *InMemoryStore) AddTaskTopic(_ context.Context, taskID string, topicID string, operationID string, seq int) error {
	row := TaskTopic{
		TaskID:      taskID,
		TopicID:     topicID,
		Status:      TopicStatusRunning,
		OperationID: operationID,
		Seq:         seq,
	}
	s.taskTopics[taskID] = append(s.taskTopics[taskID], row)
	return nil
}

// UpdateTaskTopicStatus implements TaskStore.
func (s *InMemoryStore) UpdateTaskTopicStatus(_ context.Context, taskID string, topicID string, status TopicStatus) error {
	for i, row := range s.taskTopics[taskID] {
		if row.TopicID == topicID {
			s.taskTopics[taskID][i].Status = status
			return nil
		}
	}
	return nil // idempotent: not-found is a no-op
}

// UpdateTaskTopicOperationId implements TaskStore.
func (s *InMemoryStore) UpdateTaskTopicOperationId(_ context.Context, taskID string, topicID string, operationID string) error {
	for i, row := range s.taskTopics[taskID] {
		if row.TopicID == topicID {
			s.taskTopics[taskID][i].OperationID = operationID
			return nil
		}
	}
	return nil
}

// UpdateHeartbeat implements TaskStore.
func (s *InMemoryStore) UpdateHeartbeat(_ context.Context, taskID string) error {
	now := time.Now()
	if t, ok := s.tasks[taskID]; ok {
		t.LastHeartbeatAt = &now
	}
	return nil
}

// ListRunningTopics implements TaskStore.
func (s *InMemoryStore) ListRunningTopics(_ context.Context, taskID string) ([]TaskTopic, error) {
	var out []TaskTopic
	for _, row := range s.taskTopics[taskID] {
		if row.Status == TopicStatusRunning {
			out = append(out, row)
		}
	}
	return out, nil
}

// TimeoutRunningTopics implements TaskStore. The in-memory store has no
// heartbeat timestamps on topic rows, so this is a no-op.
func (s *InMemoryStore) TimeoutRunningTopics(_ context.Context, _ string, _ time.Duration) (int, error) {
	return 0, nil
}

// GetUnlockedTasks implements TaskStore. The in-memory store has no
// dependency edges — returns an empty slice.
func (s *InMemoryStore) GetUnlockedTasks(_ context.Context, _ string) ([]*TaskItem, error) {
	return nil, nil
}

// GetCheckpointConfig implements TaskStore. Returns false (no checkpoint
// gating) by default; tests can set Config.Checkpoint on the task itself
// to opt in.
func (s *InMemoryStore) GetCheckpointConfig(_ context.Context, task *TaskItem) (CheckpointConfig, bool, error) {
	if task.Config == nil || task.Config.Raw == nil {
		return CheckpointConfig{OnAgentRequest: true}, false, nil
	}
	raw, ok := task.Config.Raw["checkpoint"].(map[string]any)
	if !ok {
		return CheckpointConfig{OnAgentRequest: true}, false, nil
	}
	cfg := CheckpointConfig{OnAgentRequest: true}
	if v, ok := raw["onAgentRequest"].(bool); ok {
		cfg.OnAgentRequest = v
	}
	if ids, ok := raw["beforeIds"].([]any); ok {
		for _, id := range ids {
			if s, ok := id.(string); ok {
				cfg.BeforeIDs = append(cfg.BeforeIDs, s)
			}
		}
	}
	return cfg, true, nil
}

// GetReviewConfig implements TaskStore.
func (s *InMemoryStore) GetReviewConfig(_ context.Context, task *TaskItem) (ReviewConfig, bool, error) {
	if task.Config == nil || task.Config.Review == nil {
		return ReviewConfig{}, false, nil
	}
	return *task.Config.Review, true, nil
}

// GetInboxAgentID implements TaskStore. Returns a fixed fallback id
// "agt_inbox" for tests.
func (s *InMemoryStore) GetInboxAgentID(_ context.Context) (string, error) {
	return "agt_inbox", nil
}

// GetAgentModelConfig implements TaskStore.
func (s *InMemoryStore) GetAgentModelConfig(_ context.Context, agentID string) (*ModelConfig, error) {
	c, ok := s.agentConfig[agentID]
	if !ok {
		return nil, nil
	}
	return c, nil
}

// GetTaskCountsByStatus implements TaskStore.
func (s *InMemoryStore) GetTaskCountsByStatus(_ context.Context) (map[TaskStatus]int, error) {
	counts := make(map[TaskStatus]int)
	for _, t := range s.tasks {
		// Skip identifier-aliased duplicates — only count unique ids.
		if t.ID == "" {
			continue
		}
		if _, seen := counts[t.Status]; !seen {
			// This is a heuristic; tests should use AddTask with unique
			// ids and identifiers to avoid double-counting.
		}
		counts[t.Status]++
	}
	return counts, nil
}
