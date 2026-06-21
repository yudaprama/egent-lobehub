package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type mockRows struct {
	pgx.Rows
	data [][]any
	pos  int
}

func (r *mockRows) Next() bool {
	r.pos++
	return r.pos <= len(r.data)
}

func (r *mockRows) Scan(dest ...any) error {
	if r.pos < 1 || r.pos > len(r.data) {
		return fmt.Errorf("no current row")
	}
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		src := row[i]
		switch dt := d.(type) {
		case *string:
			if src == nil {
				*dt = ""
			} else {
				*dt = src.(string)
			}
		case **string:
			if src == nil {
				*dt = nil
			} else if sp, ok := src.(*string); ok {
				*dt = sp
			} else {
				s := src.(string)
				*dt = &s
			}
		case *bool:
			*dt = src.(bool)
		case *[]byte:
			if src == nil {
				*dt = nil
			} else if b, ok := src.([]byte); ok {
				*dt = b
			} else if s, ok := src.(string); ok {
				*dt = []byte(s)
			}
		case *time.Time:
			if t, ok := src.(time.Time); ok {
				*dt = t
			}
		default:
			return fmt.Errorf("mockRows.Scan: unsupported dest type %T for column %d", d, i)
		}
	}
	return nil
}

func (r *mockRows) Close() {}

func (r *mockRows) Err() error { return nil }

type removeMockTx struct {
	pgx.Tx

	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row

	executedSQL []string
	committed   bool
}

func (m *removeMockTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return m, nil
}

