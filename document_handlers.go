package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- updateDocument ---

type UpdateDocumentRequest struct {
	ID                  string          `json:"id"`
	Content             *string         `json:"content,omitempty"`
	EditorData          *string         `json:"editorData,omitempty"`
	Title               *string         `json:"title,omitempty"`
	FileType            *string         `json:"fileType,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	ParentID            json.RawMessage `json:"parentId"`
	SaveSource          string          `json:"saveSource,omitempty"`
	BreakAutosaveWindow bool            `json:"breakAutosaveWindow,omitempty"`
	LockOwnerID         string          `json:"lockOwnerId,omitempty"`
}

type UpdateDocumentResponse struct {
	ID              string  `json:"id"`
	HistoryAppended bool    `json:"historyAppended"`
	SavedAt         *string `json:"savedAt,omitempty"`
}

type updateDocFunc func(ctx context.Context, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error)

var updateDocFuncOverride updateDocFunc

func updateDocumentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && updateDocFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req UpdateDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}

	userID := extractUserID(r)
	workspaceID := r.Header.Get("X-Workspace-ID")
	ctx := r.Context()

	var result UpdateDocumentResponse
	var err error
	if updateDocFuncOverride != nil {
		result, err = updateDocFuncOverride(ctx, req, userID, workspaceID)
	} else {
		result, err = execUpdateDocument(ctx, dbPool, req, userID, workspaceID)
	}
	if err != nil {
		if err.Error() == "document not found" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		slog.Error("updateDocument failed", "error", err, "user_id", userID, "document_id", req.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func execUpdateDocument(ctx context.Context, db pgxconn, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return UpdateDocumentResponse{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	result, err := execUpdateDocumentTx(ctx, tx, req, userID, workspaceID)
	if err != nil {
		return result, err
	}

	if err := tx.Commit(ctx); err != nil {
		return UpdateDocumentResponse{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

func execUpdateDocumentTx(ctx context.Context, tx pgx.Tx, req UpdateDocumentRequest, userID, workspaceID string) (UpdateDocumentResponse, error) {
	var currentEditorData []byte
	var currentFileID *string
	err := tx.QueryRow(ctx,
		`SELECT editor_data, file_id FROM documents WHERE id = $1 AND user_id = $2`,
		req.ID, userID,
	).Scan(&currentEditorData, &currentFileID)
	if err == pgx.ErrNoRows {
		return UpdateDocumentResponse{}, fmt.Errorf("document not found")
	}
	if err != nil {
		return UpdateDocumentResponse{}, err
	}

	var historyAppended bool
	var savedAt *time.Time
	if req.EditorData != nil {
		historyAppended = !jsonEqual(currentEditorData, []byte(*req.EditorData))
	}

	setClauses := []string{}
	args := []any{}
	argIdx := 1

	if req.Content != nil {
		setClauses = append(setClauses,
			fmt.Sprintf("content = $%d", argIdx),
			fmt.Sprintf("total_char_count = length($%d)", argIdx),
			fmt.Sprintf("total_line_count = (SELECT count(*) FROM regexp_split_to_table($%d, E'\\n'))", argIdx),
		)
		args = append(args, *req.Content)
		argIdx++
	}
	if req.EditorData != nil {
		setClauses = append(setClauses, fmt.Sprintf("editor_data = $%d", argIdx))
		args = append(args, *req.EditorData)
		argIdx++
	}
	if req.Title != nil {
		setClauses = append(setClauses,
			fmt.Sprintf("title = $%d", argIdx),
			fmt.Sprintf("filename = $%d", argIdx),
		)
		args = append(args, *req.Title)
		argIdx++
	}
	if req.FileType != nil {
		setClauses = append(setClauses, fmt.Sprintf("file_type = $%d", argIdx))
		args = append(args, *req.FileType)
		argIdx++
	}
	if req.Metadata != nil {
		setClauses = append(setClauses, fmt.Sprintf("metadata = $%d", argIdx))
		args = append(args, string(req.Metadata))
		argIdx++
	}
	if req.ParentID != nil {
		var parentIDVal any
		if string(req.ParentID) == "null" {
			parentIDVal = nil
		} else {
			var s string
			json.Unmarshal(req.ParentID, &s)
			parentIDVal = s
		}
		setClauses = append(setClauses, fmt.Sprintf("parent_id = $%d", argIdx))
		args = append(args, parentIDVal)
		argIdx++
	}

	if historyAppended {
		now := time.Now().UTC()
		savedAt = &now
		saveSource := req.SaveSource
		if saveSource == "" {
			saveSource = "autosave"
		}
		historyReq := SaveHistoryRequest{
			DocumentID:          req.ID,
			EditorData:          string(currentEditorData),
			SaveSource:          saveSource,
			BreakAutosaveWindow: req.BreakAutosaveWindow,
		}
		_, err := execSaveDocumentHistoryTx(ctx, tx, historyReq, userID, workspaceID)
		if err != nil {
			return UpdateDocumentResponse{}, fmt.Errorf("save history: %w", err)
		}
	}

	if len(setClauses) > 0 {
		query := fmt.Sprintf(
			"UPDATE documents SET %s, updated_at = now() WHERE id = $%d AND user_id = $%d",
			strings.Join(setClauses, ", "), argIdx, argIdx+1,
		)
		args = append(args, req.ID, userID)
		_, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return UpdateDocumentResponse{}, fmt.Errorf("update document: %w", err)
		}
	}

	if currentFileID != nil && (req.Title != nil || req.ParentID != nil) {
		fileSets := []string{}
		fileArgs := []any{}
		fi := 1
		if req.Title != nil {
			fileSets = append(fileSets, fmt.Sprintf("name = $%d", fi))
			fileArgs = append(fileArgs, *req.Title)
			fi++
		}
		if req.ParentID != nil {
			var parentIDVal any
			if string(req.ParentID) == "null" {
				parentIDVal = nil
			} else {
				var s string
				json.Unmarshal(req.ParentID, &s)
				parentIDVal = s
			}
			fileSets = append(fileSets, fmt.Sprintf("parent_id = $%d", fi))
			fileArgs = append(fileArgs, parentIDVal)
			fi++
		}
		fileQuery := fmt.Sprintf("UPDATE files SET %s WHERE id = $%d",
			strings.Join(fileSets, ", "), fi)
		fileArgs = append(fileArgs, *currentFileID)
		_, err := tx.Exec(ctx, fileQuery, fileArgs...)
		if err != nil {
			return UpdateDocumentResponse{}, fmt.Errorf("sync file: %w", err)
		}
	}

	var savedAtStr *string
	if savedAt != nil {
		s := savedAt.Format(time.RFC3339)
		savedAtStr = &s
	}
	return UpdateDocumentResponse{
		ID: req.ID, HistoryAppended: historyAppended, SavedAt: savedAtStr,
	}, nil
}

func jsonEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var ja, jb any
	if err := json.Unmarshal(a, &ja); err != nil {
		return string(a) == string(b)
	}
	if err := json.Unmarshal(b, &jb); err != nil {
		return string(a) == string(b)
	}
	na, _ := json.Marshal(ja)
	nb, _ := json.Marshal(jb)
	return string(na) == string(nb)
}

// --- deleteDocument ---

type RemoveDocumentRequest struct {
	ID  string   `json:"id,omitempty"`
	IDs []string `json:"ids,omitempty"`
}

type RemoveDocumentResponse struct {
	Deleted     int      `json:"deleted"`
	StorageURLs []string `json:"storageUrls"`
}

type removeDocFunc func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error)

var removeDocFuncOverride removeDocFunc

func removeDocumentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && removeDocFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req RemoveDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	var ids []string
	if req.ID != "" {
		ids = []string{req.ID}
	} else if len(req.IDs) > 0 {
		ids = req.IDs
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id or ids is required"})
		return
	}

	userID := extractUserID(r)
	workspaceID := r.Header.Get("X-Workspace-ID")
	ctx := r.Context()

	var result RemoveDocumentResponse
	var err error
	if removeDocFuncOverride != nil {
		result, err = removeDocFuncOverride(ctx, ids, userID, workspaceID)
	} else {
		result, err = execRemoveDocuments(ctx, dbPool, ids, userID, workspaceID)
	}
	if err != nil {
		slog.Error("removeDocument failed", "error", err, "user_id", userID, "count", len(ids))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

type pgxpoolLike interface {
	pgxconn
	pgxquery
}

func execRemoveDocuments(ctx context.Context, db pgxpoolLike, ids []string, userID, workspaceID string) (RemoveDocumentResponse, error) {
	allDocIDs, err := expandFolderDescendants(ctx, db, ids, userID)
	if err != nil {
		return RemoveDocumentResponse{}, err
	}
	if len(allDocIDs) == 0 {
		return RemoveDocumentResponse{Deleted: 0, StorageURLs: []string{}}, nil
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return RemoveDocumentResponse{}, err
	}
	defer tx.Rollback(ctx)

	fileRows, err := tx.Query(ctx,
		`SELECT file_id FROM documents WHERE id = ANY($1) AND file_id IS NOT NULL AND user_id = $2`,
		allDocIDs, userID,
	)
	if err != nil {
		return RemoveDocumentResponse{}, err
	}
	var fileIDs []string
	for fileRows.Next() {
		var fid string
		fileRows.Scan(&fid)
		fileIDs = append(fileIDs, fid)
	}
	fileRows.Close()

	var storageURLs []string
	if len(fileIDs) > 0 {
		result, err := execRemoveFilesTx(ctx, tx, fileIDs, userID, workspaceID)
		if err != nil {
			return RemoveDocumentResponse{}, fmt.Errorf("cascade files: %w", err)
		}
		storageURLs = result.StorageURLs
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM documents WHERE id = ANY($1) AND user_id = $2`,
		allDocIDs, userID,
	)
	if err != nil {
		return RemoveDocumentResponse{}, fmt.Errorf("delete documents: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RemoveDocumentResponse{}, err
	}

	if storageURLs == nil {
		storageURLs = []string{}
	}
	return RemoveDocumentResponse{
		Deleted:     int(tag.RowsAffected()),
		StorageURLs: storageURLs,
	}, nil
}

