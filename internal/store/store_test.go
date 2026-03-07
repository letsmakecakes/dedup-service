package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/yourorg/dedup-service/internal/store"
)

var ctx = context.Background()

func TestMemoryStore_FirstCallNotDuplicate(t *testing.T) {
	s := store.NewMemory()
	dup, err := s.IsDuplicate(ctx, "key:abc", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("first call should not be a duplicate")
	}
}

func TestMemoryStore_SecondCallIsDuplicate(t *testing.T) {
	s := store.NewMemory()
	s.IsDuplicate(ctx, "key:abc", 10*time.Second) // prime
	dup, err := s.IsDuplicate(ctx, "key:abc", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Error("second call with same key should be a duplicate")
	}
}

func TestMemoryStore_DifferentKeysAreIndependent(t *testing.T) {
	s := store.NewMemory()
	s.IsDuplicate(ctx, "key:aaa", 10*time.Second)

	dup, err := s.IsDuplicate(ctx, "key:bbb", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("different key should not be a duplicate")
	}
}

func TestMemoryStore_ExpiryAllowsReuse(t *testing.T) {
	s := store.NewMemory()
	s.IsDuplicate(ctx, "key:abc", 30*time.Millisecond)

	time.Sleep(60 * time.Millisecond)

	dup, err := s.IsDuplicate(ctx, "key:abc", 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Error("key should have expired and not be a duplicate anymore")
	}
}

func TestMemoryStore_ErrorPropagated(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable

	_, err := s.IsDuplicate(ctx, "key:abc", 10*time.Second)
	if err == nil {
		t.Fatal("expected error when store.Err is set")
	}
}

func TestMemoryStore_PingReturnsErr(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	if s.Ping(ctx) == nil {
		t.Error("Ping should return error when Err is set")
	}
}

func TestMemoryStore_PingOK(t *testing.T) {
	s := store.NewMemory()
	if err := s.Ping(ctx); err != nil {
		t.Errorf("Ping should return nil for healthy store, got %v", err)
	}
}

func TestMemoryStore_ConcurrentAccess(t *testing.T) {
	s := store.NewMemory()
	const goroutines = 50
	results := make(chan bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			dup, _ := s.IsDuplicate(ctx, "shared-key", 5*time.Second)
			results <- dup
		}()
	}

	allowed := 0
	for i := 0; i < goroutines; i++ {
		if !<-results {
			allowed++
		}
	}

	// Exactly one goroutine should have been allowed (first-write wins).
	if allowed != 1 {
		t.Errorf("expected exactly 1 allowed under concurrency, got %d", allowed)
	}
}
