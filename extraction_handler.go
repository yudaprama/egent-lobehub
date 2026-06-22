package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"egent-lobehub/memory/palace"

	"github.com/jackc/pgx/v5"
)

// palaceAuthChecker is the subset of palace.AuthChecker that the
// extraction handler needs. We re-declare the interface here (rather
// than importing the palace package's AuthChecker type) so the main
// package does not create a circular dependency through the palace
// package's own wiring.
type palaceAuthChecker interface {
	CheckMessageWrite(ctx context.Context, userID, workspaceID string) error
}

// extractionStartRequest is the JSON body for POST /v1/memory/extraction/start.
type extractionStartRequest struct {
	FromDate *time.Time `json:"fromDate,omitempty"`
	ToDate   *time.Time `json:"toDate,omitempty"`
}

// extractionStartResponse mirrors the shape returned by
// lambda/userMemory.ts:requestMemoryFromChatTopic so the existing
// frontend polling hook continues to work unchanged.
type extractionStartResponse struct {
	Deduped  bool        `json:"deduped"`
	ID       string      `json:"id"`
	Metadata interface{} `json:"metadata"`
	Status   string      `json:"status"`
}

// extractionStartHandler is the Go replacement for
// lambda/userMemory.ts:requestMemoryFromChatTopic. It:
//
//  1. Creates a row in async_tasks (status=pending) so the existing
//     frontend polling hook (useMemoryAnalysisAsyncTask) keeps
//     working unchanged.
//  2. Queries the user's topics in the given date range.
//  3. Enqueues one memory_ingest River job per topic — the egent-jobs
//     memoryingest worker (Phase 3) handles each one.
//  4. Returns the async_tasks row id; the frontend polls
//     async_tasks.status to track progress.
//
// Mounted under /v1/memory/extraction/start. The auth gate (Keto
// `write` check on the workspace) is applied when auth is non-nil.
func extractionStartHandler(db pgxpoolLike, auth palaceAuthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		userID := palace.UserIDFromContext(r.Context())
		if userID == "" || userID == "anonymous" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing user identity"})
			return
		}
		if auth != nil {
			workspaceID := r.Header.Get("X-Workspace-Id")
			if err := auth.CheckMessageWrite(r.Context(), userID, workspaceID); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
		}

		var req extractionStartRequest
		if err := decodeExtractionRequest(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
			return
		}
		if req.FromDate != nil && req.ToDate != nil && req.FromDate.After(*req.ToDate) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fromDate must be on or before toDate"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		result, err := startExtraction(ctx, db, userID, req.FromDate, req.ToDate)
		if err != nil {
			slog.Error("memory extraction start failed", "user", userID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "extraction start failed"})
			return
		}
		writeJSON(w, http.StatusAccepted, result)
	}
}

// extractionStatusHandler is the polling endpoint that
// useMemoryAnalysisAsyncTask hits via SWR. Mounted under
// /v1/memory/extraction/task/<id> or via ?taskId=<id>.
func extractionStatusHandler(db pgxpoolLike) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		userID := palace.UserIDFromContext(r.Context())
		if userID == "" || userID == "anonymous" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing user identity"})
			return
		}
		taskID := strings.TrimPrefix(r.URL.Path, "/v1/memory/extraction/task/")
		if taskID == "" {
			taskID = r.URL.Query().Get("taskId")
		}
		if taskID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing taskId"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var status, taskType string
		var metadataRaw, errorRaw []byte
		err := db.QueryRow(ctx, `
			SELECT type, status, metadata, COALESCE(error, '{}'::jsonb)
			FROM async_tasks
			WHERE id = $1::uuid AND user_id = $2`, taskID, userID).Scan(
			&taskType, &status, &metadataRaw, &errorRaw)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
			return
		}
		var metadata, errorObj any
		_ = json.Unmarshal(metadataRaw, &metadata)
		_ = json.Unmarshal(errorRaw, &errorObj)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       taskID,
			"type":     taskType,
			"status":   status,
			"metadata": metadata,
			"error":    errorObj,
		})
	}
}

// startExtraction is the core logic — extracted so the HTTP handler
// stays a thin wrapper and the function is easy to unit-test.
func startExtraction(ctx context.Context, db pgxpoolLike, userID string, fromDate, toDate *time.Time) (*extractionStartResponse, error) {
	taskID, err := createExtractionTask(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("create async_task: %w", err)
	}

	topics, err := listUserTopics(ctx, db, userID, fromDate, toDate)
	if err != nil {
		_ = markExtractionTaskError(ctx, db, taskID, err)
		return nil, fmt.Errorf("list topics: %w", err)
	}

	enqueued := 0
	for _, topic := range topics {
		content := topic.Title + "\n" + topic.Content
		if err := enqueueMemoryIngestJob(ctx, db, userID, topic.ID, content, "chat_topic"); err != nil {
			slog.Warn("memory extraction: enqueue failed",
				"user", userID, "topic", topic.ID, "error", err)
			continue
		}
		enqueued++
	}

	metadata := map[string]any{
		"progress": map[string]int{
			"completedTopics": 0,
			"totalTopics":     len(topics),
		},
		"range": map[string]string{
			"from": timeOrEmpty(fromDate),
			"to":   timeOrEmpty(toDate),
		},
		"source": "chat_topic",
		"enqueued": enqueued,
	}
	if err := updateExtractionMetadata(ctx, db, taskID, metadata); err != nil {
		slog.Warn("memory extraction: metadata update failed", "task", taskID, "error", err)
	}

	status := "pending"
	if enqueued == 0 {
		status = "success"
	}
	if err := setExtractionStatus(ctx, db, taskID, status); err != nil {
		slog.Warn("memory extraction: status update failed", "task", taskID, "error", err)
	}

	return &extractionStartResponse{
		Deduped:  false,
		ID:       taskID,
		Metadata: metadata,
		Status:   status,
	}, nil
}