type pgxquery interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func expandFolderDescendants(ctx context.Context, db pgxquery, ids []string, userID string) ([]string, error) {
	visited := make(map[string]bool)
	var queue []string
	queue = append(queue, ids...)

	for len(queue) > 0 {
		batch := queue
		queue = nil

		var newBatch []string
		for _, id := range batch {
			if !visited[id] {
				visited[id] = true
				newBatch = append(newBatch, id)
			}
		}
		if len(newBatch) == 0 {
			break
		}

		rows, err := db.Query(ctx,
			`SELECT id FROM documents WHERE id = ANY($1) AND file_type = 'custom/folder' AND user_id = $2`,
			newBatch, userID,
		)
		if err != nil {
			return nil, err
		}
		var folderIDs []string
		for rows.Next() {
			var fid string
			rows.Scan(&fid)
			folderIDs = append(folderIDs, fid)
		}
		rows.Close()

		if len(folderIDs) == 0 {
			continue
		}

		childRows, err := db.Query(ctx,
			`SELECT id FROM documents WHERE parent_id = ANY($1) AND user_id = $2`,
			folderIDs, userID,
		)
		if err != nil {
			return nil, err
		}
		for childRows.Next() {
			var cid string
			childRows.Scan(&cid)
			queue = append(queue, cid)
		}
		childRows.Close()
	}

	result := make([]string, 0, len(visited))
	for id := range visited {
		result = append(result, id)
	}
	return result, nil
}

func withUpdateDocOverride(t interface{ Helper(); Cleanup(func()) }, fn updateDocFunc) {
	t.Helper()
	updateDocFuncOverride = fn
	t.Cleanup(func() { updateDocFuncOverride = nil })
}

func withRemoveDocOverride(t interface{ Helper(); Cleanup(func()) }, fn removeDocFunc) {
	t.Helper()
	removeDocFuncOverride = fn
	t.Cleanup(func() { removeDocFuncOverride = nil })
}
