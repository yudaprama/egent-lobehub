package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/http"

	"github.com/jackc/pgx/v5"
)

const nanoidAlphabet = "1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// pgxconn is the subset of pgxpool.Pool used for transactional writes.
type pgxconn interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// pgxquery is the subset of pgxpool.Pool used for reads.
type pgxquery interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
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
