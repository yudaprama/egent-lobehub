package task

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func strptr(s string) *string { return &s }

// TestTopicAgentID covers the FK-safety guard: only an agt_-prefixed assignee
// becomes the topic's agent_id; slugs and nil yield NULL.
func TestTopicAgentID(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want *string
	}{
		{"nil", nil, nil},
		{"slug-inbox", strptr("inbox"), nil},
		{"empty", strptr(""), nil},
		{"real-id", strptr("agt_abc123"), strptr("agt_abc123")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := topicAgentID(c.in)
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("want nil, got %q", *got)
			case c.want != nil && (got == nil || *got != *c.want):
				t.Fatalf("want %q, got %v", *c.want, got)
			}
		})
	}
}

// TestPostgresStore_AddTaskTopic_Integration verifies that AddTaskTopic creates
// the topics row (FK-satisfying) AND the task_topics link with a non-null
// user_id — the two defects that made the path fail before. Skipped unless
// TASK_TEST_DSN points at a database with the lobehub task/topic schema.
func TestPostgresStore_AddTaskTopic_Integration(t *testing.T) {
	dsn := os.Getenv("TASK_TEST_DSN")
	if dsn == "" {
		t.Skip("set TASK_TEST_DSN to run the AddTaskTopic integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	const (
		uid     = "task-it-user"
		taskID  = "task_it_1"
		topicID = "tpc_it000000001"
		opID    = "op-it-1"
	)
	// Cleanup first + last — deleting the user cascades to tasks/topics/task_topics.
	cleanup := func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid) }
	cleanup()
	t.Cleanup(cleanup)

	if _, err := pool.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tasks (id, identifier, seq, created_by_user_id, instruction, status, name, assignee_agent_id)
		 VALUES ($1, $2, 1, $3, 'do the thing', 'running', 'IT Task', 'inbox')`,
		taskID, taskID, uid,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	store := NewPostgresStore(pool)
	if err := store.AddTaskTopic(ctx, taskID, topicID, opID, 1); err != nil {
		t.Fatalf("AddTaskTopic: %v", err)
	}

	// The topics row exists with the right owner.
	var topicUser string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM topics WHERE id = $1`, topicID).Scan(&topicUser); err != nil {
		t.Fatalf("topic row missing: %v", err)
	}
	if topicUser != uid {
		t.Fatalf("topic user_id = %q, want %q", topicUser, uid)
	}

	// The join row exists with a non-null user_id + the operation id.
	var joinUser, joinOp string
	if err := pool.QueryRow(ctx,
		`SELECT user_id, operation_id FROM task_topics WHERE task_id = $1 AND topic_id = $2`,
		taskID, topicID,
	).Scan(&joinUser, &joinOp); err != nil {
		t.Fatalf("task_topics row missing: %v", err)
	}
	if joinUser != uid || joinOp != opID {
		t.Fatalf("task_topics row = (user %q, op %q), want (%q, %q)", joinUser, joinOp, uid, opID)
	}

	// Idempotent: a second call (same ids) must not error.
	if err := store.AddTaskTopic(ctx, taskID, topicID, opID, 1); err != nil {
		t.Fatalf("AddTaskTopic (idempotent retry): %v", err)
	}
}
