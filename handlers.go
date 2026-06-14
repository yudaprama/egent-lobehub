package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"egent-lobehub/memory"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// extractUserID reads the user identity from the HTTP request.
// Priority order:
//  1. x-arch-actor-id header (set by Plano brightstaff after Talos verify)
//  2. X-User-ID header (dev/auth-proxied)
//  3. Authorization: kratos:<session_token> (prod)
//  4. Defaults to "anonymous"
func extractUserID(r *http.Request) string {
	if uid := r.Header.Get("x-arch-actor-id"); uid != "" {
		return uid
	}
	if uid := r.Header.Get("X-User-ID"); uid != "" {
		return uid
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "kratos:") {
		// TODO: validate token via Kratos admin API
		return strings.TrimPrefix(auth, "kratos:")
	}
	return "anonymous"
}

// extractArchAgentID reads Plano's routing header for logging/audit.
// This is NOT a user identifier — it's the agent_id that Plano
// resolved the request to. See brightstaff::handlers::agents::pipeline::build_agent_headers.
func extractArchAgentID(r *http.Request) string {
	return r.Header.Get("x-arch-upstream")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// toolsHandler returns the list of registered tools.
func toolsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tools := rt.Tools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(r.Context())
		if err != nil || info == nil {
			continue
		}
		names = append(names, info.Name)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"count": len(names),
		"tools": names,
	})
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if len(req.Messages) == 0 {
		http.Error(w, "messages cannot be empty", http.StatusBadRequest)
		return
	}

	query := buildConversationQuery(req.Messages)
	userID := extractUserID(r)
	ctx := memory.WithUserID(r.Context(), userID)
	iter, err := rt.Query(ctx, query)
	if err != nil {
		http.Error(w, fmt.Sprintf("runtime error: %v", err), http.StatusInternalServerError)
		return
	}

	if req.Stream {
		handleStreamingResponse(w, req, iter)
	} else {
		handleNonStreamingResponse(w, req, iter)
	}
}

// buildConversationQuery formats the full conversation history so the agent
// has multi-turn context. Single messages pass through as-is.
func buildConversationQuery(messages []ChatCompletionMessage) string {
	if len(messages) == 1 {
		return messages[0].Content
	}

	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			// system prompt is in the agent instruction
		case "user":
			fmt.Fprintf(&b, "User: %s\n", m.Content)
		case "assistant":
			fmt.Fprintf(&b, "Assistant: %s\n", m.Content)
		}
	}
	return b.String()
}

func handleNonStreamingResponse(w http.ResponseWriter, req ChatCompletionRequest, iter *adk.AsyncIterator[*adk.AgentEvent]) {
	var finalContent strings.Builder

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			http.Error(w, fmt.Sprintf("agent error: %v", event.Err), http.StatusInternalServerError)
			return
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				log.Printf("get message error: %v", err)
				continue
			}
			if msg.Role == schema.Assistant && msg.Content != "" {
				finalContent.WriteString(msg.Content)
			}
		}
	}

	resp := ChatCompletionResponse{
		ID:      generateID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []ChatCompletionChoice{
			{
				Index: 0,
				Message: ChatCompletionMessage{
					Role:    "assistant",
					Content: finalContent.String(),
				},
				FinishReason: "stop",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleStreamingResponse(w http.ResponseWriter, req ChatCompletionRequest, iter *adk.AsyncIterator[*adk.AgentEvent]) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	writer := bufio.NewWriter(w)
	requestID := generateID()

	for {
		event, ok := iter.Next()
		if !ok {
			finishReason := "stop"
			chunk := ChatCompletionChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []ChatCompletionChunkChoice{
					{Index: 0, Delta: ChatCompletionMessage{}, FinishReason: &finishReason},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(writer, "data: %s\n\n", data)
			fmt.Fprintf(writer, "data: [DONE]\n\n")
			writer.Flush()
			flusher.Flush()
			break
		}

		if event.Err != nil {
			log.Printf("agent error: %v", event.Err)
			errChunk := ChatCompletionChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []ChatCompletionChunkChoice{
					{
						Index: 0,
						Delta: ChatCompletionMessage{
							Role:    "assistant",
							Content: fmt.Sprintf("\n[Error: %v]", event.Err),
						},
					},
				},
			}
			errData, _ := json.Marshal(errChunk)
			fmt.Fprintf(writer, "data: %s\n\n", errData)
			writer.Flush()
			flusher.Flush()
			break
		}

		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				log.Printf("get message error: %v", err)
				continue
			}
			if msg.Role == schema.Assistant && msg.Content != "" {
				chunk := ChatCompletionChunk{
					ID:      requestID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   req.Model,
					Choices: []ChatCompletionChunkChoice{
						{
							Index: 0,
							Delta: ChatCompletionMessage{
								Role:    "assistant",
								Content: msg.Content,
							},
						},
					},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(writer, "data: %s\n\n", data)
				writer.Flush()
				flusher.Flush()
			}
		}
	}
}

func generateID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}
