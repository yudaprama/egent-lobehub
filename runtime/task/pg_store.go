package task

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is a TaskStore backed by PostgreSQL via pgx. It reads
// and writes the LobeHub `tasks`, `task_topics`, `task_dependencies`,
// and `agents` tables.
//
// All operations are idempotent at the row level — re-calling
// UpdateStatus(running → running) is a no-op. This matches the
// InMemoryStore behavior and satisfies the Temporal activity retry
// contract.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore creates a PostgresStore from an existing pgx pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// allowedStatusFields is the allowlist for UpdateStatus field patches.
// Prevents SQL injection via arbitrary column names.
var allowedStatusFields = map[string]bool{
	"started_at":          true,
	"completed_at":        true,
	"error":               true,
	"total_topics":        true,
	"consecutive_errors":  true,
	"schedule_started_at": true,
}

// ResolveTask implements TaskStore. Looks up by id first, then by
// identifier.
func (s *PostgresStore) ResolveTask(ctx context.Context, idOrIdentifier string) (*TaskItem, error) {
	const q = `
		SELECT id, identifier, user_id, workspace_id, name, instruction,
		       status, error, assignee_agent_id, parent_task_id,
		       current_topic_id, total_topics,
		       last_heartbeat_at, heartbeat_timeout, heartbeat_interval,
		       schedule_pattern, automation_mode, consecutive_errors,
		       schedule_started_at, config,
		       created_at, updated_at, started_at, completed_at
		FROM tasks
		WHERE id = $1 OR identifier = $1
		LIMIT 1`

	t := &TaskItem{}
	var configRaw []byte
	var workspaceID, name, instruction, err, assigneeAgentID, parentTaskID, currentTopicID *string
	var lastHeartbeatAt, startedAt, completedAt, scheduleStartedAt *time.Time
	var heartbeatTimeout, heartbeatInterval, totalTopics, consecutiveErrors *int
	var schedulePattern, automationMode *string

	err2 := s.pool.QueryRow(ctx, q, idOrIdentifier).Scan(
		&t.ID, &t.Identifier, &t.UserID, &workspaceID, &name, &instruction,
		&t.Status, &err, &assigneeAgentID, &parentTaskID,
		&currentTopicID, &totalTopics,
		&lastHeartbeatAt, &heartbeatTimeout, &heartbeatInterval,
		&schedulePattern, &automationMode, &consecutiveErrors,
		&scheduleStartedAt, &configRaw,
		&t.CreatedAt, &t.UpdatedAt, &startedAt, &completedAt,
	)
	if err2 != nil {
		if err2 == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve task: %w", err2)
	}

	if workspaceID != nil {
		t.WorkspaceID = *workspaceID
	}
	if name != nil {
		t.Name = *name
	}
	if instruction != nil {
		t.Instruction = *instruction
	}
	if err != nil {
		t.Error = *err
	}
	if assigneeAgentID != nil {
		t.AssigneeAgentID = *assigneeAgentID
	}
	if parentTaskID != nil {
		t.ParentTaskID = *parentTaskID
	}
	if currentTopicID != nil {
		t.CurrentTopicID = *currentTopicID
	}
	if totalTopics != nil {
		t.TotalTopics = *totalTopics
	}
	if lastHeartbeatAt != nil {
		t.LastHeartbeatAt = lastHeartbeatAt
	}
	if heartbeatTimeout != nil {
		t.HeartbeatTimeout = *heartbeatTimeout
	}
	if heartbeatInterval != nil {
		t.HeartbeatInterval = *heartbeatInterval
	}
	if schedulePattern != nil {
		t.SchedulePattern = *schedulePattern
	}
	if automationMode != nil {
		t.AutomationMode = *automationMode
	}
	if consecutiveErrors != nil {
		t.ConsecutiveErrors = *consecutiveErrors
	}
	if scheduleStartedAt != nil {
		t.ScheduleStartedAt = scheduleStartedAt
	}
	if startedAt != nil {
		t.StartedAt = startedAt
	}
	if completedAt != nil {
		t.CompletedAt = completedAt
	}
	if len(configRaw) > 0 {
		cfg, parseErr := ParseTaskConfig(configRaw)
		if parseErr == nil {
			t.Config = cfg
		}
	}

	return t, nil
}

// UpdateStatus implements TaskStore.
func (s *PostgresStore) UpdateStatus(ctx context.Context, taskID string, status TaskStatus, fields ...StatusField) error {
	setClauses := []string{"status = $2", "updated_at = now()"}
	args := []any{taskID, status}
	argIdx := 3

	for _, f := range fields {
		if !allowedStatusFields[f.Name] {
			return fmt.Errorf("UpdateStatus: unknown field %q", f.Name)
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", f.Name, argIdx))
		args = append(args, f.Value)
		argIdx++
	}

	q := fmt.Sprintf("UPDATE tasks SET %s WHERE id = $1", strings.Join(setClauses, ", "))
	_, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// UpdateTaskConfig implements TaskStore.
func (s *PostgresStore) UpdateTaskConfig(ctx context.Context, taskID string, config json.RawMessage) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE tasks SET config = $2, updated_at = now() WHERE id = $1",
		taskID, config)
	if err != nil {
		return fmt.Errorf("update task config: %w", err)
	}
	return nil
}

// IncrementTopicCount implements TaskStore.
func (s *PostgresStore) IncrementTopicCount(ctx context.Context, taskID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		"UPDATE tasks SET total_topics = total_topics + 1, updated_at = now() WHERE id = $1 RETURNING total_topics",
		taskID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("increment topic count: %w", err)
	}
	return count, nil
}

