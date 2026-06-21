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

type dhMockTx struct {
	pgx.Tx

	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row

	executedSQL []string
	committed   bool
}

func (m *dhMockTx) Begin(ctx context.Context) (pgx.Tx, error) { return m, nil }

func (m *dhMockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

func (m *dhMockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
}

func (m *dhMockTx) Commit(ctx context.Context) error   { m.committed = true; return nil }
func (m *dhMockTx) Rollback(ctx context.Context) error  { return nil }

type dhMockBeginner struct{ tx *dhMockTx }

func (m *dhMockBeginner) Begin(ctx context.Context) (pgx.Tx, error) { return m.tx, nil }

type dhMockQuerier struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (m *dhMockQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
}

func withSaveOverride(t *testing.T, fn saveHistoryFunc) {
	t.Helper()
	saveHistoryFuncOverride = fn
	t.Cleanup(func() { saveHistoryFuncOverride = nil })
}

func withGetOverride(t *testing.T, fn getHistoryFunc) {
	t.Helper()
	getHistoryFuncOverride = fn
	t.Cleanup(func() { getHistoryFuncOverride = nil })
}

func dhTimeRow(t *testing.T, editorData []byte, savedAt time.Time, saveSource, id string) *mockRow {
	t.Helper()
	return &mockRow{scanFn: func(dest ...any) error {
		for _, d := range dest {
			switch dt := d.(type) {
			case *[]byte:
				*dt = editorData
			case *time.Time:
				*dt = savedAt
			case *string:
				if dt != nil {
					switch len(dest) {
					case 2:
						if d == dest[0] {
							*dt = string(editorData)
						} else {
							*dt = savedAt.Format(time.RFC3339)
						}
					default:
						*dt = id
					}
				}
			case *bool:
				*dt = true
			}
		}
		return nil
	}}
}

// --- save tests ---

func TestSaveHistory_Basic(t *testing.T) {
	mtx := &dhMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			switch {
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

	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		return execSaveDocumentHistory(ctx, &dhMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"documentId": "docs_abc123",
		"editorData": `{"type":"doc","content":[]}`,
		"saveSource": "manual",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp SaveHistoryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SavedAt == "" {
		t.Error("savedAt is empty")
	}

	foundInsert := false
	foundTrim := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO document_histories") {
			foundInsert = true
		}
		if sqlContains(sql, "DELETE FROM document_histories") && sqlContains(sql, "OFFSET") {
			foundTrim = true
		}
	}
	if !foundInsert {
		t.Error("expected INSERT INTO document_histories")
	}
	if !foundTrim {
		t.Error("expected DELETE trim query")
	}
	if !mtx.committed {
		t.Error("expected commit")
	}
}

func TestSaveHistory_AutosaveCoalesce(t *testing.T) {
	now := time.Now().UTC()
	mtx := &dhMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			switch {
			case sqlContains(sql, "SELECT EXISTS"):
				return &mockRow{scanFn: func(dest ...any) error {
					if b, ok := dest[0].(*bool); ok {
						*b = true
					}
					return nil
				}}
			case sqlContains(sql, "SELECT id, save_source, saved_at"):
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if s, ok := d.(*string); ok {
								*s = "dh_latest123"
							}
						case 1:
							if s, ok := d.(*string); ok {
								*s = "autosave"
							}
						case 2:
							if tm, ok := d.(*time.Time); ok {
								*tm = now
							}
						}
					}
					return nil
				}}
			default:
				return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
			}
		},
	}

	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		return execSaveDocumentHistory(ctx, &dhMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"documentId": "docs_abc123",
		"editorData": `{"type":"doc","content":[]}`,
		"saveSource": "autosave",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	foundUpdate := false
	foundInsert := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "UPDATE document_histories") {
			foundUpdate = true
		}
		if sqlContains(sql, "INSERT INTO document_histories") {
			foundInsert = true
		}
	}
	if !foundUpdate {
		t.Error("expected UPDATE (coalesce)")
	}
	if foundInsert {
		t.Error("INSERT should not happen during coalesce")
	}
}

