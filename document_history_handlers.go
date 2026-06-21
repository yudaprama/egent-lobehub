package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	autosaveWindowMs = 10 * 60 * 1000
	historySinceDays = 30
)

var sourceLimits = map[string]int{
	"autosave": 20,
	"manual":   20,
	"restore":  5,
	"system":   5,
	"llm_call": 5,
}

type SaveHistoryRequest struct {
	DocumentID          string `json:"documentId"`
	EditorData          string `json:"editorData"`
	SaveSource          string `json:"saveSource"`
	LockOwnerID         string `json:"lockOwnerId,omitempty"`
	BreakAutosaveWindow bool   `json:"breakAutosaveWindow,omitempty"`
}

type SaveHistoryResponse struct {
	SavedAt string `json:"savedAt"`
}

type HistoryItemResponse struct {
	ID         string         `json:"id"`
	EditorData map[string]any `json:"editorData"`
	SaveSource string         `json:"saveSource"`
	SavedAt    string         `json:"savedAt"`
	IsCurrent  bool           `json:"isCurrent"`
}

type CompareHistoryResponse struct {
	From HistoryItemResponse `json:"from"`
	To   HistoryItemResponse `json:"to"`
}

func generateNanoID(size int) (string, error) {
	b := make([]byte, size)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(nanoidAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = nanoidAlphabet[n.Int64()]
	}
	return string(b), nil
}

func generateHistoryID() (string, error) {
	return generateNanoID(18)
}

// --- save ---

type saveHistoryFunc func(ctx context.Context, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error)

var saveHistoryFuncOverride saveHistoryFunc

func saveDocumentHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && saveHistoryFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req SaveHistoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.DocumentID == "" || req.EditorData == "" || req.SaveSource == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "documentId, editorData, and saveSource are required"})
		return
	}
	if _, ok := sourceLimits[req.SaveSource]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid saveSource: %s", req.SaveSource)})
		return
	}

	userID := extractUserID(r)
	workspaceID := r.Header.Get("X-Workspace-ID")
	ctx := r.Context()

	var savedAt time.Time
	var err error
	if saveHistoryFuncOverride != nil {
		savedAt, err = saveHistoryFuncOverride(ctx, req, userID, workspaceID)
	} else {
		savedAt, err = execSaveDocumentHistory(ctx, dbPool, req, userID, workspaceID)
	}
	if err != nil {
		if err.Error() == "document not found" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		slog.Error("saveDocumentHistory failed", "error", err, "user_id", userID, "document_id", req.DocumentID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, SaveHistoryResponse{SavedAt: savedAt.Format(time.RFC3339)})
}

