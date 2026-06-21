package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type delMockTx struct {
	pgx.Tx
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	executedSQL []string
	committed   bool
}

func (m *delMockTx) Begin(ctx context.Context) (pgx.Tx, error) { return m, nil }
func (m *delMockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}
func (m *delMockTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}
func (m *delMockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
}
func (m *delMockTx) Commit(ctx context.Context) error  { m.committed = true; return nil }
func (m *delMockTx) Rollback(ctx context.Context) error { return nil }

type delMockPool struct {
	queryFn func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	tx      *delMockTx
}

func (m *delMockPool) Begin(ctx context.Context) (pgx.Tx, error) { return m.tx, nil }
func (m *delMockPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}

func withDelOverride(t *testing.T, fn removeDocFunc) {
	t.Helper()
	removeDocFuncOverride = fn
	t.Cleanup(func() { removeDocFuncOverride = nil })
}

func TestRemoveDocument_Single(t *testing.T) {
	mtx := &delMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case sqlContains(sql, "SELECT file_id FROM documents"):
				return &mockRows{data: [][]any{{"file_linked1"}}}, nil
			case sqlContains(sql, "SELECT id, file_hash"):
				return &mockRows{data: [][]any{{"file_linked1", "", nil, nil, "https://s3.example.com/f"}}}, nil
			case sqlContains(sql, "SELECT chunk_id"):
				return &mockRows{}, nil
			case sqlContains(sql, "SELECT DISTINCT file_hash"):
				return &mockRows{}, nil
			default:
				return &mockRows{}, nil
			}
		},
	}
	pool := &delMockPool{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		tx: mtx,
	}

	withDelOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error) {
		return execRemoveDocuments(ctx, pool, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_abc"})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp RemoveDocumentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.StorageURLs == nil {
		t.Error("expected non-nil storageUrls")
	}

	foundDocDelete := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "DELETE FROM documents WHERE id = ANY") {
			foundDocDelete = true
		}
	}
	if !foundDocDelete {
		t.Error("expected DELETE FROM documents")
	}
}

func TestRemoveDocument_Folder(t *testing.T) {
	mtx := &delMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			if sqlContains(sql, "SELECT file_id FROM documents") {
				return &mockRows{}, nil
			}
			return &mockRows{}, nil
		},
	}
	pool := &delMockPool{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case sqlContains(sql, "file_type = 'custom/folder'"):
				return &mockRows{data: [][]any{{"docs_folder"}}}, nil
			case sqlContains(sql, "parent_id = ANY"):
				return &mockRows{data: [][]any{{"docs_child1"}, {"docs_child2"}}}, nil
			default:
				return &mockRows{}, nil
			}
		},
		tx: mtx,
	}

	withDelOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error) {
		return execRemoveDocuments(ctx, pool, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_folder"})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	if !mtx.committed {
		t.Error("expected commit")
	}
}

func TestRemoveDocument_NestedFolders(t *testing.T) {
	callCount := 0
	pool := &delMockPool{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			callCount++
			switch {
			case sqlContains(sql, "file_type = 'custom/folder'"):
				ids := args[0].([]string)
				for _, id := range ids {
					if id == "docs_root" || id == "docs_sub" {
						return &mockRows{data: [][]any{{id}}}, nil
					}
				}
				return &mockRows{}, nil
			case sqlContains(sql, "parent_id = ANY"):
				ids := args[0].([]string)
				for _, id := range ids {
					if id == "docs_root" {
						return &mockRows{data: [][]any{{"docs_sub"}}}, nil
					}
					if id == "docs_sub" {
						return &mockRows{data: [][]any{{"docs_leaf"}}}, nil
					}
				}
				return &mockRows{}, nil
			default:
				return &mockRows{}, nil
			}
		},
	}

	descendants, err := expandFolderDescendants(context.Background(), pool, []string{"docs_root"}, "user-1")
	if err != nil {
		t.Fatal(err)
	}

	ids := make(map[string]bool)
	for _, id := range descendants {
		ids[id] = true
	}
	for _, want := range []string{"docs_root", "docs_sub", "docs_leaf"} {
		if !ids[want] {
			t.Errorf("expected %q in descendants, got %v", want, descendants)
		}
	}
}

func TestRemoveDocuments_Batch(t *testing.T) {
	mtx := &delMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			if sqlContains(sql, "SELECT file_id FROM documents") {
				return &mockRows{}, nil
			}
			return &mockRows{}, nil
		},
	}
	pool := &delMockPool{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		tx: mtx,
	}

	withDelOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error) {
		return execRemoveDocuments(ctx, pool, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"ids": []string{"docs_a", "docs_b"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
}

func TestRemoveDocument_NotFound(t *testing.T) {
	mtx := &delMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	pool := &delMockPool{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		tx: mtx,
	}

	withDelOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error) {
		return execRemoveDocuments(ctx, pool, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_nonexistent"})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp RemoveDocumentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", resp.Deleted)
	}
}

func TestRemoveDocument_NoFile(t *testing.T) {
	mtx := &delMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	pool := &delMockPool{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
		tx: mtx,
	}

	withDelOverride(t, func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error) {
		return execRemoveDocuments(ctx, pool, ids, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_nofile"})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/remove", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	removeDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	_ = fmt.Sprintf("test passed")
}
