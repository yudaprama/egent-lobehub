// Package lock implements a Redis-backed distributed edit lock that
// prevents two users from editing the same agent/document/task/chatGroup
// simultaneously. It is the Go replacement for the TypeScript
// EditLockService and keeps the same Redis key pattern
// (`editlock:{resourceType}:{resourceId}`) and 30s auto-expiry.
//
// The underlying primitive is go-redsync, which performs the same
// SET NX EX + compare-token release as the TS version.
//
// When Redis is not configured the lock is a no-op (fail-open), matching
// the TS behaviour where a downed Redis returns "unlocked".
package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/go-redsync/redsync/v4"
	redsyncgoredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"
)

// defaultTTL matches the TS EditLockService auto-expiry.
const defaultTTL = 30 * time.Second

// ResourceType enumerates the resources protected by an edit lock.
// It mirrors the TS union "agent" | "chatGroup" | "document" | "task".
type ResourceType string

const (
	ResourceAgent     ResourceType = "agent"
	ResourceChatGroup ResourceType = "chatGroup"
	ResourceDocument  ResourceType = "document"
	ResourceTask      ResourceType = "task"
)

// ErrLockHeld is returned by Acquire when another caller owns the lock.
// Callers should treat this as a non-error "blocked" outcome, not a
// hard failure — it matches the TS behaviour where acquire returns
// `null` (not an exception) when the resource is busy.
var ErrLockHeld = errors.New("lock: held by another caller")

// Locker is the contract returned by Acquire. redsync.Mutex satisfies
// it, as does noopLocker (for the fail-open path).
type Locker interface {
	Unlock() (bool, error)
}

// Mutex wraps redsync.Redsync with the editlock: prefix pattern used by
// the TS EditLockService. Construct one with New and share it across
// handlers; it is safe for concurrent use.
type Mutex struct {
	rs  *redsync.Redsync
	rdb goredis.UniversalClient
}

// New creates a Mutex from an existing Redis client.
// Pass nil to disable locking (calls become no-ops). This is used for
// local dev where Redis is not available and matches the TS fail-open
// behaviour.
func New(rdb goredis.UniversalClient) *Mutex {
	if rdb == nil {
		return &Mutex{}
	}
	pool := redsyncgoredis.NewPool(rdb)
	return &Mutex{rs: redsync.New(pool), rdb: rdb}
}

// Client returns the underlying Redis client (nil when disabled).
// Exposed so callers can reuse the connection for other operations.
func (m *Mutex) Client() goredis.UniversalClient {
	if m == nil {
		return nil
	}
	return m.rdb
}

// Enabled reports whether the lock is backed by Redis. When false,
// Acquire returns a no-op locker and the other accessors report
// "no holder".
func (m *Mutex) Enabled() bool {
	return m != nil && m.rs != nil
}

// lockKey builds the same key pattern used by the TS EditLockService.
func lockKey(typ ResourceType, id string) string {
	return fmt.Sprintf("editlock:%s:%s", string(typ), id)
}

// Acquire attempts to claim the lock for a single caller. It performs
// exactly one SET NX EX — no spin, no retry. The returned Locker must
// be Released when the caller is done.
//
// Return value semantics (intentionally match the TS service):
//   - (locker, nil): lock acquired; caller owns it until Release.
//   - (nil, nil):     lock held by someone else; not an error.
//   - (nil, err):     Redis or configuration error.
//
// On a non-acquire the redsync mutex is touched so the underlying token
// stays consistent; the no-op path is only taken when Enabled() is
// false.
func (m *Mutex) Acquire(typ ResourceType, id string) (Locker, error) {
	if !m.Enabled() {
		return noopLocker{}, nil
	}
	mutex := m.rs.NewMutex(
		lockKey(typ, id),
		redsync.WithExpiry(defaultTTL),
		redsync.WithTries(1),
	)
	if err := mutex.Lock(); err != nil {
		if isLockHeldErr(err) {
			return nil, nil // held by another caller — not an error
		}
		return nil, fmt.Errorf("lock: acquire %s:%s: %w", typ, id, err)
	}
	return mutex, nil
}

// MustAcquire acquires or panics. Use only in tests.
func (m *Mutex) MustAcquire(typ ResourceType, id string) Locker {
	l, err := m.Acquire(typ, id)
	if err != nil {
		panic(fmt.Sprintf("lock acquire error: %v", err))
	}
	if l == nil {
		panic(fmt.Sprintf("lock %s:%s held by another caller", typ, id))
	}
	return l
}