func TestSaveHistory_AutosaveNewWindow(t *testing.T) {
	oldTime := time.Now().UTC().Add(-20 * time.Minute)
	mtx := &dhMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			switch {
			case sqlContains(sql, "SELECT EXISTS"):
				return &mockRow{scanFn: func(dest ...any) error {
					if b, ok := dest[0].(*bool); ok {
						*b = true
					}
					return nil
				}}
			case sqlContains(sql, "SELECT id, save_source, saved_at"):
				return &mockRow{scanFn: func(dest ...any) error {
					for i, d := range dest {
						switch i {
						case 0:
							if s, ok := d.(*string); ok {
								*s = "dh_old"
							}
						case 1:
							if s, ok := d.(*string); ok {
								*s = "autosave"
							}
						case 2:
							if tm, ok := d.(*time.Time); ok {
								*tm = oldTime
							}
						}
					}
					return nil
				}}
			default:
				return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
			}
		},
	}

	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		return execSaveDocumentHistory(ctx, &dhMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"documentId": "docs_abc123",
		"editorData": `{"type":"doc","content":[]}`,
		"saveSource": "autosave",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	foundInsert := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO document_histories") {
			foundInsert = true
		}
	}
	if !foundInsert {
		t.Error("expected INSERT (new window)")
	}
}

func TestSaveHistory_AutosaveBreakWindow(t *testing.T) {
	now := time.Now().UTC()
	mtx := &dhMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			switch {
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

	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		return execSaveDocumentHistory(ctx, &dhMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"documentId":          "docs_abc123",
		"editorData":          `{"type":"doc","content":[]}`,
		"saveSource":          "autosave",
		"breakAutosaveWindow": true,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	foundInsert := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO document_histories") {
			foundInsert = true
		}
	}
	if !foundInsert {
		t.Error("expected INSERT (break window)")
	}
	_ = now
}

func TestSaveHistory_DocumentNotFound(t *testing.T) {
	mtx := &dhMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "SELECT EXISTS") {
				return &mockRow{scanFn: func(dest ...any) error {
					if b, ok := dest[0].(*bool); ok {
						*b = false
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		return execSaveDocumentHistory(ctx, &dhMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"documentId": "docs_nonexistent",
		"editorData": `{"type":"doc","content":[]}`,
		"saveSource": "manual",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body = %s", rr.Code, rr.Body.String())
	}
}

func TestSaveHistory_TrimExcess(t *testing.T) {
	mtx := &dhMockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			if sqlContains(sql, "SELECT EXISTS") {
				return &mockRow{scanFn: func(dest ...any) error {
					if b, ok := dest[0].(*bool); ok {
						*b = true
					}
					return nil
				}}
			}
			return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
		},
	}

	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		return execSaveDocumentHistory(ctx, &dhMockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"documentId": "docs_abc123",
		"editorData": `{"type":"doc","content":[]}`,
		"saveSource": "restore",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	foundTrim := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "DELETE FROM document_histories") && sqlContains(sql, "OFFSET") {
			foundTrim = true
		}
	}
	if !foundTrim {
		t.Error("expected trim DELETE query with OFFSET")
	}
}

func TestSaveHistory_InvalidSource(t *testing.T) {
	withSaveOverride(t, func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
		t.Fatal("should not be called for invalid source")
		return time.Time{}, nil
	})

	body, _ := json.Marshal(map[string]any{
		"documentId": "docs_abc123",
		"editorData": `{"type":"doc","content":[]}`,
		"saveSource": "bogus",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/documents/history/save", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	saveDocumentHistoryHandler(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rr.Code, rr.Body.String())
	}
}

// --- get tests ---

