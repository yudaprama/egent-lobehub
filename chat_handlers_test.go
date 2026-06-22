package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type chatMockTx struct {
	pgx.Tx
	execFn  func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	queryFn func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	executedSQL []string
	committed   bool
}

func (m *chatMockTx) Begin(ctx context.Context) (pgx.Tx, error) { return m, nil }
func (m *chatMockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}
func (m *chatMockTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	m.executedSQL = append(m.executedSQL, sql)
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}
func (m *chatMockTx) Commit(ctx context.Context) error  { m.committed = true; return nil }
func (m *chatMockTx) Rollback(ctx context.Context) error { return nil }

type chatMockPool struct {
	queryFn func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	tx      *chatMockTx
}

func (m *chatMockPool) Begin(ctx context.Context) (pgx.Tx, error) { return m.tx, nil }
func (m *chatMockPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}
func (m *chatMockPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockRow{}
}
func (m *chatMockPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func withChatOverride(t *testing.T, fn sendChatFunc) {
	t.Helper()
	sendChatFuncOverride = fn
	t.Cleanup(func() { sendChatFuncOverride = nil })
}

func TestSendChat_Basic(t *testing.T) {
	mtx := &chatMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			switch {
			case sqlContains(sql, "SELECT id, role, content"):
				return &mockRows{data: [][]any{
					{"msg_user1", "user", "Hello", nil, nil, nil, nil, nil, nil, time.Now()},
					{"msg_asst1", "assistant", "Loading...", nil, nil, nil, nil, nil, nil, time.Now()},
				}}, nil
			case sqlContains(sql, "SELECT id, title"):
				return &mockRows{data: [][]any{
					{"tpc_1", "Topic 1", time.Now()},
				}}, nil
			default:
				return &mockRows{}, nil
			}
		},
	}
	pool := &chatMockPool{
		queryFn: mtx.queryFn,
		tx:      mtx,
	}

	withChatOverride(t, func(ctx context.Context, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error) {
		return execSendChat(ctx, pool, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"agentId":   "agt_abc",
		"sessionId": "ssn_1",
		"topicId":   "tpc_1",
		"groupId":   "sg_default",
		"newUserMessage": map[string]any{
			"content": "Hello!",
			"role":    "user",
		},
		"newAssistantMessage": map[string]any{
			"model":    "gpt-4o",
			"provider": "openai",
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/send", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	sendChatHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp SendChatResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.UserMessageID == "" {
		t.Error("expected userMessageId")
	}
	if resp.AssistantMessageID == "" {
		t.Error("expected assistantMessageId")
	}
	if resp.TopicID != "tpc_1" {
		t.Errorf("TopicID = %q", resp.TopicID)
	}

	foundUserMsg := false
	foundAsstMsg := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO messages") {
			foundUserMsg = true
			foundAsstMsg = true
		}
	}
	if !foundUserMsg || !foundAsstMsg {
		t.Error("expected message INSERTs")
	}
}

