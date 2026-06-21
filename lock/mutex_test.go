package lock

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func newTestMutex(t *testing.T) (*Mutex, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb), mr
}

func TestNewNilClient(t *testing.T) {
	m := New(nil)
	if m.Enabled() {
		t.Fatal("nil client should produce disabled mutex")
	}
	l, err := m.Acquire(ResourceAgent, "abc")
	if err != nil {
		t.Fatalf("acquire on disabled mutex: %v", err)
	}
	if l == nil {
		t.Fatal("disabled mutex should return noop locker, not nil")
	}
	Release(l) // must not panic
}

func TestAcquireRelease(t *testing.T) {
	m, _ := newTestMutex(t)

	l, err := m.Acquire(ResourceAgent, "agent-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if l == nil {
		t.Fatal("expected locker, got nil")
	}
	Release(l)

	// After release, a second acquire should succeed immediately.
	l2, err := m.Acquire(ResourceAgent, "agent-1")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if l2 == nil {
		t.Fatal("expected locker after release, got nil")
	}
	Release(l2)
}

func TestAcquireBlockedWhenHeld(t *testing.T) {
	m, _ := newTestMutex(t)

	l, err := m.Acquire(ResourceDocument, "doc-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if l == nil {
		t.Fatal("expected first locker")
	}
	defer Release(l)

	// Second caller — different resource id, should succeed.
	l2, err := m.Acquire(ResourceDocument, "doc-2")
	if err != nil {
		t.Fatalf("second resource acquire: %v", err)
	}
	if l2 == nil {
		t.Fatal("different resource should be acquirable")
	}
	Release(l2)

	// Same resource — should be blocked (nil locker, nil error).
	l3, err := m.Acquire(ResourceDocument, "doc-1")
	if err != nil {
		t.Fatalf("blocked acquire returned error: %v", err)
	}
	if l3 != nil {
		t.Fatal("expected nil locker when held by another caller")
	}
}

func TestMustAcquirePanicsWhenHeld(t *testing.T) {
	m, _ := newTestMutex(t)

	l := m.MustAcquire(ResourceTask, "task-1")
	defer Release(l)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on MustAcquire when held")
		}
	}()
	m.MustAcquire(ResourceTask, "task-1")
}

func TestResourceTypes(t *testing.T) {
	for _, rt := range []ResourceType{ResourceAgent, ResourceChatGroup, ResourceDocument, ResourceTask} {
		if lockKey(rt, "x") != "editlock:"+string(rt)+":x" {
			t.Errorf("unexpected key for %s", rt)
		}
	}
}

func TestTTLExpiry(t *testing.T) {
	m, mr := newTestMutex(t)

	l, err := m.Acquire(ResourceAgent, "expiring")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if l == nil {
		t.Fatal("expected locker")
	}
	// Do NOT release — let it expire.

	// Fast-forward past the 30s TTL.
	mr.FastForward(defaultTTL + time.Second)

	// Now the lock should be expired and a new acquire should succeed.
	l2, err := m.Acquire(ResourceAgent, "expiring")
	if err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	if l2 == nil {
		t.Fatal("expected locker after TTL expiry")
	}
	Release(l2)
}

// --- Holder accessor tests --------------------------------------------------

func TestGetActiveHolderEmpty(t *testing.T) {
	m, _ := newTestMutex(t)
	ctx := context.Background()

	holder, err := m.GetActiveHolder(ctx, ResourceAgent, "none")
	if err != nil {
		t.Fatalf("GetActiveHolder: %v", err)
	}
	if holder != "" {
		t.Errorf("expected empty holder, got %q", holder)
	}
}