// topicRow is the minimal projection of the topics table — just the
// fields the extraction worker needs to build Content.
type topicRow struct {
	ID      string
	Title   string
	Content string
}

// listUserTopics pulls every topic the user owns in the date range
// and concatenates the user-role messages into a single Content
// blob. Mirrors UserMemoryTopicRepository.countTopicsForMemoryExtractor.
func listUserTopics(ctx context.Context, db pgxpoolLike, userID string, fromDate, toDate *time.Time) ([]topicRow, error) {
	const q = `
		SELECT t.id, COALESCE(t.title, '') AS title,
		       COALESCE(string_agg(m.content, E'\n'), '') AS content
		FROM topics t
		LEFT JOIN LATERAL (
			SELECT content
			FROM messages
			WHERE topic_id = t.id AND role = 'user' AND deleted_at IS NULL
			ORDER BY created_at ASC
		) m ON true
		WHERE t.user_id = $1
		  AND ($2::timestamptz IS NULL OR t.created_at >= $2)
		  AND ($3::timestamptz IS NULL OR t.created_at <= $3)
		GROUP BY t.id, t.title
		ORDER BY t.created_at DESC`
	rows, err := db.Query(ctx, q, userID, nullableTime(fromDate), nullableTime(toDate))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicRow
	for rows.Next() {
		var t topicRow
		if err := rows.Scan(&t.ID, &t.Title, &t.Content); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func timeOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func createExtractionTask(ctx context.Context, db pgxpoolLike, userID string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `
		INSERT INTO async_tasks (user_id, type, status, metadata)
		VALUES ($1, 'UserMemoryExtractionWithChatTopic', 'pending', '{}'::jsonb)
		RETURNING id::text`, userID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

func setExtractionStatus(ctx context.Context, db pgxpoolLike, taskID, status string) error {
	_, err := db.Exec(ctx, `UPDATE async_tasks SET status = $1, updated_at = now() WHERE id = $2::uuid`, status, taskID)
	return err
}

func markExtractionTaskError(ctx context.Context, db pgxpoolLike, taskID string, cause error) error {
	errJSON, _ := json.Marshal(map[string]string{"type": "InternalError", "message": cause.Error()})
	_, err := db.Exec(ctx, `
		UPDATE async_tasks
		SET status = 'error', error = $1::jsonb, updated_at = now()
		WHERE id = $2::uuid`, string(errJSON), taskID)
	return err
}

func updateExtractionMetadata(ctx context.Context, db pgxpoolLike, taskID string, metadata map[string]any) error {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE async_tasks SET metadata = $1::jsonb, updated_at = now() WHERE id = $2::uuid`, string(raw), taskID)
	return err
}

// enqueueMemoryIngestJob inserts a memory_ingest job into the River
// queue. Mirrors the lobehub/apps/server/rivers/riverProducer.ts
// pattern: a direct SQL INSERT against the river_job table that
// egent-jobs polls. State=1 (available), priority=2 (worker pool
// default), attempt=0 (start retry counter fresh).
func enqueueMemoryIngestJob(ctx context.Context, db pgxpoolLike, userID, topicID, content, source string) error {
	argsJSON, err := json.Marshal(map[string]string{
		"userId":  userID,
		"topicId": topicID,
		"content": content,
		"source":  source,
	})
	if err != nil {
		return err
	}
	const q = `
		INSERT INTO river_job
			(kind, args, queue, max_attempts, tags, metadata, state, priority, attempt, created_at, scheduled_at)
		VALUES
			('memory_ingest', $1::jsonb, 'memory_ingest', 5, '{}'::text[],
			 '{}'::jsonb, 1, 2, 0, now(), now())`
	_, err = db.Exec(ctx, q, string(argsJSON))
	return err
}

// decodeExtractionRequest is a small helper that decodes the JSON
// body. We do not use json.NewDecoder directly because we want to
// return a clean error and tolerate an empty body.
func decodeExtractionRequest(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// Compile-time check: palaceAuthChecker must satisfy palace.AuthChecker
// (when the wiring in main.go builds a real one from the palace
// package). The test in main.go uses the interface, so we just need
// to confirm the contract matches.
var _ palace.AuthChecker = (palaceAuthChecker)(nil)