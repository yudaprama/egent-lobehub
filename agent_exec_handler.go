package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"egent-lobehub/config"
	"egent-lobehub/runtime"

	"github.com/cloudwego/eino/schema"
)

// operationIDMap is a bidirectional map between FE-facing operation IDs
// and Go-internal task IDs. Thread-safe for concurrent access.
//
// The FE receives an operationId from POST /v1/agent/exec and later
// needs to resolve it to a Go taskId (for cancel, status queries, etc.).
// The reverse lookup (taskId → operationId) is used when the task
// worker completes and needs to notify the FE.
var operationIDMap = &opIDMap{
	forward:  sync.Map{}, // operationId → taskId
	backward: sync.Map{}, // taskId → operationId
}

type opIDMap struct {
	forward  sync.Map
	backward sync.Map
}

func (m *opIDMap) Put(operationID, taskID string) {
	m.forward.Store(operationID, taskID)
	m.backward.Store(taskID, operationID)
}

func (m *opIDMap) TaskIDForOp(operationID string) (string, bool) {
	v, ok := m.forward.Load(operationID)
	if !ok {
		return "", false
	}
	return v.(string), true
}

func (m *opIDMap) OpForTaskID(taskID string) (string, bool) {
	v, ok := m.backward.Load(taskID)
	if !ok {
		return "", false
	}
	return v.(string), true
}

func (m *opIDMap) Delete(operationID string) {
	if taskID, ok := m.forward.Load(operationID); ok {
		m.backward.Delete(taskID.(string))
	}
	m.forward.Delete(operationID)
}

// generateOperationID produces a random 16-byte hex operation ID.
func generateOperationID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "op_" + hex.EncodeToString(b)
}