func TestSendChat_NewTopic(t *testing.T) {
	mtx := &chatMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	pool := &chatMockPool{queryFn: mtx.queryFn, tx: mtx}

	withChatOverride(t, func(ctx context.Context, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error) {
		return execSendChat(ctx, pool, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"agentId":   "agt_abc",
		"sessionId": "ssn_1",
		"newTopic":  map[string]any{"title": "New Chat", "trigger": "user"},
		"newUserMessage": map[string]any{
			"content": "Hello!",
			"role":    "user",
		},
		"newAssistantMessage": map[string]any{
			"model": "gpt-4o", "provider": "openai",
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/send", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	sendChatHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp SendChatResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.IsCreateNewTopic {
		t.Error("expected isCreateNewTopic = true")
	}

	foundTopicInsert := false
	foundAgentUpdate := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO topics") {
			foundTopicInsert = true
		}
		if sqlContains(sql, "UPDATE agents SET updated_at") {
			foundAgentUpdate = true
		}
	}
	if !foundTopicInsert {
		t.Error("expected INSERT INTO topics")
	}
	if !foundAgentUpdate {
		t.Error("expected UPDATE agents")
	}
}

func TestSendChat_NewThread(t *testing.T) {
	mtx := &chatMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	pool := &chatMockPool{queryFn: mtx.queryFn, tx: mtx}

	withChatOverride(t, func(ctx context.Context, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error) {
		return execSendChat(ctx, pool, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"agentId":   "agt_abc",
		"sessionId": "ssn_1",
		"topicId":   "tpc_1",
		"newThread": map[string]any{
			"title": "Thread", "type": "subtopic", "sourceMessageId": "msg_1",
		},
		"newUserMessage": map[string]any{
			"content": "Hello!", "role": "user",
		},
		"newAssistantMessage": map[string]any{
			"model": "gpt-4o", "provider": "openai",
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/send", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	sendChatHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp SendChatResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.CreatedThreadID == nil {
		t.Error("expected createdThreadId")
	}

	foundThreadInsert := false
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO threads") {
			foundThreadInsert = true
		}
	}
	if !foundThreadInsert {
		t.Error("expected INSERT INTO threads")
	}
}

func TestSendChat_PreloadMessages(t *testing.T) {
	mtx := &chatMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	pool := &chatMockPool{queryFn: mtx.queryFn, tx: mtx}

	withChatOverride(t, func(ctx context.Context, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error) {
		return execSendChat(ctx, pool, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"agentId":   "agt_abc",
		"sessionId": "ssn_1",
		"topicId":   "tpc_1",
		"preloadMessages": []map[string]any{
			{"content": "preload 1", "role": "system"},
			{"content": "preload 2", "role": "system"},
		},
		"newUserMessage": map[string]any{
			"content": "Hello!", "role": "user",
		},
		"newAssistantMessage": map[string]any{
			"model": "gpt-4o", "provider": "openai",
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/send", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	sendChatHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	msgInsertCount := 0
	for _, sql := range mtx.executedSQL {
		if sqlContains(sql, "INSERT INTO messages") {
			msgInsertCount++
		}
	}
	if msgInsertCount != 4 {
		t.Errorf("expected 4 message INSERTs (2 preload + user + assistant), got %d", msgInsertCount)
	}
}

func TestSendChat_NoSession(t *testing.T) {
	mtx := &chatMockTx{
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return &mockRows{}, nil
		},
	}
	pool := &chatMockPool{queryFn: mtx.queryFn, tx: mtx}

	withChatOverride(t, func(ctx context.Context, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error) {
		return execSendChat(ctx, pool, req, userID, workspaceID)
	})

	body, _ := json.Marshal(map[string]any{
		"agentId": "agt_abc",
		"newUserMessage": map[string]any{
			"content": "Hello!", "role": "user",
		},
		"newAssistantMessage": map[string]any{
			"model": "gpt-4o", "provider": "openai",
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/send", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	sendChatHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no session is allowed), body = %s", rr.Code, rr.Body.String())
	}
}

func TestOutputJSON_Basic(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/generate", bytes.NewReader([]byte("{}")))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	outputJSONHandler(rr, r)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rr.Code)
	}
}

func TestArchiveToolResult_BelowThreshold(t *testing.T) {
	oldPool := dbPool
	dbPool = nil
	t.Cleanup(func() { dbPool = oldPool })

	withArchiveOverride(t, func(ctx context.Context, req ArchiveToolResultRequest, userID string) (ArchiveToolResultResponse, error) {
		return execArchiveToolResult(ctx, &chatArchiveMockBeginner{}, req, userID)
	})

	body, _ := json.Marshal(map[string]any{
		"topicId": "tpc_1", "toolCallId": "call_1", "content": "short", "limit": 10000,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/archive-tool-result", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	archiveToolResultHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp ArchiveToolResultResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Archived {
		t.Error("expected archived = false for short content")
	}
}

func TestArchiveToolResult_AboveThreshold(t *testing.T) {
	mtx := &chatArchiveMockTx{}
	beginner := &chatArchiveMockBeginner{tx: mtx}

	withArchiveOverride(t, func(ctx context.Context, req ArchiveToolResultRequest, userID string) (ArchiveToolResultResponse, error) {
		return execArchiveToolResult(ctx, beginner, req, userID)
	})

	longContent := make([]byte, 20000)
	for i := range longContent {
		longContent[i] = 'x'
	}
	body, _ := json.Marshal(map[string]any{
		"topicId": "tpc_1", "toolCallId": "call_1", "content": string(longContent), "limit": 10000,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/archive-tool-result", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "user-1")
	rr := httptest.NewRecorder()

	archiveToolResultHandler(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var resp ArchiveToolResultResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Archived {
		t.Error("expected archived = true for long content")
	}
	if resp.DocumentID == "" {
		t.Error("expected documentId")
	}
}

type chatArchiveMockTx struct {
	pgx.Tx
	executedSQL []string
}

func (m *chatArchiveMockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.executedSQL = append(m.executedSQL, sql)
	return pgconn.CommandTag{}, nil
}
func (m *chatArchiveMockTx) Commit(ctx context.Context) error  { return nil }
func (m *chatArchiveMockTx) Rollback(ctx context.Context) error { return nil }

type chatArchiveMockBeginner struct {
	tx *chatArchiveMockTx
}

func (m *chatArchiveMockBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	if m.tx != nil {
		return m.tx, nil
	}
	return &chatArchiveMockTx{}, nil
}
func (m *chatArchiveMockBeginner) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