func execSaveDocumentHistory(ctx context.Context, db pgxconn, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	savedAt, err := execSaveDocumentHistoryTx(ctx, tx, req, userID, workspaceID)
	if err != nil {
		return time.Time{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, fmt.Errorf("commit: %w", err)
	}
	return savedAt, nil
}

func execSaveDocumentHistoryTx(ctx context.Context, tx pgx.Tx, req SaveHistoryRequest, userID, workspaceID string) (time.Time, error) {
	savedAt := time.Now().UTC()

	var docExists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM documents WHERE id = $1 AND user_id = $2)`,
		req.DocumentID, userID,
	).Scan(&docExists)
	if err != nil {
		return time.Time{}, fmt.Errorf("check document: %w", err)
	}
	if !docExists {
		return time.Time{}, fmt.Errorf("document not found")
	}

	if req.SaveSource == "autosave" && !req.BreakAutosaveWindow {
		var latestID string
		var latestSource string
		var latestSavedAt time.Time

		scanErr := tx.QueryRow(ctx,
			`SELECT id, save_source, saved_at FROM document_histories
			 WHERE document_id = $1 AND user_id = $2
			 ORDER BY saved_at DESC, id DESC LIMIT 1`,
			req.DocumentID, userID,
		).Scan(&latestID, &latestSource, &latestSavedAt)

		if scanErr == nil && latestSource == "autosave" {
			latestBucket := latestSavedAt.UnixMilli() / autosaveWindowMs
			currentBucket := savedAt.UnixMilli() / autosaveWindowMs
			if latestBucket == currentBucket {
				_, err := tx.Exec(ctx,
					`UPDATE document_histories SET editor_data = $1, saved_at = $2 WHERE id = $3`,
					req.EditorData, savedAt, latestID,
				)
				if err != nil {
					return time.Time{}, fmt.Errorf("coalesce update: %w", err)
				}
				return savedAt, nil
			}
		} else if scanErr != nil && scanErr != pgx.ErrNoRows {
			return time.Time{}, fmt.Errorf("check latest history: %w", scanErr)
		}
	}

	historyID, err := generateHistoryID()
	if err != nil {
		return time.Time{}, fmt.Errorf("generate history id: %w", err)
	}

	var wsID *string
	if workspaceID != "" {
		wsID = &workspaceID
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO document_histories (id, document_id, user_id, workspace_id, editor_data, save_source, saved_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		historyID, req.DocumentID, userID, wsID, req.EditorData, req.SaveSource, savedAt,
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("insert history: %w", err)
	}

	limit := sourceLimits[req.SaveSource]
	_, err = tx.Exec(ctx,
		`DELETE FROM document_histories
		 WHERE id IN (
		     SELECT id FROM document_histories
		     WHERE document_id = $1 AND user_id = $2 AND save_source = $3
		     ORDER BY saved_at DESC, id DESC
		     OFFSET $4
		 )`,
		req.DocumentID, userID, req.SaveSource, limit,
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("trim history: %w", err)
	}

	return savedAt, nil
}

// --- get ---

type getHistoryFunc func(ctx context.Context, documentID, historyID, userID string) (HistoryItemResponse, error)

var getHistoryFuncOverride getHistoryFunc

func getDocumentHistoryItemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && getHistoryFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	documentID := r.URL.Query().Get("documentId")
	historyID := r.URL.Query().Get("historyId")
	if documentID == "" || historyID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "documentId and historyId are required"})
		return
	}

	userID := extractUserID(r)
	ctx := r.Context()

	var item HistoryItemResponse
	var err error
	if getHistoryFuncOverride != nil {
		item, err = getHistoryFuncOverride(ctx, documentID, historyID, userID)
	} else {
		historySince := time.Now().AddDate(0, 0, -historySinceDays)
		item, err = execGetHistoryItem(ctx, dbPool, documentID, historyID, userID, historySince)
	}
	if err != nil {
		if err.Error() == "not found" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "history item not found"})
			return
		}
		slog.Error("getDocumentHistoryItem failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, item)
}

type pgxqueryrow interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func execGetHistoryItem(ctx context.Context, db pgxqueryrow, documentID, historyID, userID string, historySince time.Time) (HistoryItemResponse, error) {
	if historyID == "head" {
		var editorData []byte
		var updatedAt time.Time
		err := db.QueryRow(ctx,
			`SELECT editor_data, updated_at FROM documents WHERE id = $1 AND user_id = $2`,
			documentID, userID,
		).Scan(&editorData, &updatedAt)
		if err != nil {
			return HistoryItemResponse{}, fmt.Errorf("not found")
		}
		var ed map[string]any
		if editorData != nil {
			json.Unmarshal(editorData, &ed)
		}
		return HistoryItemResponse{
			ID: "head", EditorData: ed, SaveSource: "system",
			SavedAt: updatedAt.Format(time.RFC3339), IsCurrent: true,
		}, nil
	}

	var id string
	var editorData []byte
	var saveSource string
	var savedAt time.Time
	err := db.QueryRow(ctx,
		`SELECT id, editor_data, save_source, saved_at FROM document_histories
		 WHERE id = $1 AND document_id = $2 AND user_id = $3 AND saved_at >= $4`,
		historyID, documentID, userID, historySince,
	).Scan(&id, &editorData, &saveSource, &savedAt)
	if err != nil {
		return HistoryItemResponse{}, fmt.Errorf("not found")
	}
	var ed map[string]any
	if editorData != nil {
		json.Unmarshal(editorData, &ed)
	}
	return HistoryItemResponse{
		ID: id, EditorData: ed, SaveSource: saveSource,
		SavedAt: savedAt.Format(time.RFC3339), IsCurrent: false,
	}, nil
}

// --- compare ---

func compareDocumentHistoryItemsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && getHistoryFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	q := r.URL.Query()
	documentID := q.Get("documentId")
	fromID := q.Get("fromHistoryId")
	toID := q.Get("toHistoryId")
	if documentID == "" || fromID == "" || toID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "documentId, fromHistoryId, and toHistoryId are required"})
		return
	}

	userID := extractUserID(r)
	ctx := r.Context()

	var from, to HistoryItemResponse
	var err error
	if getHistoryFuncOverride != nil {
		from, err = getHistoryFuncOverride(ctx, documentID, fromID, userID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "from history item not found"})
			return
		}
		to, err = getHistoryFuncOverride(ctx, documentID, toID, userID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "to history item not found"})
			return
		}
	} else {
		historySince := time.Now().AddDate(0, 0, -historySinceDays)
		from, err = execGetHistoryItem(ctx, dbPool, documentID, fromID, userID, historySince)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "from history item not found"})
			return
		}
		to, err = execGetHistoryItem(ctx, dbPool, documentID, toID, userID, historySince)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "to history item not found"})
			return
		}
	}

	writeJSON(w, http.StatusOK, CompareHistoryResponse{From: from, To: to})
}
