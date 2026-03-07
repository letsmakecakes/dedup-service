// Package store — localcache.go provides a sharded in-process L1 cache
// for high-frequency deduplication lookups.
//
// The cache uses 256 independently-locked shards with FNV-1a distribution
// to minimise lock contention under high concurrency. Entries expire lazily
// on read and are periodically swept by a background goroutine.
package store

import (
	"context"
	"sync"
	"time"
)

const numShards = 256

// LocalCache is a sharded in-process cache providing O(1) duplicate lookups
// without a Redis round-trip.
type LocalCache struct {
	shards [numShards]cacheShard
}

type cacheShard struct {
	mu   sync.RWMutex
	data map[string]int64 // key → expiry (UnixNano)
}

// NewLocalCache returns an initialised LocalCache ready for concurrent use.
func NewLocalCache() *LocalCache {
	c := &LocalCache{}
	for i := range c.shards {
		c.shards[i].data = make(map[string]int64, 256)
	}
	return c
}

// Contains reports whether key exists in the cache and has not expired.
func (c *LocalCache) Contains(key string) bool {
	idx := c.shardIndex(key)
	shard := &c.shards[idx]
	now := time.Now().UnixNano()

	shard.mu.RLock()
	expiry, ok := shard.data[key]
	shard.mu.RUnlock()

	if !ok {
		return false
	}
	if now >= expiry {
		// Lazy eviction of expired entry.
		shard.mu.Lock()
		if exp, still := shard.data[key]; still && time.Now().UnixNano() >= exp {
			delete(shard.data, key)
		}
		shard.mu.Unlock()
		return false
	}
	return true
}

// Set stores key with the given TTL, overwriting any existing entry.
func (c *LocalCache) Set(key string, ttl time.Duration) {
	idx := c.shardIndex(key)
	shard := &c.shards[idx]
	expiry := time.Now().Add(ttl).UnixNano()

	shard.mu.Lock()
	shard.data[key] = expiry
	shard.mu.Unlock()
}

// Len returns the total number of entries across all shards (including expired).
func (c *LocalCache) Len() int {
	n := 0
	for i := range c.shards {
		c.shards[i].mu.RLock()
		n += len(c.shards[i].data)
		c.shards[i].mu.RUnlock()
	}
	return n
}

// sweepLoop periodically evicts expired entries. Stops when ctx is cancelled.
func (c *LocalCache) sweepLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep()
		}
	}
}

// sweep removes all expired entries from every shard.
func (c *LocalCache) sweep() {
	now := time.Now().UnixNano()
	for i := range c.shards {
		shard := &c.shards[i]
		shard.mu.Lock()
		for k, exp := range shard.data {
			if now >= exp {
				delete(shard.data, k)
			}
		}
		shard.mu.Unlock()
	}
}

// shardIndex returns the shard index for key using FNV-1a.
func (c *LocalCache) shardIndex(key string) uint8 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return uint8(h & 0xFF) // #nosec G115 -- intentional modulo-256 shard index
}