// SetCurrentTopic implements TaskStore.
func (s *PostgresStore) SetCurrentTopic(ctx context.Context, taskID string, topicID string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE tasks SET current_topic_id = $2, updated_at = now() WHERE id = $1",
		taskID, topicID)
	return err
}

// AddTaskTopic implements TaskStore.
func (s *PostgresStore) AddTaskTopic(ctx context.Context, taskID string, topicID string, operationID string, seq int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_topics (task_id, topic_id, operation_id, seq, status)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT DO NOTHING`,
		taskID, topicID, operationID, seq, string(TopicStatusRunning))
	return err
}

// UpdateTaskTopicStatus implements TaskStore.
func (s *PostgresStore) UpdateTaskTopicStatus(ctx context.Context, taskID string, topicID string, status TopicStatus) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE task_topics SET status = $3 WHERE task_id = $1 AND topic_id = $2",
		taskID, topicID, string(status))
	return err
}

// UpdateTaskTopicOperationId implements TaskStore.
func (s *PostgresStore) UpdateTaskTopicOperationId(ctx context.Context, taskID string, topicID string, operationID string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE task_topics SET operation_id = $3 WHERE task_id = $1 AND topic_id = $2",
		taskID, topicID, operationID)
	return err
}

// UpdateHeartbeat implements TaskStore.
func (s *PostgresStore) UpdateHeartbeat(ctx context.Context, taskID string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE tasks SET last_heartbeat_at = now() WHERE id = $1",
		taskID)
	return err
}

// ListRunningTopics implements TaskStore.
func (s *PostgresStore) ListRunningTopics(ctx context.Context, taskID string) ([]TaskTopic, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT task_id, topic_id, status, COALESCE(operation_id, ''), COALESCE(seq, 0)
		 FROM task_topics
		 WHERE task_id = $1 AND status = $2
		 ORDER BY seq`,
		taskID, string(TopicStatusRunning))
	if err != nil {
		return nil, fmt.Errorf("list running topics: %w", err)
	}
	defer rows.Close()

	var topics []TaskTopic
	for rows.Next() {
		var t TaskTopic
		if err := rows.Scan(&t.TaskID, &t.TopicID, &t.Status, &t.OperationID, &t.Seq); err != nil {
			return nil, fmt.Errorf("scan topic: %w", err)
		}
		topics = append(topics, t)
	}
	return topics, rows.Err()
}

// TimeoutRunningTopics implements TaskStore.
func (s *PostgresStore) TimeoutRunningTopics(ctx context.Context, taskID string, heartbeatTimeout time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE task_topics
		 SET status = $3
		 WHERE task_id = $1
		   AND status = $2
		   AND last_heartbeat_at < now() - $4::interval`,
		taskID, string(TopicStatusRunning), string(TopicStatusFailed),
		fmt.Sprintf("%d seconds", int(heartbeatTimeout.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("timeout running topics: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// GetUnlockedTasks implements TaskStore. Returns downstream tasks whose
// 'blocks' dependencies are all in terminal completed state.
func (s *PostgresStore) GetUnlockedTasks(ctx context.Context, completedTaskID string) ([]*TaskItem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT d.task_id
		 FROM task_dependencies d
		 WHERE d.depends_on_id = $1
		   AND d.type = 'blocks'
		   AND NOT EXISTS (
		     SELECT 1 FROM task_dependencies d2
		     JOIN tasks t ON t.id = d2.depends_on_id
		     WHERE d2.task_id = d.task_id
		       AND d2.type = 'blocks'
		       AND t.status NOT IN ('completed', 'canceled')
		   )`,
		completedTaskID)
	if err != nil {
		return nil, fmt.Errorf("get unlocked tasks: %w", err)
	}
	defer rows.Close()

	var items []*TaskItem
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		item, err := s.ResolveTask(ctx, taskID)
		if err != nil {
			slog.Warn("resolve unlocked task failed", "task_id", taskID, "error", err)
			continue
		}
		if item != nil {
			items = append(items, item)
		}
	}
	return items, rows.Err()
}

// GetCheckpointConfig implements TaskStore.
func (s *PostgresStore) GetCheckpointConfig(ctx context.Context, task *TaskItem) (CheckpointConfig, bool, error) {
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
func (s *PostgresStore) GetReviewConfig(_ context.Context, task *TaskItem) (ReviewConfig, bool, error) {
	if task.Config == nil || task.Config.Review == nil {
		return ReviewConfig{}, false, nil
	}
	return *task.Config.Review, true, nil
}

// GetInboxAgentID implements TaskStore. Queries the agents table for
// the inbox session's builtin agent.
func (s *PostgresStore) GetInboxAgentID(ctx context.Context) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM agents WHERE slug = 'inbox' OR "group" = 'inbox' LIMIT 1`).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "agt_inbox", nil
		}
		return "", fmt.Errorf("get inbox agent: %w", err)
	}
	return id, nil
}

// GetAgentModelConfig implements TaskStore.
func (s *PostgresStore) GetAgentModelConfig(ctx context.Context, agentID string) (*ModelConfig, error) {
	var model, provider string
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(model, ''), COALESCE(provider, '')
		 FROM agents WHERE id = $1`, agentID).Scan(&model, &provider)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get agent model config: %w", err)
	}
	if model == "" && provider == "" {
		return nil, nil
	}
	return &ModelConfig{Model: model, Provider: provider}, nil
}

// GetTaskCountsByStatus implements TaskStore.
func (s *PostgresStore) GetTaskCountsByStatus(ctx context.Context) (map[TaskStatus]int, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT status, COUNT(*)::int FROM tasks GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("get task counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[TaskStatus]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[TaskStatus(status)] = count
	}
	return counts, rows.Err()
}

// Ensure interface compliance.
var _ TaskStore = (*PostgresStore)(nil)
