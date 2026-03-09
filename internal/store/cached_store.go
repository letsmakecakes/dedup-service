package store

import (
	"context"
	"time"

	"github.com/yourorg/dedup-service/internal/metrics"
)

// CachedStore wraps a Store with an in-process L1 cache that eliminates
// Redis round-trips for known duplicates.
//
// Flow:
//  1. Check L1 (local cache) — if hit → duplicate, no Redis call.
//  2. Miss → call backend.IsDuplicate (Redis SETNX).
//  3. Populate L1 with the key regardless of result.
//
// Safety:
//   - False positives: impossible — only keys confirmed by Redis are cached.
//   - False negatives: the backend catches them on L1 miss.
//   - Cross-instance: Redis remains the source of truth.
type CachedStore struct {
	backend Store
	cache   *LocalCache
	cancel  context.CancelFunc
}

// NewCached wraps backend with an L1 local cache. A background goroutine
// sweeps expired entries every 10 seconds. Call Close to stop it.
func NewCached(backend Store) *CachedStore {
	ctx, cancel := context.WithCancel(context.Background())
	cs := &CachedStore{
		backend: backend,
		cache:   NewLocalCache(),
		cancel:  cancel,
	}
	go cs.cache.SweepLoop(ctx, 10*time.Second)
	return cs
}

// IsDuplicate checks the L1 cache first, then falls through to Redis.
func (s *CachedStore) IsDuplicate(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	// L1 hit → known duplicate, skip Redis entirely.
	if s.cache.Contains(key) {
		metrics.CacheHitsTotal.Inc()
		return true, nil
	}
	metrics.CacheMissesTotal.Inc()

	// L2 — Redis SETNX.
	isDup, err := s.backend.IsDuplicate(ctx, key, ttl)
	if err != nil {
		return isDup, err
	}

	// Populate L1 for future lookups.
	s.cache.Set(key, ttl)
	return isDup, nil
}

// Ping delegates to the backend store.
func (s *CachedStore) Ping(ctx context.Context) error {
	return s.backend.Ping(ctx)
}

// Close stops the background sweep goroutine and closes the backend store.
func (s *CachedStore) Close() error {
	s.cancel()
	return s.backend.Close()
}

// CacheLen returns the number of entries in the L1 cache.
func (s *CachedStore) CacheLen() int {
	return s.cache.Len()
}
