package memory

import (
	"context"
	"fmt"
	"sync"
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

// InMemoryStore is a simple in-memory implementation of Store.
// Useful for development, testing, and single-server deployments.
// For production, swap with a PostgreSQL-backed implementation.
type InMemoryStore struct {
	mu  sync.RWMutex
	buf map[string]map[string]*MemoryEntry // userID -> key -> entry
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		buf: make(map[string]map[string]*MemoryEntry),
	}
}

func (s *InMemoryStore) Set(_ context.Context, userID, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf[userID] == nil {
		s.buf[userID] = make(map[string]*MemoryEntry)
	}
	now := time.Now()
	if existing, ok := s.buf[userID][key]; ok {
		existing.Value = value
		existing.UpdatedAt = now
		return nil
	}
	s.buf[userID][key] = &MemoryEntry{
		Key:       key,
		Value:     value,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return nil
}

func (s *InMemoryStore) Get(_ context.Context, userID, key string) (*MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf[userID] == nil {
		return nil, nil
	}
	entry, ok := s.buf[userID][key]
	if !ok {
		return nil, nil
	}
	cp := *entry
	return &cp, nil
}

func (s *InMemoryStore) Delete(_ context.Context, userID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf[userID] != nil {
		delete(s.buf[userID], key)
	}
	return nil
}

func (s *InMemoryStore) Search(_ context.Context, userID, query string, limit int) ([]MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf[userID] == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	var results []MemoryEntry
	for _, entry := range s.buf[userID] {
		if len(results) >= limit {
			break
		}
		// Simple substring matching on both key and value.
		// In production this would use embedding similarity search.
		if match(entry.Key, query) || match(entry.Value, query) {
			results = append(results, *entry)
		}
	}
	return results, nil
}

func (s *InMemoryStore) List(_ context.Context, userID string) ([]MemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf[userID] == nil {
		return nil, nil
	}
	results := make([]MemoryEntry, 0, len(s.buf[userID]))
	for _, entry := range s.buf[userID] {
		results = append(results, *entry)
	}
	return results, nil
}

func match(text, query string) bool {
	return len(query) > 0 && containsFold(text, query)
}

func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return false
	}
	if len(s) < len(substr) {
		return false
	}
	delta := byte('a' - 'A')
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			sc := s[i+j]
			tc := substr[j]
			if sc == tc {
				continue
			}
			// case-insensitive comparison
			if sc >= 'A' && sc <= 'Z' && sc+delta == tc {
				continue
			}
			if sc >= 'a' && sc <= 'z' && sc-delta == tc {
				continue
			}
			match = false
			break
		}
		if match {
			return true
		}
	}
	return false
}

// Ensure interface compliance.
var _ Store = (*InMemoryStore)(nil)

// NotFoundError is returned when a memory entry is not found.
type NotFoundError struct {
	UserID string
	Key    string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("memory entry not found: user=%s key=%s", e.UserID, e.Key)
}
