package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const nanoidAlphabet = "1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// pgxconn is the subset of pgxpool.Pool used for transactional writes.
type pgxconn interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgxquery is the subset of pgxpool.Pool used for reads.
type pgxquery interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pgxpoolLike combines read + write interfaces satisfied by *pgxpool.Pool.
type pgxpoolLike interface {
	pgxconn
	pgxquery
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a JSON {"error": msg} body. Used by the HTTP
// handlers in main package for non-success responses.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
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
