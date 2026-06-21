package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- types ---

type SendChatRequest struct {
	AgentID             string             `json:"agentId"`
	SessionID           string             `json:"sessionId,omitempty"`
	TopicID             string             `json:"topicId,omitempty"`
	GroupID             string             `json:"groupId,omitempty"`
	ThreadID            *string            `json:"threadId"`
	NewTopic            *NewTopicParams    `json:"newTopic,omitempty"`
	NewThread           *NewThreadParams   `json:"newThread,omitempty"`
	NewUserMessage      NewMessageParams   `json:"newUserMessage"`
	NewAssistantMessage NewAssistantParams `json:"newAssistantMessage"`
	PreloadMessages     []NewMessageParams `json:"preloadMessages,omitempty"`
	TopicFilter         any                `json:"topicFilter,omitempty"`
	TopicPageSize       int                `json:"topicPageSize,omitempty"`
}

type NewTopicParams struct {
	Title           string `json:"title"`
	Trigger         string `json:"trigger,omitempty"`
	Metadata        any    `json:"metadata,omitempty"`
	TopicMessageIDs []string `json:"topicMessageIds,omitempty"`
}

type NewThreadParams struct {
	Title           string  `json:"title"`
	Type            string  `json:"type"`
	SourceMessageID string  `json:"sourceMessageId"`
	ParentThreadID  *string `json:"parentThreadId"`
}

type NewMessageParams struct {
	Content    string `json:"content"`
	Role       string `json:"role"`
	ParentID   *string `json:"parentId"`
	Files      any    `json:"files,omitempty"`
	EditorData any    `json:"editorData,omitempty"`
	Metadata   any    `json:"metadata,omitempty"`
	Plugin     any    `json:"plugin,omitempty"`
	Tools      any    `json:"tools,omitempty"`
	ToolCallID *string `json:"tool_call_id,omitempty"`
}

type NewAssistantParams struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Metadata any    `json:"metadata,omitempty"`
}

type SendChatResponse struct {
	UserMessageID      string `json:"userMessageId"`
	AssistantMessageID string `json:"assistantMessageId"`
	TopicID            string `json:"topicId"`
	Messages           []any  `json:"messages"`
	Topics             any    `json:"topics"`
	IsCreateNewTopic   bool   `json:"isCreateNewTopic"`
	CreatedThreadID    *string `json:"createdThreadId,omitempty"`
}