func TestGetHistoryItem_Head(t *testing.T) {
	edJSON, _ := json.Marshal(map[string]any{"type": "doc", "content": []any{}})

	withGetOverride(t, func(ctx context.Context, documentID, historyID, userID string) (HistoryItemResponse, error) {
		return execGetHistoryItem(ctx, &dhMockQuerier{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				if sqlContains(sql, "SELECT editor_data, updated_at FROM documents") {
					return &mockRow{scanFn: func(dest ...any) error {
						for i, d := range dest {
							switch i {
							case 0:
								if b, ok := d.(*[]byte); ok {
									*b = edJSON
								}
							case 1:
								if tm, ok := d.(*time.Time); ok {
									*tm = time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
								}
							}
						}
						return nil
					}}
				}
				return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
			},
		}, documentID, historyID, userID, time.Now().AddDate(0, 0, -30))
	})

	r := httptest.NewRequest(http.MethodGet, "/v1/documents/history/item?documentId=docs_abc&historyId=head", nil)
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	getDocumentHistoryItemHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var item HistoryItemResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "head" {
		t.Errorf("ID = %q, want head", item.ID)
	}
	if !item.IsCurrent {
		t.Error("expected isCurrent = true for head")
	}
}

func TestGetHistoryItem_HistoryRow(t *testing.T) {
	edJSON, _ := json.Marshal(map[string]any{"type": "doc", "content": []any{}})
	savedAt := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

	withGetOverride(t, func(ctx context.Context, documentID, historyID, userID string) (HistoryItemResponse, error) {
		return execGetHistoryItem(ctx, &dhMockQuerier{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				if sqlContains(sql, "SELECT id, editor_data, save_source, saved_at FROM document_histories") {
					return &mockRow{scanFn: func(dest ...any) error {
						for i, d := range dest {
							switch i {
							case 0:
								if s, ok := d.(*string); ok {
									*s = "dh_history123"
								}
							case 1:
								if b, ok := d.(*[]byte); ok {
									*b = edJSON
								}
							case 2:
								if s, ok := d.(*string); ok {
									*s = "manual"
								}
							case 3:
								if tm, ok := d.(*time.Time); ok {
									*tm = savedAt
								}
							}
						}
						return nil
					}}
				}
				return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
			},
		}, documentID, historyID, userID, time.Now().AddDate(0, 0, -30))
	})

	r := httptest.NewRequest(http.MethodGet, "/v1/documents/history/item?documentId=docs_abc&historyId=dh_history123", nil)
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	getDocumentHistoryItemHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var item HistoryItemResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "dh_history123" {
		t.Errorf("ID = %q", item.ID)
	}
	if item.IsCurrent {
		t.Error("expected isCurrent = false for history row")
	}
	if item.SaveSource != "manual" {
		t.Errorf("SaveSource = %q, want manual", item.SaveSource)
	}
}

func TestGetHistoryItem_NotFound(t *testing.T) {
	withGetOverride(t, func(ctx context.Context, documentID, historyID, userID string) (HistoryItemResponse, error) {
		return HistoryItemResponse{}, fmt.Errorf("not found")
	})

	r := httptest.NewRequest(http.MethodGet, "/v1/documents/history/item?documentId=docs_abc&historyId=dh_nonexistent", nil)
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	getDocumentHistoryItemHandler(rr, r)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body = %s", rr.Code, rr.Body.String())
	}
}

func TestCompareHistoryItems(t *testing.T) {
	items := map[string]HistoryItemResponse{
		"dh_from": {ID: "dh_from", EditorData: map[string]any{"type": "doc"}, SaveSource: "manual", SavedAt: "2026-06-20T10:00:00Z", IsCurrent: false},
		"dh_to":   {ID: "dh_to", EditorData: map[string]any{"type": "doc"}, SaveSource: "autosave", SavedAt: "2026-06-21T10:00:00Z", IsCurrent: false},
	}

	withGetOverride(t, func(ctx context.Context, documentID, historyID, userID string) (HistoryItemResponse, error) {
		item, ok := items[historyID]
		if !ok {
			return HistoryItemResponse{}, fmt.Errorf("not found")
		}
		return item, nil
	})

	r := httptest.NewRequest(http.MethodGet,
		"/v1/documents/history/compare?documentId=docs_abc&fromHistoryId=dh_from&toHistoryId=dh_to", nil)
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	compareDocumentHistoryItemsHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp CompareHistoryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.From.ID != "dh_from" {
		t.Errorf("From.ID = %q", resp.From.ID)
	}
	if resp.To.ID != "dh_to" {
		t.Errorf("To.ID = %q", resp.To.ID)
	}
}
