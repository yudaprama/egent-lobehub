package palace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SearchInput is the parameter set for [Store.Search]. It mirrors the
// pREST userMemorySearchHybrid.read.sql template: an optional free-text
// query plus optional layer/category/type/tag/status filters. The query
// drives semantic ranking when an embedder is configured, and falls back
// to an ILIKE + recency search when it is not.
type SearchInput struct {
	// Query is the natural-language search string. When an embedder is
	// configured it is embedded and used for cosine-similarity ranking;
	// otherwise it is applied as an ILIKE filter on title/summary/details.
	Query string `json:"query"`
	// Layers filters on user_memories.memory_layer (e.g. "identity").
	Layers []string `json:"layers,omitempty"`
	// Categories filters on user_memories.memory_category.
	Categories []string `json:"categories,omitempty"`
	// Types filters on user_memories.memory_type.
	Types []string `json:"types,omitempty"`
	// Tags filters on user_memories.tags (array overlap — ANY match).
	Tags []string `json:"tags,omitempty"`
	// Statuses filters on user_memories.status. Defaults to ["active"]
	// when empty, matching the pREST template.
	Statuses []string `json:"statuses,omitempty"`
	// Limit caps the result count. Defaults to 20, clamped to 100.
	Limit int `json:"limit,omitempty"`
}

// SearchResult is one ranked row from [Store.Search]. It reuses the
// shared parent shape and adds the similarity Score. Score is the cosine
// similarity (1 - distance) in [0,1] when the result was semantically
// ranked, and 0 when the search fell back to recency ordering.
type SearchResult struct {
	BaseMemory
	Score float64 `json:"score"`
}

// Search returns the user's memories ranked by relevance to in.Query.
//
// When the store has an embedder, the query is embedded (1024-d) and rows
// are ordered by cosine distance against summary_vector_1024 (NULL vectors
// sort last). When no embedder is configured the query is applied as an
// ILIKE filter and rows are ordered by captured_at DESC — matching the
// NULL-embedding path of userMemorySearchHybrid.read.sql. All results are
// scoped to userID.
func (s *PgStore) Search(ctx context.Context, userID string, in SearchInput) ([]SearchResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	args := []any{userID}
	idx := 2 // $1 is userID

	// Decide ranking mode up front: semantic when an embedder is wired and
	// the query is non-empty, recency otherwise.
	var vecLiteral string
	ranked := false
	if in.Query != "" {
		v, err := s.embedIfConfigured(ctx, in.Query)
		if err != nil {
			return nil, err
		}
		if v != "" {
			vecLiteral = v
			ranked = true
		}
	}

	var scoreExpr, orderExpr string
	if ranked {
		args = append(args, vecLiteral)
		vecIdx := idx
		idx++
		scoreExpr = fmt.Sprintf("1 - (m.summary_vector_1024 <=> $%d::vector)", vecIdx)
		orderExpr = fmt.Sprintf("m.summary_vector_1024 <=> $%d::vector ASC NULLS LAST", vecIdx)
	} else {
		scoreExpr = "NULL::float"
		orderExpr = "m.captured_at DESC"
	}

	where := []string{"m.user_id = $1"}

	// Status filter — default to 'active' when none supplied.
	if len(in.Statuses) > 0 {
		where = append(where, "m.status IN ("+inPlaceholders(in.Statuses, &idx, &args)+")")
	} else {
		where = append(where, "m.status = 'active'")
	}

	// Free-text ILIKE only on the recency path: when ranked, the vector
	// ordering already encodes relevance and an ILIKE AND-filter would
	// wrongly exclude semantically-related rows that share no substring.
	if !ranked && in.Query != "" {
		where = append(where, fmt.Sprintf(
			"(m.title ILIKE '%%' || $%d || '%%' OR m.summary ILIKE '%%' || $%d || '%%' OR m.details ILIKE '%%' || $%d || '%%')",
			idx, idx, idx))
		args = append(args, in.Query)
		idx++
	}

	if len(in.Layers) > 0 {
		where = append(where, "m.memory_layer IN ("+inPlaceholders(in.Layers, &idx, &args)+")")
	}
	if len(in.Categories) > 0 {
		where = append(where, "m.memory_category IN ("+inPlaceholders(in.Categories, &idx, &args)+")")
	}
	if len(in.Types) > 0 {
		where = append(where, "m.memory_type IN ("+inPlaceholders(in.Types, &idx, &args)+")")
	}
	if len(in.Tags) > 0 {
		where = append(where, fmt.Sprintf("m.tags && $%d::text[]", idx))
		args = append(args, in.Tags)
		idx++
	}

	query := fmt.Sprintf(`
		SELECT m.id, m.memory_category, m.memory_layer, m.memory_type,
		       m.title, m.summary, m.details, m.tags, m.metadata, m.status,
		       m.accessed_count, m.last_accessed_at, m.captured_at,
		       %s AS score
		FROM   user_memories m
		WHERE  %s
		ORDER  BY %s
		LIMIT  %d`,
		scoreExpr, strings.Join(where, " AND "), orderExpr, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("palace: search query: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var (
			r              SearchResult
			category       *string
			memType        *string
			title          *string
			summary        *string
			details        *string
			tags           []string
			metaBytes      []byte
			lastAccessedAt *time.Time
			capturedAt     *time.Time
			score          *float64
		)
		if err := rows.Scan(
			&r.ID, &category, &r.MemoryLayer, &memType,
			&title, &summary, &details, &tags, &metaBytes, &r.Status,
			&r.AccessedCount, &lastAccessedAt, &capturedAt,
			&score,
		); err != nil {
			return nil, fmt.Errorf("palace: search scan: %w", err)
		}
		r.UserID = userID
		r.MemoryCategory = deref(category)
		r.MemoryType = deref(memType)
		r.Title = deref(title)
		r.Summary = deref(summary)
		r.Details = deref(details)
		r.Tags = tags
		if len(metaBytes) > 0 {
			var meta JSONMap
			if err := json.Unmarshal(metaBytes, &meta); err != nil {
				return nil, fmt.Errorf("palace: search decode metadata: %w", err)
			}
			r.Metadata = meta
		}
		if lastAccessedAt != nil {
			r.LastAccessedAt = *lastAccessedAt
		}
		if capturedAt != nil {
			r.CapturedAt = *capturedAt
		}
		if score != nil {
			r.Score = *score
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("palace: search rows: %w", err)
	}
	return out, nil
}

// inPlaceholders appends each value in vals to *args and returns a
// comma-separated list of positional placeholders ($n, $n+1, ...),
// advancing *idx. Used to build IN (...) clauses safely.
func inPlaceholders(vals []string, idx *int, args *[]any) string {
	phs := make([]string, len(vals))
	for i, v := range vals {
		phs[i] = fmt.Sprintf("$%d", *idx)
		*args = append(*args, v)
		*idx++
	}
	return strings.Join(phs, ",")
}

// deref returns the pointed-to string, or "" when the pointer is nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
