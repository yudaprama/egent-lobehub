package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type mockRow struct {
	scanFn func(dest ...any) error
}

func (r *mockRow) Scan(dest ...any) error { return r.scanFn(dest...) }

type mockTx struct {
	pgx.Tx

	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	commitFn   func(ctx context.Context) error
	rollbackFn func(ctx context.Context) error

	executedSQL []string
	committed   bool
	rolledBack  bool
}

func (m *mockTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return m, nil
}

func (m *mockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

func (m *mockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockRow{scanFn: func(dest ...any) error { return pgx.ErrNoRows }}
}

func (m *mockTx) Commit(ctx context.Context) error {
	m.committed = true
	if m.commitFn != nil {
		return m.commitFn(ctx)
	}
	return nil
}

func (m *mockTx) Rollback(ctx context.Context) error {
	m.rolledBack = true
	if m.rollbackFn != nil {
		return m.rollbackFn(ctx)
	}
	return nil
}

type mockBeginner struct {
	tx  *mockTx
	err error
}

func (m *mockBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return m.tx, m.err
}

func withCreateFileOverride(t *testing.T, fn createFileFunc) {
	t.Helper()
	createFileFuncOverride = fn
	t.Cleanup(func() { createFileFuncOverride = nil })
}

func sqlContains(sql, substr string) bool {
	return strings.Contains(sql, substr)
}

func TestCreateFileHandler_Basic(t *testing.T) {
	mtx := &mockTx{}
	withCreateFileOverride(t, func(ctx context.Context, req CreateFileRequest, userID, workspaceID string) (string, error) {
		return execCreateFile(ctx, &mockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"name":     "report.pdf",
		"fileType": "application/pdf",
		"size":     1048576,
		"url":      "https://storage.example.com/file.pdf",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/create", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	createFileHandler(rr, r)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rr.Code, rr.Body.String())
	}

	var resp CreateFileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.ID, "file_") || len(resp.ID) != 17 {
		t.Errorf("ID = %q, want file_<12chars>", resp.ID)
	}
	if resp.URL != "https://storage.example.com/file.pdf" {
		t.Errorf("URL = %q", resp.URL)
	}

	if !mtx.committed {
		t.Error("expected commit")
	}

	foundFiles := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO files") {
			foundFiles = true
		}
	}
	if !foundFiles {
		t.Errorf("expected INSERT INTO files in %v", mtx.executedSQL)
	}
}

func TestCreateFileHandler_WithHash(t *testing.T) {
	mtx := &mockTx{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			return &mockRow{scanFn: func(dest ...any) error {
				if b, ok := dest[0].(*bool); ok {
					*b = true
				}
				return nil
			}}
		},
	}
	withCreateFileOverride(t, func(ctx context.Context, req CreateFileRequest, userID, workspaceID string) (string, error) {
		return execCreateFile(ctx, &mockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"name":     "report.pdf",
		"fileType": "application/pdf",
		"size":     1048576,
		"url":      "https://storage.example.com/file.pdf",
		"hash":     "sha256:abc123",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/create", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	createFileHandler(rr, r)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rr.Code, rr.Body.String())
	}

	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO global_files") {
			t.Error("global_files INSERT should be skipped when hash exists")
		}
	}

	foundFiles := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO files") {
			foundFiles = true
		}
	}
	if !foundFiles {
		t.Error("expected INSERT INTO files")
	}
	if !mtx.committed {
		t.Error("expected commit")
	}
}

func TestCreateFileHandler_WithKnowledgeBase(t *testing.T) {
	mtx := &mockTx{}
	withCreateFileOverride(t, func(ctx context.Context, req CreateFileRequest, userID, workspaceID string) (string, error) {
		return execCreateFile(ctx, &mockBeginner{tx: mtx}, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"name":            "report.pdf",
		"fileType":        "application/pdf",
		"size":            1048576,
		"url":             "https://storage.example.com/file.pdf",
		"knowledgeBaseId": "kb_abc123",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/create", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	createFileHandler(rr, r)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rr.Code, rr.Body.String())
	}

	foundKB := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO knowledge_base_files") {
			foundKB = true
		}
	}
	if !foundKB {
		t.Errorf("expected INSERT INTO knowledge_base_files in %v", mtx.executedSQL)
	}
	if !mtx.committed {
		t.Error("expected commit")
	}
}

func TestCreateFileHandler_MissingFields(t *testing.T) {
	withCreateFileOverride(t, func(ctx context.Context, req CreateFileRequest, userID, workspaceID string) (string, error) {
		t.Fatal("should not be called when fields are missing")
		return "", nil
	})

	body, _ := json.Marshal(map[string]any{
		"name":     "",
		"fileType": "application/pdf",
		"url":      "https://storage.example.com/file.pdf",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/create", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	createFileHandler(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] == "" {
		t.Error("expected error message in response")
	}
}

func TestCreateFileHandler_NoDB(t *testing.T) {
	oldPool := dbPool
	dbPool = nil
	t.Cleanup(func() { dbPool = oldPool })

	body, _ := json.Marshal(map[string]any{
		"name":     "report.pdf",
		"fileType": "application/pdf",
		"url":      "https://storage.example.com/file.pdf",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/files/create", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	createFileHandler(rr, r)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestCreateFileHandler_MethodNotAllowed(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/files/create", nil)
	rr := httptest.NewRecorder()

	createFileHandler(rr, r)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestGenerateFileID(t *testing.T) {
	id, err := generateFileID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "file_") {
		t.Errorf("expected file_ prefix, got %q", id)
	}
	if len(id) != 17 {
		t.Errorf("expected length 17, got %d", len(id))
	}

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := generateFileID()
		if err != nil {
			t.Fatal(err)
		}
		if ids[id] {
			t.Errorf("duplicate ID: %q", id)
		}
		ids[id] = true
	}
}

func TestMarshalJSONParam(t *testing.T) {
	if v := marshalJSONParam(nil); v != nil {
		t.Errorf("nil map should return nil, got %v", v)
	}

	v := marshalJSONParam(map[string]any{"key": "value"})
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected string, got %T", v)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["key"] != "value" {
		t.Errorf("expected key=value, got %v", m)
	}
}
