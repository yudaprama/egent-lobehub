package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	fp "github.com/kawai-network/fileprocessor"
)

// ragEmbedder is the 1024-dim embedder shared with the knowledge_search tool.
// Set in main.go when the knowledge pipeline is wired (dbPool present). When nil
// the document-ingest endpoint reports "not configured".
var ragEmbedder fp.Embedder

// documentsHandler: POST /v1/documents {title, content, workspaceId?} — a thin,
// SYNCHRONOUS text-ingest endpoint that populates the same RAG tables the
// knowledge_search agent tool reads (public.files → file_chunks → chunks →
// embeddings). It reuses fileprocessor's chunker + embedder rather than the
// async River pipeline, so a plain-text/markdown upload is searchable
// immediately with no AList/Temporal/River dependency.
//
// Binary formats (PDF/DOCX) are out of scope for this v1 endpoint — the client
// extracts text first. The file row's url is a sentinel (inline://<id>) because
// the bytes live in the chunks, not external storage.
func documentsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := extractUserID(r)
	if userID == "" || userID == "anonymous" {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if dbPool == nil || ragEmbedder == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "knowledge ingest not configured")
		return
	}

	var body struct {
		Title       string `json:"title"`
		Content     string `json:"content"`
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	if body.Title == "" {
		body.Title = "Untitled"
	}
	if strings.TrimSpace(body.Content) == "" {
		writeJSONError(w, http.StatusBadRequest, "content required")
		return
	}
	// Active workspace: header is authoritative (set by the edge); body is a
	// fallback. Empty = personal scope (workspace_id NULL).
	workspaceID := r.Header.Get("X-Workspace-Id")
	if workspaceID == "" {
		workspaceID = body.WorkspaceID
	}

	ctx := r.Context()

	// 1. Chunk the text. CharChunker(1000, 200) mirrors the default RAG window.
	chunks := fp.NewCharChunker(1000, 200).Chunk(body.Content)
	if len(chunks) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no chunks produced from content")
		return
	}

	// 2. Embed all chunks (1024-dim, validated by the embedder/store).
	vectors, err := ragEmbedder.Embed(ctx, chunks)
	if err != nil {
		slog.Error("documents: embed failed", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not embed content")
		return
	}
	if len(vectors) != len(chunks) {
		slog.Error("documents: embed count mismatch", "chunks", len(chunks), "vectors", len(vectors))
		writeJSONError(w, http.StatusBadGateway, "embedding count mismatch")
		return
	}

	// 3. Persist files → chunks → file_chunks → embeddings in one tx.
	id, err := generateNanoID(16)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "id generation failed")
		return
	}
	fileID := "file_" + id
	if err := persistDocument(ctx, persistArgs{
		fileID:      fileID,
		userID:      userID,
		workspaceID: workspaceID,
		name:        body.Title,
		size:        int64(len(body.Content)),
		chunks:      chunks,
		vectors:     vectors,
	}); err != nil {
		slog.Error("documents: persist failed", "err", err)
		writeJSONError(w, http.StatusBadGateway, "could not store document")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fileId": fileID,
		"name":   body.Title,
		"chunks": len(chunks),
	})
}

type persistArgs struct {
	fileID      string
	userID      string
	workspaceID string
	name        string
	size        int64
	chunks      []string
	vectors     [][]float32
}

func persistDocument(ctx context.Context, a persistArgs) error {
	model := os.Getenv("OPENAI_EMBEDDINGS_MODEL")
	if model == "" {
		model = "text-embedding-3-small"
	}

	tx, err := dbPool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// files row — url is a sentinel; the content lives in chunks.
	if _, err := tx.Exec(ctx,
		`INSERT INTO public.files (id, user_id, file_type, name, size, url, workspace_id)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))`,
		a.fileID, a.userID, "text/plain", a.name, a.size, "inline://"+a.fileID, a.workspaceID); err != nil {
		return fmt.Errorf("insert file: %w", err)
	}

	for i, text := range a.chunks {
		var chunkID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO public.chunks (text, "index", type, user_id, workspace_id)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''))
			 RETURNING id::text`,
			text, i, "DocumentChunk", a.userID, a.workspaceID).Scan(&chunkID); err != nil {
			return fmt.Errorf("insert chunk %d: %w", i, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO public.file_chunks (chunk_id, file_id, user_id, workspace_id)
			 VALUES ($1, $2, $3, NULLIF($4, ''))`,
			chunkID, a.fileID, a.userID, a.workspaceID); err != nil {
			return fmt.Errorf("insert file_chunk %d: %w", i, err)
		}
		if len(a.vectors[i]) != 1024 {
			return fmt.Errorf("chunk %d: vector dim %d != 1024", i, len(a.vectors[i]))
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO public.embeddings (chunk_id, embeddings, model, user_id, workspace_id)
			 VALUES ($1, $2::vector, $3, $4, NULLIF($5, ''))`,
			chunkID, vectorToPGLiteral(a.vectors[i]), model, a.userID, a.workspaceID); err != nil {
			return fmt.Errorf("insert embedding %d: %w", i, err)
		}
	}

	return tx.Commit(ctx)
}

// vectorToPGLiteral renders a float32 slice as pgvector's text format
// "[v1,v2,...]" for a ::vector cast.
func vectorToPGLiteral(v []float32) string {
	b := make([]byte, 0, 1+len(v)*8)
	b = append(b, '[')
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(x), 'f', -1, 32)
	}
	b = append(b, ']')
	return string(b)
}