type ArchiveToolResultRequest struct {
	TopicID    string `json:"topicId"`
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"`
	AgentID    string `json:"agentId,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Limit      int    `json:"limit"`
}

type ArchiveToolResultResponse struct {
	Archived   bool   `json:"archived"`
	DocumentID string `json:"documentId,omitempty"`
}

type GenerateRequest struct {
	Messages []any    `json:"messages"`
	Model    string   `json:"model"`
	Provider string   `json:"provider"`
	Schema   any      `json:"schema,omitempty"`
	Tools    []any    `json:"tools,omitempty"`
	Metadata any      `json:"metadata,omitempty"`
	Tracing  any      `json:"tracing,omitempty"`
}

type GenerateResponse struct {
	Data      any    `json:"data"`
	TracingID string `json:"tracingId,omitempty"`
}

func generatePrefixedID(prefix string) (string, error) {
	suffix, err := generateNanoID(12)
	if err != nil {
		return "", err
	}
	return prefix + "_" + suffix, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func marshalJSON2(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

// --- sendChat ---

type sendChatFunc func(ctx context.Context, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error)

var sendChatFuncOverride sendChatFunc

func sendChatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && sendChatFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req SendChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	userID := extractUserID(r)
	workspaceID := r.Header.Get("X-Workspace-ID")
	ctx := r.Context()

	var result SendChatResponse
	var err error
	if sendChatFuncOverride != nil {
		result, err = sendChatFuncOverride(ctx, req, userID, workspaceID)
	} else {
		result, err = execSendChat(ctx, dbPool, req, userID, workspaceID)
	}
	if err != nil {
		slog.Error("sendChat failed", "error", err, "user_id", userID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func execSendChat(ctx context.Context, db pgxpoolLike, req SendChatRequest, userID, workspaceID string) (SendChatResponse, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return SendChatResponse{}, err
	}
	defer tx.Rollback(ctx)

	sessionID := req.SessionID
	topicID := req.TopicID
	threadID := req.ThreadID
	var createdThreadID *string
	isCreateNewTopic := false

	if req.NewTopic != nil {
		newTopicID, err := insertTopic(ctx, tx, req.AgentID, sessionID, req.GroupID, req.NewTopic, userID, workspaceID)
		if err != nil {
			return SendChatResponse{}, fmt.Errorf("create topic: %w", err)
		}
		topicID = newTopicID
		isCreateNewTopic = true

		if req.AgentID != "" {
			tx.Exec(ctx, `UPDATE agents SET updated_at = now() WHERE id = $1`, req.AgentID)
		}
	}

	if req.NewThread != nil {
		newThreadID, err := insertThread(ctx, tx, topicID, req.NewThread, userID, workspaceID)
		if err != nil {
			return SendChatResponse{}, fmt.Errorf("create thread: %w", err)
		}
		threadID = &newThreadID
		createdThreadID = &newThreadID
	}

	var parentID *string
	if req.NewUserMessage.ParentID != nil {
		parentID = req.NewUserMessage.ParentID
	}
	for _, preload := range req.PreloadMessages {
		msgID, err := insertMessage(ctx, tx, preload, req.AgentID, sessionID, topicID, threadID, req.GroupID, parentID, userID, workspaceID)
		if err != nil {
			return SendChatResponse{}, fmt.Errorf("create preload: %w", err)
		}
		parentID = &msgID
	}

	userMsg := req.NewUserMessage
	userMsg.Role = "user"
	userMsg.ParentID = parentID
	userMessageID, err := insertMessage(ctx, tx, userMsg, req.AgentID, sessionID, topicID, threadID, req.GroupID, parentID, userID, workspaceID)
	if err != nil {
		return SendChatResponse{}, fmt.Errorf("create user message: %w", err)
	}

	assistantMsg := NewMessageParams{
		Content: "Loading...",
		Role:    "assistant",
	}
	assistantMessageID, err := insertMessage(ctx, tx, assistantMsg, req.AgentID, sessionID, topicID, threadID, req.GroupID, &userMessageID, userID, workspaceID)
	if err != nil {
		return SendChatResponse{}, fmt.Errorf("create assistant message: %w", err)
	}
	tx.Exec(ctx,
		`UPDATE messages SET model = $1, provider = $2 WHERE id = $3`,
		req.NewAssistantMessage.Model, req.NewAssistantMessage.Provider, assistantMessageID,
	)

	if err := tx.Commit(ctx); err != nil {
		return SendChatResponse{}, err
	}

	messages, err := fetchChatMessages(ctx, db, topicID, userID)
	if err != nil {
		return SendChatResponse{}, fmt.Errorf("fetch messages: %w", err)
	}

	topics, err := fetchChatTopics(ctx, db, sessionID, userID)
	if err != nil {
		return SendChatResponse{}, fmt.Errorf("fetch topics: %w", err)
	}

	return SendChatResponse{
		UserMessageID:      userMessageID,
		AssistantMessageID: assistantMessageID,
		TopicID:            topicID,
		Messages:           messages,
		Topics:             topics,
		IsCreateNewTopic:   isCreateNewTopic,
		CreatedThreadID:    createdThreadID,
	}, nil
}

func insertTopic(ctx context.Context, tx pgx.Tx, agentID, sessionID, groupID string, params *NewTopicParams, userID, workspaceID string) (string, error) {
	id, err := generatePrefixedID("tpc")
	if err != nil {
		return "", err
	}
	var wsID *string
	if workspaceID != "" {
		wsID = &workspaceID
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO topics (id, title, session_id, agent_id, group_id, trigger, metadata, user_id, workspace_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, params.Title, sessionID, nullIfEmpty(agentID), nullIfEmpty(groupID),
		nullIfEmpty(params.Trigger), marshalJSON2(params.Metadata), userID, wsID,
	)
	return id, err
}

func insertThread(ctx context.Context, tx pgx.Tx, topicID string, params *NewThreadParams, userID, workspaceID string) (string, error) {
	id, err := generatePrefixedID("thd")
	if err != nil {
		return "", err
	}
	var wsID *string
	if workspaceID != "" {
		wsID = &workspaceID
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO threads (id, topic_id, title, type, source_message_id, parent_thread_id, user_id, workspace_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, topicID, params.Title, params.Type, params.SourceMessageID,
		params.ParentThreadID, userID, wsID,
	)
	return id, err
}

func insertMessage(ctx context.Context, tx pgx.Tx, msg NewMessageParams, agentID, sessionID, topicID string, threadID *string, groupID string, parentID *string, userID, workspaceID string) (string, error) {
	id, err := generatePrefixedID("msg")
	if err != nil {
		return "", err
	}
	var wsID *string
	if workspaceID != "" {
		wsID = &workspaceID
	}
	var tid *string
	if threadID != nil && *threadID != "" {
		tid = threadID
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO messages (id, role, content, topic_id, session_id, agent_id,
		 parent_id, thread_id, group_id, files, editor_data, metadata, user_id, workspace_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		id, msg.Role, msg.Content, topicID, sessionID, nullIfEmpty(agentID),
		parentID, tid, nullIfEmpty(groupID),
		marshalJSON2(msg.Files), marshalJSON2(msg.EditorData), marshalJSON2(msg.Metadata),
		userID, wsID,
	)
	return id, err
}

func fetchChatMessages(ctx context.Context, db pgxquery, topicID, userID string) ([]any, error) {
	rows, err := db.Query(ctx,
		`SELECT id, role, content, parent_id, model, provider, metadata,
		        files, editor_data, created_at
		 FROM messages
		 WHERE topic_id = $1 AND user_id = $2
		 ORDER BY created_at ASC`,
		topicID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []any
	for rows.Next() {
		var id, role, content string
		var parentID, model, provider *string
		var metadata, files, editorData []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &role, &content, &parentID, &model, &provider, &metadata, &files, &editorData, &createdAt); err != nil {
			return nil, err
		}
		msg := map[string]any{
			"id": id, "role": role, "content": content,
			"parentId": parentID, "model": model, "provider": provider,
			"createdAt": createdAt.Format(time.RFC3339),
		}
		if metadata != nil {
			var m any
			json.Unmarshal(metadata, &m)
			msg["metadata"] = m
		}
		if files != nil {
			var f any
			json.Unmarshal(files, &f)
			msg["files"] = f
		}
		if editorData != nil {
			var ed any
			json.Unmarshal(editorData, &ed)
			msg["editorData"] = ed
		}
		messages = append(messages, msg)
	}
	if messages == nil {
		messages = []any{}
	}
	return messages, nil
}

func fetchChatTopics(ctx context.Context, db pgxquery, sessionID, userID string) (any, error) {
	rows, err := db.Query(ctx,
		`SELECT id, title, created_at FROM topics
		 WHERE session_id = $1 AND user_id = $2
		 ORDER BY created_at DESC
		 LIMIT 50`,
		sessionID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []any
	for rows.Next() {
		var id, title string
		var createdAt time.Time
		if err := rows.Scan(&id, &title, &createdAt); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"id": id, "title": title, "createdAt": createdAt.Format(time.RFC3339),
		})
	}
	if items == nil {
		items = []any{}
	}
	return map[string]any{"items": items}, nil
}

// --- archiveToolResult ---

type archiveFunc func(ctx context.Context, req ArchiveToolResultRequest, userID string) (ArchiveToolResultResponse, error)

var archiveFuncOverride archiveFunc

func archiveToolResultHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && archiveFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req ArchiveToolResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	userID := extractUserID(r)
	ctx := r.Context()

	var result ArchiveToolResultResponse
	var err error
	if archiveFuncOverride != nil {
		result, err = archiveFuncOverride(ctx, req, userID)
	} else {
		result, err = execArchiveToolResult(ctx, dbPool, req, userID)
	}
	if err != nil {
		slog.Error("archiveToolResult failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func execArchiveToolResult(ctx context.Context, db pgxconn, req ArchiveToolResultRequest, userID string) (ArchiveToolResultResponse, error) {
	limit := req.Limit
	if limit == 0 {
		limit = 10000
	}
	if len(req.Content) <= limit {
		return ArchiveToolResultResponse{Archived: false}, nil
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return ArchiveToolResultResponse{}, err
	}
	defer tx.Rollback(ctx)

	docID, err := generatePrefixedID("docs")
	if err != nil {
		return ArchiveToolResultResponse{}, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO documents (id, user_id, content, title, file_type, editor_data, total_char_count, total_line_count)
		 VALUES ($1, $2, $3, $4, 'custom/tool-result', $5, length($3), (SELECT count(*) FROM regexp_split_to_table($3, E'\n')))`,
		docID, userID, req.Content, fmt.Sprintf("Tool result: %s", req.ToolCallID),
		fmt.Sprintf(`{"toolCallId":"%s"}`, req.ToolCallID),
	)
	if err != nil {
		return ArchiveToolResultResponse{}, fmt.Errorf("insert document: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ArchiveToolResultResponse{}, err
	}

	return ArchiveToolResultResponse{Archived: true, DocumentID: docID}, nil
}

// --- outputJSON ---

func outputJSONHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "outputJSON requires LLM provider integration; use /v1/chat/completions instead",
	})
}

func withSendChatOverride(t interface{ Helper(); Cleanup(func()) }, fn sendChatFunc) {
	t.Helper()
	sendChatFuncOverride = fn
	t.Cleanup(func() { sendChatFuncOverride = nil })
}

func withArchiveOverride(t interface{ Helper(); Cleanup(func()) }, fn archiveFunc) {
	t.Helper()
	archiveFuncOverride = fn
	t.Cleanup(func() { archiveFuncOverride = nil })
}
