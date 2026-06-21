package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type CreateFileRequest struct {
	Name            string         `json:"name"`
	FileType        string         `json:"fileType"`
	Size            int64          `json:"size"`
	URL             string         `json:"url"`
	Hash            string         `json:"hash,omitempty"`
	ParentID        *string        `json:"parentId,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	KnowledgeBaseID *string        `json:"knowledgeBaseId,omitempty"`
}

type CreateFileResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type pgxconn interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type createFileFunc func(ctx context.Context, req CreateFileRequest, userID, workspaceID string) (string, error)

var createFileFuncOverride createFileFunc

func createFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && createFileFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req CreateFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" || req.FileType == "" || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, fileType, and url are required"})
		return
	}

	userID := extractUserID(r)
	workspaceID := r.Header.Get("X-Workspace-ID")
	ctx := r.Context()

	var fileID string
	var err error
	if createFileFuncOverride != nil {
		fileID, err = createFileFuncOverride(ctx, req, userID, workspaceID)
	} else {
		fileID, err = execCreateFile(ctx, dbPool, req, userID, workspaceID)
	}
	if err != nil {
		slog.Error("createFile failed", "error", err, "user_id", userID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusCreated, CreateFileResponse{ID: fileID, URL: req.URL})
}

func execCreateFile(ctx context.Context, db pgxconn, req CreateFileRequest, userID, workspaceID string) (string, error) {
	fileID, err := generateFileID()
	if err != nil {
		return "", fmt.Errorf("generate file id: %w", err)
	}

	var wsID *string
	if workspaceID != "" {
		wsID = &workspaceID
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	hashExists := false
	if req.Hash != "" {
		err := tx.QueryRow(ctx,
			`SELECT 1 FROM global_files WHERE hash_id = $1`, req.Hash,
		).Scan(&hashExists)
		if err != nil && err != pgx.ErrNoRows {
			return "", fmt.Errorf("check hash: %w", err)
		}
	}

	if req.Hash != "" && !hashExists {
		_, err := tx.Exec(ctx,
			`INSERT INTO global_files (hash_id, file_type, size, url, metadata, creator)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (hash_id) DO NOTHING`,
			req.Hash, req.FileType, req.Size, req.URL, marshalJSONParam(req.Metadata), userID,
		)
		if err != nil {
			return "", fmt.Errorf("insert global_files: %w", err)
		}
	}

	var fileHash *string
	if req.Hash != "" {
		fileHash = &req.Hash
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO files (id, user_id, workspace_id, file_type, file_hash, name, size, url, parent_id, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		fileID, userID, wsID, req.FileType, fileHash, req.Name, req.Size, req.URL, req.ParentID,
		marshalJSONParam(req.Metadata),
	)
	if err != nil {
		return "", fmt.Errorf("insert files: %w", err)
	}

	if req.KnowledgeBaseID != nil && *req.KnowledgeBaseID != "" {
		_, err := tx.Exec(ctx,
			`INSERT INTO knowledge_base_files (knowledge_base_id, file_id, user_id, workspace_id)
			 VALUES ($1, $2, $3, $4)`,
			*req.KnowledgeBaseID, fileID, userID, wsID,
		)
		if err != nil {
			return "", fmt.Errorf("insert knowledge_base_files: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return fileID, nil
}

const nanoidAlphabet = "1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func generateFileID() (string, error) {
	b := make([]byte, 12)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(nanoidAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = nanoidAlphabet[n.Int64()]
	}
	return "file_" + string(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func marshalJSONParam(v map[string]any) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// --- removeFile ---

type RemoveFileRequest struct {
	ID  string   `json:"id,omitempty"`
	IDs []string `json:"ids,omitempty"`
}

type RemoveFileResponse struct {
	Deleted     int      `json:"deleted"`
	StorageURLs []string `json:"storageUrls"`
}

type removeFileFunc func(ctx context.Context, ids []string, userID, workspaceID string) (RemoveFileResponse, error)

var removeFileFuncOverride removeFileFunc

func removeFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if dbPool == nil && removeFileFuncOverride == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
		return
	}

	var req RemoveFileRequest
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

	var result RemoveFileResponse
	var err error
	if removeFileFuncOverride != nil {
		result, err = removeFileFuncOverride(ctx, ids, userID, workspaceID)
	} else {
		result, err = execRemoveFiles(ctx, dbPool, ids, userID, workspaceID)
	}
	if err != nil {
		slog.Error("removeFile failed", "error", err, "user_id", userID, "count", len(ids))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func execRemoveFiles(ctx context.Context, db pgxconn, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return RemoveFileResponse{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	result, err := execRemoveFilesTx(ctx, tx, ids, userID, workspaceID)
	if err != nil {
		return result, err
	}

	if err := tx.Commit(ctx); err != nil {
		return RemoveFileResponse{}, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

func execRemoveFilesTx(ctx context.Context, tx pgx.Tx, ids []string, userID, workspaceID string) (RemoveFileResponse, error) {
	type fileInfo struct {
		id, hash, url string
		chunkTask     *string
		embedTask     *string
	}

	rows, err := tx.Query(ctx,
		`SELECT id, file_hash, chunk_task_id, embedding_task_id, url
		 FROM files WHERE id = ANY($1) AND user_id = $2`,
		ids, userID,
	)
	if err != nil {
		return RemoveFileResponse{}, fmt.Errorf("select files: %w", err)
	}

	var filesToDelete []fileInfo
	var hashList []string
	var taskIDs []string

	for rows.Next() {
		var f fileInfo
		if err := rows.Scan(&f.id, &f.hash, &f.chunkTask, &f.embedTask, &f.url); err != nil {
			rows.Close()
			return RemoveFileResponse{}, fmt.Errorf("scan file: %w", err)
		}
		filesToDelete = append(filesToDelete, f)
		if f.hash != "" {
			hashList = append(hashList, f.hash)
		}
		if f.chunkTask != nil {
			taskIDs = append(taskIDs, *f.chunkTask)
		}
		if f.embedTask != nil {
			taskIDs = append(taskIDs, *f.embedTask)
		}
	}
	rows.Close()

	if len(filesToDelete) == 0 {
		return RemoveFileResponse{Deleted: 0, StorageURLs: []string{}}, nil
	}

	fileIDs := make([]string, len(filesToDelete))
	for i, f := range filesToDelete {
		fileIDs[i] = f.id
	}

	chunkRows, err := tx.Query(ctx,
		`SELECT chunk_id FROM file_chunks WHERE file_id = ANY($1)`, fileIDs,
	)
	if err != nil {
		return RemoveFileResponse{}, fmt.Errorf("select chunk_ids: %w", err)
	}
	var chunkIDs []string
	for chunkRows.Next() {
		var cid string
		if err := chunkRows.Scan(&cid); err != nil {
			chunkRows.Close()
			return RemoveFileResponse{}, fmt.Errorf("scan chunk_id: %w", err)
		}
		chunkIDs = append(chunkIDs, cid)
	}
	chunkRows.Close()

	if len(chunkIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM embeddings WHERE chunk_id = ANY($1)`, chunkIDs,
		); err != nil {
			return RemoveFileResponse{}, fmt.Errorf("delete embeddings: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM document_chunks WHERE chunk_id = ANY($1)`, chunkIDs,
		); err != nil {
			return RemoveFileResponse{}, fmt.Errorf("delete document_chunks: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM chunks WHERE id = ANY($1)`, chunkIDs,
		); err != nil {
			return RemoveFileResponse{}, fmt.Errorf("delete chunks: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM documents WHERE file_id = ANY($1) AND source_type = 'file' AND user_id = $2`,
		fileIDs, userID,
	); err != nil {
		return RemoveFileResponse{}, fmt.Errorf("delete documents: %w", err)
	}

	if len(taskIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM async_tasks WHERE id = ANY($1)`, taskIDs,
		); err != nil {
			return RemoveFileResponse{}, fmt.Errorf("delete async_tasks: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM files WHERE id = ANY($1) AND user_id = $2`,
		fileIDs, userID,
	); err != nil {
		return RemoveFileResponse{}, fmt.Errorf("delete files: %w", err)
	}

	storageURLs := []string{}
	if len(hashList) > 0 {
		seen := make(map[string]bool)
		var uniqueHashes []string
		for _, h := range hashList {
			if !seen[h] {
				seen[h] = true
				uniqueHashes = append(uniqueHashes, h)
			}
		}

		refRows, err := tx.Query(ctx,
			`SELECT DISTINCT file_hash FROM files WHERE file_hash = ANY($1)`,
			uniqueHashes,
		)
		if err != nil {
			return RemoveFileResponse{}, fmt.Errorf("check remaining refs: %w", err)
		}
		usedHashes := make(map[string]bool)
		for refRows.Next() {
			var h string
			if err := refRows.Scan(&h); err != nil {
				refRows.Close()
				return RemoveFileResponse{}, fmt.Errorf("scan remaining ref: %w", err)
			}
			usedHashes[h] = true
		}
		refRows.Close()

		for _, hash := range uniqueHashes {
			if usedHashes[hash] {
				continue
			}
			var url string
			err := tx.QueryRow(ctx,
				`DELETE FROM global_files WHERE hash_id = $1 RETURNING url`, hash,
			).Scan(&url)
			if err != nil && err != pgx.ErrNoRows {
				return RemoveFileResponse{}, fmt.Errorf("delete global_files: %w", err)
			}
			if url != "" {
				storageURLs = append(storageURLs, url)
			}
		}
	}

	return RemoveFileResponse{
		Deleted:     len(filesToDelete),
		StorageURLs: storageURLs,
	}, nil
}