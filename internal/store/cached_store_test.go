package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCachedStore_FirstCallNotDuplicate(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	defer cs.Close()

	dup, err := cs.IsDuplicate(context.Background(), "key:1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("first call should not be a duplicate")
	}
}

func TestCachedStore_SecondCallIsDuplicate(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	defer cs.Close()

	cs.IsDuplicate(context.Background(), "key:1", 10*time.Second)
	dup, err := cs.IsDuplicate(context.Background(), "key:1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("second call should be a duplicate")
	}
}

func TestCachedStore_L1CacheHit(t *testing.T) {
	// Track whether backend is called on subsequent requests.
	backend := &callCounter{Store: NewMemory()}
	cs := NewCached(backend)
	defer cs.Close()

	cs.IsDuplicate(context.Background(), "key:1", 10*time.Second) // miss → backend
	cs.IsDuplicate(context.Background(), "key:1", 10*time.Second) // L1 hit

	if backend.calls != 1 {
		t.Errorf("expected backend called once (L1 hit on 2nd), got %d calls", backend.calls)
	}
}

func TestCachedStore_DifferentKeysIndependent(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	defer cs.Close()

	cs.IsDuplicate(context.Background(), "key:a", 10*time.Second)
	dup, err := cs.IsDuplicate(context.Background(), "key:b", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("different keys should not be duplicates")
	}
}

func TestCachedStore_TTLExpiry(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	defer cs.Close()

	cs.IsDuplicate(context.Background(), "key:1", 30*time.Millisecond)

	time.Sleep(60 * time.Millisecond)

	dup, err := cs.IsDuplicate(context.Background(), "key:1", 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("key should have expired in both L1 and backend")
	}
}

func TestCachedStore_BackendErrorPropagated(t *testing.T) {
	backend := NewMemory()
	backend.Err = ErrUnavailable
	cs := NewCached(backend)
	defer cs.Close()

	_, err := cs.IsDuplicate(context.Background(), "key:1", 10*time.Second)
	if err == nil {
		t.Fatal("expected error from backend")
	}
}

func TestCachedStore_Ping(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	defer cs.Close()

	if err := cs.Ping(context.Background()); err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	backend.Err = ErrUnavailable
	if err := cs.Ping(context.Background()); err == nil {
		t.Error("expected error when backend is unavailable")
	}
}

func TestCachedStore_Close(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	if err := cs.Close(); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestCachedStore_ConcurrentAccess(t *testing.T) {
	backend := NewMemory()
	cs := NewCached(backend)
	defer cs.Close()

	const goroutines = 50
	const keys = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < keys; i++ {
				key := fmt.Sprintf("g%d-k%d", id, i)
				cs.IsDuplicate(context.Background(), key, time.Second)
			}
		}(g)
	}
	wg.Wait()
}

// callCounter wraps a Store and counts IsDuplicate calls.
type callCounter struct {
	Store
	mu    sync.Mutex
	calls int
}

func (c *callCounter) IsDuplicate(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Store.IsDuplicate(ctx, key, ttl)
}
