package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLocalCache_SetAndContains(t *testing.T) {
	c := NewLocalCache()
	c.Set("key1", 5*time.Second)

	if !c.Contains("key1") {
		t.Error("expected key1 to be in cache")
	}
	if c.Contains("key2") {
		t.Error("expected key2 to NOT be in cache")
	}
}

func TestLocalCache_Expiry(t *testing.T) {
	c := NewLocalCache()
	c.Set("key1", 30*time.Millisecond)

	if !c.Contains("key1") {
		t.Fatal("expected key1 present before expiry")
	}

	time.Sleep(60 * time.Millisecond)

	if c.Contains("key1") {
		t.Error("expected key1 to have expired")
	}
}

func TestLocalCache_LazyEviction(t *testing.T) {
	c := NewLocalCache()
	c.Set("key1", 30*time.Millisecond)

	if c.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", c.Len())
	}

	time.Sleep(60 * time.Millisecond)

	// Contains triggers lazy eviction.
	c.Contains("key1")

	if c.Len() != 0 {
		t.Errorf("expected Len=0 after lazy eviction, got %d", c.Len())
	}
}

func TestLocalCache_Sweep(t *testing.T) {
	c := NewLocalCache()
	for i := 0; i < 100; i++ {
		c.Set(fmt.Sprintf("k-%d", i), 30*time.Millisecond)
	}
	if c.Len() != 100 {
		t.Fatalf("expected 100 entries, got %d", c.Len())
	}

	time.Sleep(60 * time.Millisecond)
	c.sweep()

	if c.Len() != 0 {
		t.Errorf("expected 0 entries after sweep, got %d", c.Len())
	}
}

func TestLocalCache_SweepLoop(t *testing.T) {
	c := NewLocalCache()
	c.Set("key1", 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go c.sweepLoop(ctx, 30*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	cancel()

	if c.Len() != 0 {
		t.Errorf("expected sweep loop to evict expired entry, got Len=%d", c.Len())
	}
}

func TestLocalCache_Overwrite(t *testing.T) {
	c := NewLocalCache()
	c.Set("key1", 30*time.Millisecond)
	c.Set("key1", 5*time.Second) // extend TTL

	time.Sleep(60 * time.Millisecond)

	if !c.Contains("key1") {
		t.Error("expected key1 present after TTL extension")
	}
}

func TestLocalCache_ShardDistribution(t *testing.T) {
	c := NewLocalCache()
	// Insert 1000 distinct keys and verify they land in multiple shards.
	for i := 0; i < 1000; i++ {
		c.Set(fmt.Sprintf("dedup:%064d", i), time.Hour)
	}
	usedShards := 0
	for i := range c.shards {
		c.shards[i].mu.RLock()
		if len(c.shards[i].data) > 0 {
			usedShards++
		}
		c.shards[i].mu.RUnlock()
	}
	// With 1000 keys across 256 shards, at least 100 shards should have entries.
	if usedShards < 100 {
		t.Errorf("poor shard distribution: only %d/256 shards used for 1000 keys", usedShards)
	}
}

func TestLocalCache_ConcurrentAccess(t *testing.T) {
	c := NewLocalCache()
	const goroutines = 100
	const opsPerGoroutine = 500

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				key := fmt.Sprintf("g%d-k%d", id, i)
				c.Set(key, time.Second)
				c.Contains(key)
			}
		}(g)
	}
	wg.Wait()

	if c.Len() != goroutines*opsPerGoroutine {
		t.Errorf("expected %d entries, got %d", goroutines*opsPerGoroutine, c.Len())
	}
}

func TestLocalCache_Len(t *testing.T) {
	c := NewLocalCache()
	if c.Len() != 0 {
		t.Fatalf("expected empty cache, got Len=%d", c.Len())
	}
	c.Set("a", time.Hour)
	c.Set("b", time.Hour)
	c.Set("c", time.Hour)
	if c.Len() != 3 {
		t.Errorf("expected Len=3, got %d", c.Len())
	}
}
