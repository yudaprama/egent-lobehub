package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"egent-lobehub/memory/palace"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgxqueryRowMock satisfies pgxconn + pgxquery so we can exercise
// the extraction handler against in-memory state.
type pgxqueryRowMock struct {
	execFn    func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryFn   func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (m *pgxqueryRowMock) Begin(_ context.Context) (pgx.Tx, error) { return nil, errors.New("no tx in extraction handler") }
func (m *pgxqueryRowMock) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}
func (m *pgxqueryRowMock) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{}
}
func (m *pgxqueryRowMock) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

// mockRowScan is a test helper for QueryRow — lets the test return
// a fixed value from Scan calls.
type mockRowScan struct {
	pgx.Row
	value string
}

func (r *mockRowScan) Scan(dest ...any) error {
	if len(dest) > 0 {
		if s, ok := dest[0].(*string); ok {
			*s = r.value
		}
	}
	return nil
}

func TestStartExtraction_CreatesTaskAndEnqueuesJobs(t *testing.T) {
	// Map of queued (kind, args) pairs.
	var queued []map[string]any
	mock := &pgxqueryRowMock{
		execFn: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			// Catch the INSERT INTO river_job and capture the kind/args.
			if strings.Contains(sql, "INSERT INTO river_job") && len(args) > 0 {
				if m, ok := args[0].(string); ok {
					var parsed map[string]any
					if err := json.Unmarshal([]byte(m), &parsed); err == nil {
						queued = append(queued, parsed)
					}
				}
			}
			return pgconn.CommandTag{}, nil
		},
		queryFn: func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
			// The listUserTopics query joins topics + messages.
			if strings.Contains(sql, "FROM topics t") {
				return &mockRows{data: [][]any{
					{"topic-1", "Sprint planning", "What did we decide about the deadline?"},
					{"topic-2", "Standup", "Yesterday: code review. Today: fix flaky test."},
				}}, nil
			}
			return &mockRows{}, nil
		},
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRowScan{value: "00000000-0000-0000-0000-000000000001"}
		},
	}

	resp, err := startExtraction(context.Background(), mock, "u-1", nil, nil)
	if err != nil {
		t.Fatalf("startExtraction: %v", err)
	}
	if resp.ID == "" {
		t.Error("expected non-empty task id")
	}
	if resp.Status != "pending" {
		t.Errorf("expected status=pending, got %q", resp.Status)
	}
	if len(queued) != 2 {
		t.Errorf("expected 2 river jobs enqueued, got %d", len(queued))
	}
	for _, q := range queued {
		if q["userId"] != "u-1" {
			t.Errorf("expected userId=u-1, got %v", q["userId"])
		}
		if q["source"] != "chat_topic" {
			t.Errorf("expected source=chat_topic, got %v", q["source"])
		}
		if q["topicId"] == "" || q["topicId"] == nil {
			t.Errorf("expected non-empty topicId, got %v", q["topicId"])
		}
	}
	if len(queued) >= 2 {
		if queued[0]["topicId"] == queued[1]["topicId"] {
			t.Error("expected different topicIds per job")
		}
	}
}

func TestStartExtraction_NoTopicsMarksSuccess(t *testing.T) {
	var statusUpdates []string
	mock := &pgxqueryRowMock{
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRowScan{value: "task-empty"}
		},
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			if strings.Contains(sql, "SET status") {
				// Capture the status value passed as the first arg.
				statusUpdates = append(statusUpdates, "captured")
			}
			return pgconn.CommandTag{}, nil
		},
	}
	resp, err := startExtraction(context.Background(), mock, "u-1", nil, nil)
	if err != nil {
		t.Fatalf("startExtraction: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected status=success when no topics, got %q", resp.Status)
	}
	if len(statusUpdates) == 0 {
		t.Error("expected a status update to be issued")
	}
}

// dispatchAuth wraps h in the palace auth middleware and dispatches
// the request. Mirrors the production wiring in main.go — handlers
// read user-id from the request context (via palace.UserIDFromContext),
// so the middleware must run before they execute.
func dispatchAuth(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc(r.URL.Path, h)
	wrapped := (&palace.AuthMiddleware{}).Wrap(mux)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, r)
	return rec
}

func TestExtractionStartHandler_RejectsBadRange(t *testing.T) {
	handler := extractionStartHandler(&pgxqueryRowMock{}, nil)
	from := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body, _ := json.Marshal(extractionStartRequest{FromDate: &from, ToDate: &to})
	req := httptest.NewRequest(http.MethodPost, "/v1/memory/extraction/start", bytes.NewReader(body))
	req.Header.Set("x-arch-actor-id", "u-1")
	rec := dispatchAuth(handler, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExtractionStartHandler_RequiresUserID(t *testing.T) {
	handler := extractionStartHandler(&pgxqueryRowMock{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/memory/extraction/start", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestExtractionStartHandler_OK(t *testing.T) {
	mock := &pgxqueryRowMock{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, nil
		},
		queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &mockRowScan{value: "task-id-1"}
		},
	}
	handler := extractionStartHandler(mock, nil)
	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/memory/extraction/start", body)
	req.Header.Set("x-arch-actor-id", "u-1")
	rec := httptest.NewRecorder()
	// Wrap with the palace auth middleware so the request context
	// carries the user-id that extractionStartHandler reads via
	// palace.UserIDFromContext. Mirrors the production wiring.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/memory/extraction/start", handler)
	wrapped := (&palace.AuthMiddleware{}).Wrap(mux)
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExtractionStatusHandler_RejectsAnonymous(t *testing.T) {
	handler := extractionStatusHandler(&pgxqueryRowMock{})
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/extraction/task/abc", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestExtractionStatusHandler_NotFound(t *testing.T) {
	mock := &pgxqueryRowMock{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &rowNotFound{}
		},
	}
	handler := extractionStatusHandler(mock)
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/extraction/task/missing", nil)
	req.Header.Set("x-arch-actor-id", "u-1")
	rec := dispatchAuth(handler, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExtractionStatusHandler_OK(t *testing.T) {
	mock := &pgxqueryRowMock{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &rowWithData{rows: &mockRows{data: [][]any{
				{"UserMemoryExtractionWithChatTopic", "success", []byte(`{"progress":{"totalTopics":3}}`), []byte(`null`)},
			}}}
		},
	}
	handler := extractionStatusHandler(mock)
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/extraction/task/uuid", nil)
	req.Header.Set("x-arch-actor-id", "u-1")
	rec := dispatchAuth(handler, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UserMemoryExtractionWithChatTopic") {
		t.Errorf("expected response to include task type, got: %s", rec.Body.String())
	}
}

// rowNotFound returns pgx.ErrNoRows on Scan.
type rowNotFound struct{ pgx.Row }

func (r *rowNotFound) Scan(_ ...any) error { return pgx.ErrNoRows }

// rowWithData embeds mockRows and returns its data on Scan.
type rowWithData struct {
	pgx.Row
	rows *mockRows
}

func (r *rowWithData) Scan(dest ...any) error {
	if !r.rows.Next() {
		return pgx.ErrNoRows
	}
	row := r.rows.data[r.rows.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		switch dt := d.(type) {
		case *string:
			if row[i] != nil {
				*dt = row[i].(string)
			}
		case *[]byte:
			if row[i] != nil {
				*dt = row[i].([]byte)
			}
		default:
			_ = dt
		}
	}
	return nil
}

// sanity check that our mock is reasonable
var _ = fmt.Sprintf