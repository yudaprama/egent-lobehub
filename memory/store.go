package memory

import (
	"context"
	"fmt"
	"time"
)

// MemoryEntry represents a single stored memory fact.
type MemoryEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store is the memory persistence interface.
// Maps to LobeHub's userMemory service: agents read/write user
// preferences, facts, and context during conversations.
type Store interface {
	// Set stores a memory entry for a user.
	Set(ctx context.Context, userID, key, value string) error
	// Get retrieves a specific memory entry.
	Get(ctx context.Context, userID, key string) (*MemoryEntry, error)
	// Delete removes a memory entry.
	Delete(ctx context.Context, userID, key string) error
	// Search finds memory entries matching a query.
	Search(ctx context.Context, userID, query string, limit int) ([]MemoryEntry, error)
	// List returns all memory entries for a user.
	List(ctx context.Context, userID string) ([]MemoryEntry, error)
}



// NotFoundError is returned when a memory entry is not found.
type NotFoundError struct {
	UserID string
	Key    string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("memory entry not found: user=%s key=%s", e.UserID, e.Key)
}