func TestGetActiveHolderFromTSPayload(t *testing.T) {
	m, _ := newTestMutex(t)
	ctx := context.Background()

	// Simulate a TS EditLockService write: JSON payload at editlock:...
	payload := HolderPayload{
		OwnerID:    "user-42",
		UserName:   "alice",
		AcquiredAt: time.Now().UTC().Truncate(time.Second),
		ExpiresAt:  time.Now().Add(30 * time.Second).UTC().Truncate(time.Second),
	}
	raw, _ := json.Marshal(payload)
	if err := m.Client().Set(ctx, lockKey(ResourceAgent, "a1"), raw, 30*time.Second).Err(); err != nil {
		t.Fatalf("seed payload: %v", err)
	}

	holder, err := m.GetActiveHolder(ctx, ResourceAgent, "a1")
	if err != nil {
		t.Fatalf("GetActiveHolder: %v", err)
	}
	if holder != "user-42" {
		t.Errorf("expected user-42, got %q", holder)
	}

	full, err := m.GetActiveLock(ctx, ResourceAgent, "a1")
	if err != nil {
		t.Fatalf("GetActiveLock: %v", err)
	}
	if full.OwnerID != "user-42" || full.UserName != "alice" {
		t.Errorf("unexpected payload: %+v", full)
	}
}

func TestGetBlockingHolder(t *testing.T) {
	m, _ := newTestMutex(t)
	ctx := context.Background()

	payload := HolderPayload{OwnerID: "user-1"}
	raw, _ := json.Marshal(payload)
	_ = m.Client().Set(ctx, lockKey(ResourceDocument, "d1"), raw, 30*time.Second).Err()

	// Same owner → not blocking.
	b, err := m.GetBlockingHolder(ctx, ResourceDocument, "d1", "user-1")
	if err != nil {
		t.Fatalf("GetBlockingHolder same: %v", err)
	}
	if b != "" {
		t.Errorf("same owner should not be blocking, got %q", b)
	}

	// Different owner → blocking.
	b, err = m.GetBlockingHolder(ctx, ResourceDocument, "d1", "user-2")
	if err != nil {
		t.Fatalf("GetBlockingHolder other: %v", err)
	}
	if b != "user-1" {
		t.Errorf("expected user-1 as blocker, got %q", b)
	}
}

func TestCanWrite(t *testing.T) {
	m, _ := newTestMutex(t)
	ctx := context.Background()

	// No lock → can write.
	ok, err := m.CanWrite(ctx, ResourceTask, "t1", "user-1")
	if err != nil {
		t.Fatalf("CanWrite empty: %v", err)
	}
	if !ok {
		t.Error("expected CanWrite=true when no lock held")
	}

	// Held by someone else → cannot write.
	payload := HolderPayload{OwnerID: "user-2"}
	raw, _ := json.Marshal(payload)
	_ = m.Client().Set(ctx, lockKey(ResourceTask, "t1"), raw, 30*time.Second).Err()
	ok, err = m.CanWrite(ctx, ResourceTask, "t1", "user-1")
	if err != nil {
		t.Fatalf("CanWrite blocked: %v", err)
	}
	if ok {
		t.Error("expected CanWrite=false when held by another user")
	}

	// Held by same user → can write.
	ok, err = m.CanWrite(ctx, ResourceTask, "t1", "user-2")
	if err != nil {
		t.Fatalf("CanWrite same owner: %v", err)
	}
	if !ok {
		t.Error("expected CanWrite=true when held by same user")
	}
}

func TestDisabledAccessorsReturnZero(t *testing.T) {
	m := New(nil)
	ctx := context.Background()

	holder, err := m.GetActiveHolder(ctx, ResourceAgent, "x")
	if err != nil || holder != "" {
		t.Errorf("disabled GetActiveHolder: holder=%q err=%v", holder, err)
	}
	full, err := m.GetActiveLock(ctx, ResourceAgent, "x")
	if err != nil || full != nil {
		t.Errorf("disabled GetActiveLock: full=%v err=%v", full, err)
	}
	b, err := m.GetBlockingHolder(ctx, ResourceAgent, "x", "y")
	if err != nil || b != "" {
		t.Errorf("disabled GetBlockingHolder: b=%q err=%v", b, err)
	}
	ok, err := m.CanWrite(ctx, ResourceAgent, "x", "y")
	if err != nil || !ok {
		t.Errorf("disabled CanWrite: ok=%v err=%v", ok, err)
	}
}

func TestReleaseNilSafe(t *testing.T) {
	Release(nil)             // must not panic
	Release(noopLocker{})    // must not panic
}
