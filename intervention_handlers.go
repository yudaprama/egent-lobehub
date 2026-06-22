package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"egent-lobehub/runtime"
)

// interventionStore holds pending human-in-the-loop interventions.
// Keyed by operationId → list of pending approval requests.
type interventionStore struct {
	mu      sync.RWMutex
	pending map[string][]pendingIntervention
}

type pendingIntervention struct {
	ID        string                   `json:"id"`
	Request   runtime.ApprovalRequest  `json:"request"`
	CreatedAt time.Time                `json:"createdAt"`
	Responded bool                     `json:"responded"`
	Response  *runtime.ApprovalResponse `json:"response,omitempty"`
	respCh    chan runtime.ApprovalResponse
}

var interventions = &interventionStore{
	pending: make(map[string][]pendingIntervention),
}

// AddIntervention records a pending approval request for an operation.
// Returns the intervention ID and a channel that receives the response.
func (s *interventionStore) AddIntervention(operationID string, req runtime.ApprovalRequest) (string, chan runtime.ApprovalResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := fmt.Sprintf("int_%d", time.Now().UnixNano())
	ch := make(chan runtime.ApprovalResponse, 1)

	s.pending[operationID] = append(s.pending[operationID], pendingIntervention{
		ID:        id,
		Request:   req,
		CreatedAt: time.Now(),
		respCh:    ch,
	})

	return id, ch
}

// ListPending returns all unresponded interventions for an operation.
func (s *interventionStore) ListPending(operationID string) []pendingIntervention {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := s.pending[operationID]
	var out []pendingIntervention
	for _, p := range all {
		if !p.Responded {
			out = append(out, p)
		}
	}
	return out
}

// ListAllPending returns all unresponded interventions across all operations.
func (s *interventionStore) ListAllPending() map[string][]pendingIntervention {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string][]pendingIntervention)
	for opID, all := range s.pending {
		for _, p := range all {
			if !p.Responded {
				result[opID] = append(result[opID], p)
			}
		}
	}
	return result
}

// Respond submits a decision for a specific intervention.
func (s *interventionStore) Respond(interventionID string, resp runtime.ApprovalResponse) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, all := range s.pending {
		for i := range all {
			if all[i].ID == interventionID && !all[i].Responded {
				all[i].Responded = true
				all[i].Response = &resp
				select {
				case all[i].respCh <- resp:
				default:
				}
				return true
			}
		}
	}
	return false
}

// interventionListItem is the JSON shape returned by the list endpoint.
type interventionListItem struct {
	ID          string                  `json:"id"`
	OperationID string                  `json:"operationId"`
	ToolName    string                  `json:"toolName"`
	Identifier  string                  `json:"identifier"`
	Arguments   string                  `json:"arguments"`
	ParsedArgs  map[string]any          `json:"parsedArgs,omitempty"`
	Reason      string                  `json:"reason,omitempty"`
	CreatedAt   string                  `json:"createdAt"`
}

// handleListInterventions handles GET /v1/agent/interventions
// Returns all pending human-in-the-loop approval requests.
// Optional query param: ?operationId=op_xxx to filter by operation.
func handleListInterventions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	opFilter := r.URL.Query().Get("operationId")

	var items []interventionListItem

	if opFilter != "" {
		pending := interventions.ListPending(opFilter)
		for _, p := range pending {
			items = append(items, interventionListItem{
				ID:          p.ID,
				OperationID: opFilter,
				ToolName:    p.Request.ToolName,
				Identifier:  p.Request.Identifier,
				Arguments:   p.Request.Arguments,
				ParsedArgs:  p.Request.ParsedArgs,
				Reason:      p.Request.Reason,
				CreatedAt:   p.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
	} else {
		all := interventions.ListAllPending()
		for opID, pending := range all {
			for _, p := range pending {
				items = append(items, interventionListItem{
					ID:          p.ID,
					OperationID: opID,
					ToolName:    p.Request.ToolName,
					Identifier:  p.Request.Identifier,
					Arguments:   p.Request.Arguments,
					ParsedArgs:  p.Request.ParsedArgs,
					Reason:      p.Request.Reason,
					CreatedAt:   p.CreatedAt.UTC().Format(time.RFC3339),
				})
			}
		}
	}

	if items == nil {
		items = []interventionListItem{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"interventions": items,
		"count":         len(items),
	})
}

// handleRespondIntervention handles POST /v1/agent/interventions/{id}/respond
// Accepts a JSON body with the approval decision.
func handleRespondIntervention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract intervention ID from path: /v1/agent/interventions/{id}/respond
	path := strings.TrimPrefix(r.URL.Path, "/v1/agent/interventions/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "respond" {
		http.Error(w, "expected /v1/agent/interventions/{id}/respond", http.StatusBadRequest)
		return
	}
	interventionID := parts[0]

	var resp runtime.ApprovalResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if interventions.Respond(interventionID, resp) {
		slog.Info("intervention responded", "id", interventionID, "approved", resp.Approved)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"id":      interventionID,
		})
	} else {
		http.Error(w, "intervention not found or already responded", http.StatusNotFound)
	}
}