// Release undoes Acquire. It is safe to call on a no-op locker.
// Errors are ignored — a failed release simply means the lock will
// expire on its own at TTL.
func Release(l Locker) {
	if l == nil {
		return
	}
	if _, ok := l.(noopLocker); ok {
		return
	}
	// redsync ignores Unlock failures internally and always returns
	// (false, nil) when the lock already expired.
	_, _ = l.Unlock()
}

// noopLocker is returned when Redis is unavailable so the rest of the
// call path runs unchanged.
type noopLocker struct{}

func (noopLocker) Unlock() (bool, error) { return true, nil }

// isLockHeldErr reports whether err indicates the lock is currently
// held by another caller (as opposed to a Redis connectivity error).
// redsync returns one of three shapes in this case: ErrFailed (retry
// exhausted), ErrTaken (quorum reached on contention), or ErrNodeTaken
// (single node contention). All three should map to "blocked" rather
// than "error".
func isLockHeldErr(err error) bool {
	if errors.Is(err, redsync.ErrFailed) {
		return true
	}
	var taken *redsync.ErrTaken
	var nodeTaken *redsync.ErrNodeTaken
	return errors.As(err, &taken) || errors.As(err, &nodeTaken)
}

// --- Active-holder accessors ------------------------------------------------
//
// These mirror the read-side methods of the TS EditLockService. They
// are thin wrappers over the shared Redis client because redsync does
// not expose raw GET (the data it stores is a per-owner token, not the
// TS "holder payload"). When migrating off TS these methods let the
// Go service answer "who holds this lock?" while the TS service is
// still authoritative.

// HolderPayload is the JSON shape written by the TS EditLockService.
// Re-declared here (and not imported from a shared package) so the
// lock package has no dependency on the server schema.
type HolderPayload struct {
	OwnerID    string    `json:"ownerId"`
	UserName   string    `json:"userName,omitempty"`
	AcquiredAt time.Time `json:"acquiredAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// GetActiveHolder returns the ownerID of whoever currently holds the
// lock for (typ, id). Returns "" when no lock is held or the lock is
// disabled. It intentionally does NOT consult redsync (which stores a
// private token) — it reads the TS-written payload directly so the
// value is meaningful across the dual-stack migration.
func (m *Mutex) GetActiveHolder(ctx context.Context, typ ResourceType, id string) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	payload, err := m.getActiveLockRaw(ctx, typ, id)
	if err != nil {
		return "", err
	}
	if payload == nil {
		return "", nil
	}
	return payload.OwnerID, nil
}

// GetActiveLock returns the full holder payload, or nil when the lock
// is free (or disabled).
func (m *Mutex) GetActiveLock(ctx context.Context, typ ResourceType, id string) (*HolderPayload, error) {
	if !m.Enabled() {
		return nil, nil
	}
	return m.getActiveLockRaw(ctx, typ, id)
}

// GetBlockingHolder returns the ownerID of the current holder only when
// that holder is someone other than the supplied ownerID. This is used
// to answer "can this user write?" — a holder that matches the caller
// is not blocking.
func (m *Mutex) GetBlockingHolder(ctx context.Context, typ ResourceType, id, ownerID string) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	payload, err := m.getActiveLockRaw(ctx, typ, id)
	if err != nil {
		return "", err
	}
	if payload == nil {
		return "", nil
	}
	if payload.OwnerID == ownerID {
		return "", nil
	}
	return payload.OwnerID, nil
}

// CanWrite reports whether ownerID may edit (typ, id). True when no
// lock is held, when the caller owns the lock, or when locking is
// disabled. False only when a different user holds the lock.
func (m *Mutex) CanWrite(ctx context.Context, typ ResourceType, id, ownerID string) (bool, error) {
	blocker, err := m.GetBlockingHolder(ctx, typ, id, ownerID)
	if err != nil {
		return false, err
	}
	return blocker == "", nil
}

// getActiveLockRaw reads + JSON-decodes the holder payload written by
// the TS EditLockService. Returns (nil, nil) when the key does not
// exist.
func (m *Mutex) getActiveLockRaw(ctx context.Context, typ ResourceType, id string) (*HolderPayload, error) {
	raw, err := m.rdb.Get(ctx, lockKey(typ, id)).Bytes()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("lock: get %s:%s: %w", typ, id, err)
	}
	var p HolderPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("lock: decode payload %s:%s: %w", typ, id, err)
	}
	return &p, nil
}