// agentExecRequest is the JSON body for POST /v1/agent/exec.
type agentExecRequest struct {
	AgentID     string `json:"agentId"`
	UserID      string `json:"userId"`
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	Prompt      string `json:"prompt"`
	WorkspaceID string `json:"workspaceId"`
	TopicID     string `json:"topicId,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	Stream      bool   `json:"stream"`
}

// agentExecResult mirrors the FE's ExecAgentResult interface so the
// response can be consumed directly by the LobeChat frontend.
type agentExecResult struct {
	AgentID            string `json:"agentId"`
	AssistantMessageID string `json:"assistantMessageId"`
	AutoStarted        bool   `json:"autoStarted"`
	CreatedAt          string `json:"createdAt"`
	Message            string `json:"message"`
	OperationID        string `json:"operationId"`
	Status             string `json:"status"`
	Success            bool   `json:"success"`
	Timestamp          string `json:"timestamp"`
	TopicID            string `json:"topicId"`
	UserMessageID      string `json:"userMessageId"`
}

// agentStreamEvent mirrors @lobechat/agent-gateway-client AgentStreamEvent.
// Emitted as SSE data lines when stream=true.
type agentStreamEvent struct {
	Type        string `json:"type"`
	Data        any    `json:"data"`
	OperationID string `json:"operationId"`
	StepIndex   int    `json:"stepIndex"`
	Timestamp   int64  `json:"timestamp"`
	ID          string `json:"id,omitempty"`
}

// makeAgentExecHandler builds the handler closure with the given
// AiAgentService. Called from main() after aiSvc is constructed.
func makeAgentExecHandler(aiSvc *runtime.AiAgentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req agentExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if req.AgentID == "" {
			http.Error(w, "agentId is required", http.StatusBadRequest)
			return
		}
		if req.Prompt == "" {
			http.Error(w, "prompt is required", http.StatusBadRequest)
			return
		}

		userID := req.UserID
		if userID == "" {
			userID = extractUserID(r)
		}

		opID := generateOperationID()
		now := time.Now()
		nowISO := now.UTC().Format(time.RFC3339)

		// Create DB rows when Postgres is available.
		topicID := req.TopicID
		userMsgID := generateNanoIDSafe("msg")
		asstMsgID := generateNanoIDSafe("msg")

		if dbPool != nil {
			// Create or reuse topic.
			if topicID == "" {
				topicID = generateNanoIDSafe("topic")
				title := req.Prompt
				if len(title) > 100 {
					title = title[:100]
				}
				_, err := dbPool.Exec(r.Context(),
					`INSERT INTO topics (id, title, agent_id, user_id, session_id, status)
					 VALUES ($1, $2, $3, $4, $5, 'running')
					 ON CONFLICT (id) DO NOTHING`,
					topicID, title, req.AgentID, userID, nullIfEmpty(req.SessionID))
				if err != nil {
					slog.Warn("agent exec: insert topic failed", "error", err)
				}
			}

			// Insert user message.
			_, err := dbPool.Exec(r.Context(),
				`INSERT INTO messages (id, role, content, topic_id, agent_id, user_id)
				 VALUES ($1, 'user', $2, $3, $4, $5)`,
				userMsgID, req.Prompt, topicID, req.AgentID, userID)
			if err != nil {
				slog.Warn("agent exec: insert user message failed", "error", err)
			}

			// Insert agent operation.
			_, err = dbPool.Exec(r.Context(),
				`INSERT INTO agent_operations (id, user_id, agent_id, topic_id, status, model, provider, started_at)
				 VALUES ($1, $2, $3, $4, 'running', $5, $6, $7)
				 ON CONFLICT (id) DO NOTHING`,
				opID, userID, req.AgentID, nullIfEmpty(topicID), nullIfEmpty(req.Model), nullIfEmpty(req.Provider), now)
			if err != nil {
				slog.Warn("agent exec: insert agent_operation failed", "error", err)
			}

			// Insert assistant message placeholder.
			_, err = dbPool.Exec(r.Context(),
				`INSERT INTO messages (id, role, content, topic_id, agent_id, user_id)
				 VALUES ($1, 'assistant', '', $2, $3, $4)`,
				asstMsgID, topicID, req.AgentID, userID)
			if err != nil {
				slog.Warn("agent exec: insert assistant message failed", "error", err)
			}
		}

		operationIDMap.Put(opID, req.AgentID)

		agentCfg := map[string]any{"id": req.AgentID}
		if req.Model != "" {
			agentCfg["model"] = req.Model
		}
		if req.Provider != "" {
			agentCfg["provider"] = req.Provider
		}

		params := runtime.ExecAgentParams{
			AgentID:        req.AgentID,
			UserID:         userID,
			Model:          req.Model,
			Provider:       req.Provider,
			Prompt:         req.Prompt,
			WorkspaceID:    req.WorkspaceID,
			DefaultsConfig: config.DefaultAgentConfig,
			ServerConfig:   config.LoadServerDefaults(),
			AgentConfig:    agentCfg,
		}

		result, err := aiSvc.ExecAgent(r.Context(), params)
		if err != nil {
			slog.Error("agent exec failed", "error", err, "agent_id", req.AgentID)
			// Update operation status to error if DB is available.
			if dbPool != nil {
				_, _ = dbPool.Exec(r.Context(),
					`UPDATE agent_operations SET status = 'error', error = $2, completed_at = now() WHERE id = $1`,
					opID, fmt.Sprintf(`{"message":"%s"}`, err.Error()))
			}
			http.Error(w, fmt.Sprintf("agent exec: %v", err), http.StatusInternalServerError)
			return
		}

		if req.Stream {
			streamAgentExecWithIDs(w, r, result, opID, asstMsgID)
			return
		}

		content := runtime.CollectResult(result.Events)

		// Update assistant message content and operation status in DB.
		if dbPool != nil {
			_, _ = dbPool.Exec(r.Context(),
				`UPDATE messages SET content = $2 WHERE id = $1`,
				asstMsgID, content)
			_, _ = dbPool.Exec(r.Context(),
				`UPDATE agent_operations SET status = 'done', completion_reason = 'done', completed_at = now() WHERE id = $1`,
				opID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentExecResult{
			AgentID:            result.AgentID,
			AssistantMessageID: asstMsgID,
			AutoStarted:        true,
			CreatedAt:          nowISO,
			Message:            "operation started",
			OperationID:        opID,
			Status:             "running",
			Success:            true,
			Timestamp:          nowISO,
			TopicID:            topicID,
			UserMessageID:      userMsgID,
		})
	}
}

// streamAgentExecWithIDs is like streamAgentExec but includes the
// assistant message ID in the stream_start event so the FE can
// associate the stream with the correct message row.
func streamAgentExecWithIDs(w http.ResponseWriter, r *http.Request, result *runtime.ExecAgentResult, opID, asstMsgID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	bw := bufio.NewWriter(w)
	stepIndex := 0
	eventSeq := 0

	writeEvent := func(ev agentStreamEvent) {
		eventSeq++
		ev.ID = fmt.Sprintf("%s-%d", opID, eventSeq)
		data, _ := json.Marshal(ev)
		fmt.Fprintf(bw, "id: %s\nevent: agent_event\ndata: %s\n\n", ev.ID, data)
		bw.Flush()
		flusher.Flush()
	}

	now := func() int64 { return time.Now().UnixMilli() }

	writeEvent(agentStreamEvent{
		Type:        "stream_start",
		Data:        map[string]any{"assistantMessage": map[string]any{"id": asstMsgID}, "model": result.ModelUsed},
		OperationID: opID,
		StepIndex:   stepIndex,
		Timestamp:   now(),
	})

	for {
		select {
		case <-r.Context().Done():
			writeEvent(agentStreamEvent{
				Type:        "error",
				Data:        map[string]any{"message": "client disconnected"},
				OperationID: opID,
				StepIndex:   stepIndex,
				Timestamp:   now(),
			})
			return
		default:
		}

		event, ok := result.Events.Next()
		if !ok {
			writeEvent(agentStreamEvent{
				Type:        "stream_end",
				Data:        map[string]any{"reason": "stop"},
				OperationID: opID,
				StepIndex:   stepIndex,
				Timestamp:   now(),
			})
			writeEvent(agentStreamEvent{
				Type:        "agent_runtime_end",
				Data:        map[string]any{},
				OperationID: opID,
				StepIndex:   stepIndex,
				Timestamp:   now(),
			})
			// Mark operation as done in DB.
			if dbPool != nil {
				_, _ = dbPool.Exec(r.Context(),
					`UPDATE agent_operations SET status = 'done', completion_reason = 'done', completed_at = now() WHERE id = $1`,
					opID)
			}
			return
		}

		if event.Err != nil {
			slog.Warn("agent stream error", "error", event.Err, "operation_id", opID)
			writeEvent(agentStreamEvent{
				Type:        "error",
				Data:        map[string]any{"message": event.Err.Error()},
				OperationID: opID,
				StepIndex:   stepIndex,
				Timestamp:   now(),
			})
			if dbPool != nil {
				_, _ = dbPool.Exec(r.Context(),
					`UPDATE agent_operations SET status = 'error', error = $2, completed_at = now() WHERE id = $1`,
					opID, fmt.Sprintf(`{"message":"%s"}`, event.Err.Error()))
			}
			return
		}

		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}

		mo := event.Output.MessageOutput
		var msg *schema.Message
		if mo.IsStreaming {
			stream := mo.MessageStream
			for {
				chunk, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					slog.Warn("stream recv error", "error", err, "operation_id", opID)
					break
				}
				if chunk.Role == schema.Assistant && chunk.Content != "" {
					writeEvent(agentStreamEvent{
						Type: "stream_chunk",
						Data: map[string]any{
							"chunkType": "text",
							"content":   chunk.Content,
						},
						OperationID: opID,
						StepIndex:   stepIndex,
						Timestamp:   now(),
					})
				}
				if chunk.Role == schema.Assistant && len(chunk.ToolCalls) > 0 {
					toolsCalling := make([]map[string]any, 0, len(chunk.ToolCalls))
					for _, tc := range chunk.ToolCalls {
						toolsCalling = append(toolsCalling, map[string]any{
							"id":   tc.ID,
							"type": tc.Type,
							"function": map[string]any{
								"name":      tc.Function.Name,
								"arguments": tc.Function.Arguments,
							},
						})
					}
					writeEvent(agentStreamEvent{
						Type: "stream_chunk",
						Data: map[string]any{
							"chunkType":    "tools_calling",
							"toolsCalling": toolsCalling,
						},
						OperationID: opID,
						StepIndex:   stepIndex,
						Timestamp:   now(),
					})
				}
				if chunk.Role == schema.Tool {
					writeEvent(agentStreamEvent{
						Type: "tool_end",
						Data: map[string]any{
							"isSuccess": true,
							"result":    chunk.Content,
						},
						OperationID: opID,
						StepIndex:   stepIndex,
						Timestamp:   now(),
					})
					stepIndex++
				}
			}
		} else {
			var err error
			msg, err = mo.GetMessage()
			if err != nil {
				continue
			}

			if msg.Role == schema.Assistant && msg.Content != "" {
				writeEvent(agentStreamEvent{
					Type: "stream_chunk",
					Data: map[string]any{
						"chunkType": "text",
						"content":   msg.Content,
					},
					OperationID: opID,
					StepIndex:   stepIndex,
					Timestamp:   now(),
				})
			}

			if msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
				toolsCalling := make([]map[string]any, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					toolsCalling = append(toolsCalling, map[string]any{
						"id":   tc.ID,
						"type": tc.Type,
						"function": map[string]any{
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						},
					})
				}
				writeEvent(agentStreamEvent{
					Type: "stream_chunk",
					Data: map[string]any{
						"chunkType":    "tools_calling",
						"toolsCalling": toolsCalling,
					},
					OperationID: opID,
					StepIndex:   stepIndex,
					Timestamp:   now(),
				})
			}

			if msg.Role == schema.Tool {
				writeEvent(agentStreamEvent{
					Type: "tool_end",
					Data: map[string]any{
						"isSuccess": true,
						"result":    msg.Content,
					},
					OperationID: opID,
					StepIndex:   stepIndex,
					Timestamp:   now(),
				})
				stepIndex++
			}
		}
	}
}

// generateNanoIDSafe generates a prefixed nanoid. Falls back to a
// random hex string if generateNanoID is not available.
func generateNanoIDSafe(prefix string) string {
	id, err := generateNanoID(18)
	if err != nil {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		return prefix + "_" + hex.EncodeToString(b)
	}
	return prefix + "_" + id
}
