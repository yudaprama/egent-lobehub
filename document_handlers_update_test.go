package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type updMockTx struct {
	pgx.Tx
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	executedSQL []string
	committed   bool
}

func (m *updMockTx) Begin(ctx context.Context) (pgx.Tx, error) { return m, nil }
func (m *updMockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}
func (m *updMockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
}
func (m *updMockTx) Commit(ctx context.Context) error  { m.committed = true; return nil }
func (m *updMockTx) Rollback(ctx context.Context) error { return nil }

type updMockBeginner struct{ tx *updMockTx }

func (m *updMockBeginner) Begin(ctx context.Context) (pgx.Tx, error) { return m.tx, nil }

func withUpdOverride(t *testing.T, fn updateDocFunc) {
	t.Helper()
	updateDocFuncOverride = fn
	t.Cleanup(func() { updateDocFuncOverride = nil })
}

func strP(s string) *string { return &s }

func TestUpdateDocument_ContentOnly(t *testing.T) {
	mtx := &updMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "SELECT editor_data, file_id FROM documents") {
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if b, ok := d.(*[]byte); ok {
								*b = nil
							}
						case 1:
							if s, ok := d.(**string); ok {
								*s = nil
							}
						}
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withUpdOverride(t, func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
		return execUpdateDocument(ctx, &updMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	content := "Hello world"
	body, _ := json.Marshal(map[string]any{"id": "docs_abc", "content": content})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/update", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	updateDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp UpdateDocumentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ID != "docs_abc" {
		t.Errorf("ID = %q", resp.ID)
	}
	if resp.HistoryAppended {
		t.Error("no history expected (no editorData change)")
	}

	foundContentUpdate := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "UPDATE documents") && sqlContains(sql, "total_char_count") {
			foundContentUpdate = true
		}
	}
	if !foundContentUpdate {
		t.Error("expected UPDATE with total_char_count")
	}
}

func TestUpdateDocument_WithHistory(t *testing.T) {
	oldED := `{"type":"doc","content":[{"old":true}]}`
	mtx := &updMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			switch {
			case sqlContains(sql, "SELECT editor_data, file_id FROM documents"):
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if b, ok := d.(*[]byte); ok {
								*b = []byte(oldED)
							}
						case 1:
							if s, ok := d.(**string); ok {
								*s = nil
							}
						}
					}
					return nil
				}}
			case sqlContains(sql, "SELECT EXISTS"):
				return &mockRow{scanFn: func(dest ...any) error {
					if b, ok := dest[0].(*bool); ok {
						*b = true
					}
					return nil
				}}
			default:
				return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
			}
		},
	}

	withUpdOverride(t, func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
		return execUpdateDocument(ctx, &updMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	newED := `{"type":"doc","content":[{"new":true}]}`
	body, _ := json.Marshal(map[string]any{
		"id": "docs_abc", "editorData": newED, "saveSource": "manual",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/update", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	updateDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp UpdateDocumentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.HistoryAppended {
		t.Error("expected historyAppended = true")
	}
	if resp.SavedAt == nil {
		t.Error("expected savedAt to be set")
	}

	foundHistoryInsert := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO document_histories") {
			foundHistoryInsert = true
		}
	}
	if !foundHistoryInsert {
		t.Error("expected INSERT INTO document_histories")
	}
}

func TestUpdateDocument_NoChanges(t *testing.T) {
	ed := `{"type":"doc","content":[]}`
	mtx := &updMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "SELECT editor_data, file_id FROM documents") {
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if b, ok := d.(*[]byte); ok {
								*b = []byte(ed)
							}
						case 1:
							if s, ok := d.(**string); ok {
								*s = nil
							}
						}
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withUpdOverride(t, func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
		return execUpdateDocument(ctx, &updMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_abc", "editorData": ed})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/update", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	updateDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp UpdateDocumentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.HistoryAppended {
		t.Error("same editorData should not append history")
	}
}

func TestUpdateDocument_FileSync(t *testing.T) {
	fileID := "file_linked123"
	mtx := &updMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "SELECT editor_data, file_id FROM documents") {
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if b, ok := d.(*[]byte); ok {
								*b = nil
							}
						case 1:
							if s, ok := d.(**string); ok {
								*s = &fileID
							}
						}
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withUpdOverride(t, func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
		return execUpdateDocument(ctx, &updMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_abc", "title": "New Title"})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/update", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	updateDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	foundFileSync := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "UPDATE files SET") && sqlContains(sql, "name") {
			foundFileSync = true
		}
	}
	if !foundFileSync {
		t.Error("expected UPDATE files for title sync")
	}
}

func TestUpdateDocument_NotFound(t *testing.T) {
	mtx := &updMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withUpdOverride(t, func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
		return execUpdateDocument(ctx, &updMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_nonexistent"})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/update", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	updateDocumentHandler(rr, r)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestUpdateDocument_ParentIdNull(t *testing.T) {
	mtx := &updMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "SELECT editor_data, file_id FROM documents") {
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if b, ok := d.(*[]byte); ok {
								*b = nil
							}
						case 1:
							if s, ok := d.(**string); ok {
								*s = nil
							}
						}
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withUpdOverride(t, func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
		return execUpdateDocument(ctx, &updMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{"id": "docs_abc", "parentId": nil})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/update", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	updateDocumentHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	foundParentUpdate := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "UPDATE documents") && sqlContains(sql, "parent_id") {
			foundParentUpdate = true
		}
	}
	if !foundParentUpdate {
		t.Error("expected UPDATE with parent_id")
	}
}
