package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// mockRows is a test helper that implements pgx.Rows for in-memory data.
type mockRows struct {
	pgx.Rows
	data [][]any
	pos  int
}

func (r *mockRows) Next() bool {
	r.pos++
	return r.pos <= len(r.data)
}

func (r *mockRows) Scan(dest ...any) error {
	if r.pos < 1 || r.pos > len(r.data) {
		return fmt.Errorf("no current row")
	}
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		src := row[i]
		switch dt := d.(type) {
		case *string:
			if src == nil {
				*dt = ""
			} else {
				*dt = src.(string)
			}
		case **string:
			if src == nil {
				*dt = nil
			} else if sp, ok := src.(*string); ok {
				*dt = sp
			} else {
				s := src.(string)
				*dt = &s
			}
		case *bool:
			*dt = src.(bool)
		case *[]byte:
			if src == nil {
				*dt = nil
			} else if b, ok := src.([]byte); ok {
				*dt = b
			} else if s, ok := src.(string); ok {
				*dt = []byte(s)
			}
		case *time.Time:
			if t, ok := src.(time.Time); ok {
				*dt = t
			}
		default:
			return fmt.Errorf("mockRows.Scan: unsupported dest type %T for column %d", d, i)
		}
	}
	return nil
}

func (r *mockRows) Close() {}

func (r *mockRows) Err() error { return nil }

// sqlContains checks whether a SQL string contains a substring.
func sqlContains(sql, substr string) bool {
	return strings.Contains(sql, substr)
}