func (m *removeMockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

func (m *removeMockTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}

func (m *removeMockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
}

func (m *removeMockTx) Commit(ctx context.Context) error {
	m.committed = true
	return nil
}

func (m *removeMockTx) Rollback(ctx context.Context) error {
	return nil
}

type removeMockBeginner struct {
	tx *removeMockTx
}

func (m *removeMockBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return m.tx, nil
}

func withRemoveOverride(t *testing.T, fn removeFileFunc) {
	t.Helper()
	removeFileFuncOverride = fn
	t.Cleanup(func() { removeFileFuncOverride = nil })
}

func strPtr(s string) *string { return &s }

func TestRemoveFile_Single(t *testing.T) {
	mtx := &removeMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case sqlContains(sql, "SELECT id, file_hash"):
				return &mockRows{data: [][]any{
					{"file_abc123def456", "sha256:abc", strPtr("task-chunk-1"), strPtr("task-embed-1"), "https://storage.example.com/file.pdf"},
				}}, nil
			case sqlContains(sql, "SELECT chunk_id FROM file_chunks"):
				return &mockRows{data: [][]any{
					{"chunk-1"},
					{"chunk-2"},
					{"chunk-3"},
				}}, nil
			case sqlContains(sql, "SELECT DISTINCT file_hash"):
				return &mockRows{}, nil
			default:
				return &mockRows{}, nil
			}
		},
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "DELETE FROM global_files") {
				return &mockRow{scanFn: func(dest ...any) error {
					if s, ok := dest[0].(*string); ok {
						*s = "https://storage.example.com/file.pdf"
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withRemoveOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
		return execRemoveFiles(ctx, &removeMockBeginner{tx: mtx}, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "file_abc123def456"})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeFileHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp RemoveFileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", resp.Deleted)
	}
	if len(resp.StorageURLs) != 1 || resp.StorageURLs[0] != "https://storage.example.com/file.pdf" {
		t.Errorf("StorageURLs = %v, want [https://storage.example.com/file.pdf]", resp.StorageURLs)
	}
	if !mtx.committed {
		t.Error("expected commit")
	}

	foundEmbed := false
	foundDocChunks := false
	foundChunks := false
	foundDocs := false
	foundTasks := false
	foundFiles := false
	for _, sql := range mtx.executedSQL {
		switch {
		case sqlContains(sql, "DELETE FROM embeddings"):
			foundEmbed = true
		case sqlContains(sql, "DELETE FROM document_chunks"):
			foundDocChunks = true
		case sqlContains(sql, "DELETE FROM chunks"):
			foundChunks = true
		case sqlContains(sql, "DELETE FROM documents"):
			foundDocs = true
		case sqlContains(sql, "DELETE FROM async_tasks"):
			foundTasks = true
		case sqlContains(sql, "DELETE FROM files"):
			foundFiles = true
		}
	}
	for _, check := range []struct {
		name string
		ok   bool
	}{
		{"embeddings", foundEmbed},
		{"document_chunks", foundDocChunks},
		{"chunks", foundChunks},
		{"documents", foundDocs},
		{"async_tasks", foundTasks},
		{"files", foundFiles},
	} {
		if !check.ok {
			t.Errorf("expected DELETE FROM %s in executed SQL", check.name)
		}
	}
}

func TestRemoveFiles_Batch(t *testing.T) {
	mtx := &removeMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case sqlContains(sql, "SELECT id, file_hash"):
				return &mockRows{data: [][]any{
					{"file_a", "sha256:shared", nil, nil, "https://storage.example.com/a.pdf"},
					{"file_b", "sha256:shared", nil, nil, "https://storage.example.com/b.pdf"},
				}}, nil
			case sqlContains(sql, "SELECT chunk_id FROM file_chunks"):
				return &mockRows{}, nil
			case sqlContains(sql, "SELECT DISTINCT file_hash"):
				return &mockRows{}, nil
			default:
				return &mockRows{}, nil
			}
		},
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "DELETE FROM global_files") {
				return &mockRow{scanFn: func(dest ...any) error {
					if s, ok := dest[0].(*string); ok {
						*s = "https://storage.example.com/a.pdf"
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withRemoveOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
		return execRemoveFiles(ctx, &removeMockBeginner{tx: mtx}, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"ids": []string{"file_a", "file_b"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeFileHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp RemoveFileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", resp.Deleted)
	}
	if len(resp.StorageURLs) != 1 {
		t.Errorf("StorageURLs = %v, want 1 URL (shared hash deleted once)", resp.StorageURLs)
	}
}

func TestRemoveFiles_SharedHash(t *testing.T) {
	mtx := &removeMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case sqlContains(sql, "SELECT id, file_hash"):
				return &mockRows{data: [][]any{
					{"file_a", "sha256:shared", nil, nil, "https://storage.example.com/a.pdf"},
					{"file_b", "sha256:shared", nil, nil, "https://storage.example.com/b.pdf"},
				}}, nil
			case sqlContains(sql, "SELECT chunk_id FROM file_chunks"):
				return &mockRows{}, nil
			case sqlContains(sql, "SELECT DISTINCT file_hash"):
				return &mockRows{data: [][]any{
					{"sha256:shared"},
				}}, nil
			default:
				return &mockRows{}, nil
			}
		},
	}

	withRemoveOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
		return execRemoveFiles(ctx, &removeMockBeginner{tx: mtx}, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"ids": []string{"file_a", "file_b"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeFileHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp RemoveFileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", resp.Deleted)
	}
	if len(resp.StorageURLs) != 0 {
		t.Errorf("StorageURLs = %v, want empty (hash still referenced)", resp.StorageURLs)
	}

	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "DELETE FROM global_files") {
			t.Error("global_files should NOT be deleted when hash is still referenced")
		}
	}
}

func TestRemoveFile_NotFound(t *testing.T) {
	mtx := &removeMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}

	withRemoveOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
		return execRemoveFiles(ctx, &removeMockBeginner{tx: mtx}, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "file_nonexistent"})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeFileHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp RemoveFileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", resp.Deleted)
	}
	if len(resp.StorageURLs) != 0 {
		t.Errorf("StorageURLs = %v, want empty", resp.StorageURLs)
	}
}

func TestRemoveFile_MissingFields(t *testing.T) {
	withRemoveOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
		t.Fatal("should not be called")
		return RemoveFileResponse{}, nil
	})

	body, _ := json.Marshal(map[string]any{})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	removeFileHandler(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] == "" {
		t.Error("expected error message")
	}
}

func TestRemoveFile_NoDB(t *testing.T) {
	oldPool := dbPool
	dbPool = nil
	t.Cleanup(func() { dbPool = oldPool })

	body, _ := json.Marshal(map[string]any{"id": "file_abc"})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	removeFileHandler(rr, r)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}
