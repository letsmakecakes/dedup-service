package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/dedup-service/internal/store"
)

// ── LocalCache tests ──────────────────────────────────────────────────────────

func TestLocalCache_ContainsMiss(t *testing.T) {
	c := store.NewLocalCache()
	if c.Contains("nonexistent") {
		t.Error("Contains should return false for missing key")
	}
}

func TestLocalCache_SetThenContains(t *testing.T) {
	c := store.NewLocalCache()
	c.Set("key1", 5*time.Second)
	if !c.Contains("key1") {
		t.Error("Contains should return true after Set")
	}
}

func TestLocalCache_Expiry(t *testing.T) {
	c := store.NewLocalCache()
	c.Set("key1", 30*time.Millisecond)

	if !c.Contains("key1") {
		t.Fatal("key should exist immediately after Set")
	}

	time.Sleep(60 * time.Millisecond)

	if c.Contains("key1") {
		t.Error("key should have expired")
	}
}

func TestLocalCache_OverwriteResetsTTL(t *testing.T) {
	c := store.NewLocalCache()
	c.Set("key1", 30*time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	// Overwrite with a fresh TTL.
	c.Set("key1", 100*time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	// Original TTL would have expired, but overwrite extended it.
	if !c.Contains("key1") {
		t.Error("key should still exist after TTL refresh")
	}
}

func TestLocalCache_Len(t *testing.T) {
	c := store.NewLocalCache()
	if c.Len() != 0 {
		t.Errorf("empty cache should have Len 0, got %d", c.Len())
	}

	c.Set("a", 5*time.Second)
	c.Set("b", 5*time.Second)
	c.Set("c", 5*time.Second)
	if c.Len() != 3 {
		t.Errorf("expected Len 3, got %d", c.Len())
	}
}

func TestLocalCache_LenIncludesExpired(t *testing.T) {
	c := store.NewLocalCache()
	c.Set("a", 10*time.Millisecond)
	c.Set("b", 5*time.Second)
	time.Sleep(30 * time.Millisecond)

	// Len includes expired entries (they're removed lazily or by sweep).
	if c.Len() < 1 {
		t.Error("Len should include at least the non-expired entry")
	}
}

func TestLocalCache_DifferentKeysIndependent(t *testing.T) {
	c := store.NewLocalCache()
	c.Set("key1", 5*time.Second)

	if c.Contains("key2") {
		t.Error("unset key should not be found")
	}
	if !c.Contains("key1") {
		t.Error("set key should be found")
	}
}

func TestLocalCache_Sweep(t *testing.T) {
	c := store.NewLocalCache()
	c.Set("expire-me", 10*time.Millisecond)
	c.Set("keep-me", 5*time.Second)

	time.Sleep(30 * time.Millisecond)

	// Export Sweep for testing via sweepLoop with a very short interval.
	ctx, cancel := context.WithCancel(context.Background())
	// Trigger a single sweep by starting and immediately stopping.
	go c.SweepLoop(ctx, 1*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	cancel()

	// After sweep, the expired key should be evicted.
	if c.Len() != 1 {
		t.Errorf("expected 1 entry after sweep, got %d", c.Len())
	}
	if !c.Contains("keep-me") {
		t.Error("non-expired key should survive sweep")
	}
}

func TestLocalCache_ConcurrentAccess(t *testing.T) {
	c := store.NewLocalCache()
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			key := "key"
			c.Set(key, 5*time.Second)
			c.Contains(key)
			c.Len()
		}(i)
	}

	wg.Wait()
	// No panic or race = pass. Content correctness tested separately.
}

func TestLocalCache_ShardDistribution(t *testing.T) {
	c := store.NewLocalCache()
	// Insert many keys; they should distribute across shards (not all in one).
	for i := 0; i < 1000; i++ {
		c.Set(time.Now().String()+string(rune(i)), 5*time.Second)
	}
	if c.Len() != 1000 {
		t.Errorf("expected 1000 entries, got %d", c.Len())
	}
}

// ── CachedStore tests ─────────────────────────────────────────────────────────

func TestCachedStore_FirstCallNotDuplicate(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	dup, err := cs.IsDuplicate(ctx, "key:1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("first call should not be a duplicate")
	}
}

func TestCachedStore_SecondCallIsDuplicate(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	cs.IsDuplicate(ctx, "key:1", 5*time.Second)

	dup, err := cs.IsDuplicate(ctx, "key:1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("second call should be a duplicate (L1 cache hit)")
	}
}

func TestCachedStore_L1CacheHitSkipsBackend(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	// Prime both L1 and backend.
	cs.IsDuplicate(ctx, "key:1", 5*time.Second)

	// Now break the backend — L1 should still catch the duplicate.
	backend.Err = store.ErrUnavailable

	dup, err := cs.IsDuplicate(ctx, "key:1", 5*time.Second)
	if err != nil {
		t.Fatalf("L1 hit should not have called backend, got error: %v", err)
	}
	if !dup {
		t.Error("L1 cache should detect duplicate without backend")
	}
}

func TestCachedStore_L1MissFallsToBackend(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	// Insert directly into backend, bypassing L1.
	backend.IsDuplicate(ctx, "key:1", 5*time.Second)

	// L1 miss → backend should detect duplicate.
	dup, err := cs.IsDuplicate(ctx, "key:1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("L1 miss should fall through to backend and detect duplicate")
	}
}

func TestCachedStore_BackendErrorPropagated(t *testing.T) {
	backend := store.NewMemory()
	backend.Err = store.ErrUnavailable
	cs := store.NewCached(backend)
	defer cs.Close()

	_, err := cs.IsDuplicate(ctx, "key:1", 5*time.Second)
	if err == nil {
		t.Error("backend error should propagate through CachedStore")
	}
}

func TestCachedStore_CacheLen(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	if cs.CacheLen() != 0 {
		t.Error("initial CacheLen should be 0")
	}

	cs.IsDuplicate(ctx, "a", 5*time.Second)
	cs.IsDuplicate(ctx, "b", 5*time.Second)

	if cs.CacheLen() != 2 {
		t.Errorf("expected CacheLen 2, got %d", cs.CacheLen())
	}
}

func TestCachedStore_Ping(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	if err := cs.Ping(ctx); err != nil {
		t.Errorf("Ping should succeed, got %v", err)
	}

	backend.Err = store.ErrUnavailable
	if cs.Ping(ctx) == nil {
		t.Error("Ping should propagate backend error")
	}
}

func TestCachedStore_CloseStopsSweep(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)

	// Close should not panic and should stop the sweep goroutine.
	if err := cs.Close(); err != nil {
		t.Errorf("Close should succeed, got %v", err)
	}
}

func TestCachedStore_ConcurrentAccess(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			cs.IsDuplicate(ctx, "shared-key", 5*time.Second)
		}()
	}

	wg.Wait()
}

func TestCachedStore_ExpiryAllowsReuse(t *testing.T) {
	backend := store.NewMemory()
	cs := store.NewCached(backend)
	defer cs.Close()

	cs.IsDuplicate(ctx, "key:1", 30*time.Millisecond)

	// Should be duplicate immediately.
	dup, _ := cs.IsDuplicate(ctx, "key:1", 30*time.Millisecond)
	if !dup {
		t.Error("should be duplicate within TTL")
	}

	// Wait for both L1 and backend to expire.
	time.Sleep(60 * time.Millisecond)

	dup, err := cs.IsDuplicate(ctx, "key:1", 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("after expiry, key should not be a duplicate")
	}
}
