// Package store implements the fingerprint persistence layer.
//
// The Store interface is the only dependency that handler and main.go care about.
// Two implementations are provided:
//   - RedisStore: production, backed by Redis SETNX
//   - MemoryStore: in-process, for unit tests only (not safe for multi-instance deployments)
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yourorg/dedup-service/internal/config"
)

// ErrUnavailable is returned when the backing store cannot be reached.
// Handlers use errors.Is(err, ErrUnavailable) to apply fail-open/closed logic.
var ErrUnavailable = errors.New("store unavailable")

// Store is the deduplication persistence interface.
type Store interface {
	// IsDuplicate atomically checks whether key exists in the store.
	//   - If the key does not exist: create it with the given TTL and return false (not a duplicate).
	//   - If the key already exists: return true (duplicate detected).
	//   - If the store is unreachable: return ErrUnavailable so the caller can fail-open or fail-closed.
	IsDuplicate(ctx context.Context, key string, ttl time.Duration) (bool, error)

	// Ping verifies that the store is reachable. Used by /healthz.
	Ping(ctx context.Context) error

	// Close releases any resources held by the store.
	Close() error
}

// ── Redis implementation ───────────────────────────────────────────────────────

// RedisStore implements Store using Redis SETNX for atomic, distributed deduplication.
// It is safe for use across multiple service instances sharing the same Redis.
type RedisStore struct {
	client *redis.Client
}

// NewRedis constructs a RedisStore from cfg and verifies connectivity with an
// initial Ping. Returns an error if the Ping fails.
func NewRedis(cfg *config.Config) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  cfg.RedisDialTimeout,
		ReadTimeout:  cfg.RedisReadTimeout,
		WriteTimeout: cfg.RedisWriteTimeout,
		PoolSize:     cfg.RedisPoolSize,
		MinIdleConns: cfg.RedisMinIdle,
	})

	ctx, cancel := context.WithTimeout(context.Background(), cfg.RedisDialTimeout)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &RedisStore{client: client}, nil
}

// IsDuplicate executes SET key 1 NX PX <ttl-ms>.
//
// Redis semantics:
//   - SetNX returns true  → key was absent and has now been set → first occurrence → NOT a duplicate.
//   - SetNX returns false → key already existed                 → duplicate detected.
//
// Any Redis error is wrapped as ErrUnavailable so the caller can apply fail-open logic.
func (s *RedisStore) IsDuplicate(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	set, err := s.client.SetNX(ctx, key, 1, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	// set=true  → we just created the key → not a duplicate
	// set=false → key existed             → duplicate
	return !set, nil
}

// Ping checks Redis connectivity.
func (s *RedisStore) Ping(ctx context.Context) error {
	if err := s.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	return nil
}

// Close shuts down the underlying Redis client and releases pooled connections.
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// ── In-memory store (unit tests only) ─────────────────────────────────────────

// entry holds a cached fingerprint and its wall-clock expiry time.
type entry struct {
	expiresAt time.Time
}

// MemoryStore is a goroutine-safe in-process store for unit testing.
//
// WARNING: This store is intentionally single-process. It must NOT be used in
// production deployments where multiple service instances run in parallel, as
// each instance has its own independent map and will not detect cross-instance
// duplicates.
//
// Set Err to a non-nil value to simulate store unavailability in tests.
type MemoryStore struct {
	mu   sync.Mutex
	data map[string]entry
	Err  error // if non-nil, all operations return this error
}

// NewMemory returns an initialised MemoryStore.
func NewMemory() *MemoryStore {
	return &MemoryStore{data: make(map[string]entry)}
}

// IsDuplicate performs a check-and-set against the in-memory map.
// Only the requested key is checked for expiry (O(1) per call).
func (m *MemoryStore) IsDuplicate(_ context.Context, key string, ttl time.Duration) (bool, error) {
	if m.Err != nil {
		return false, m.Err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	if e, exists := m.data[key]; exists {
		if now.After(e.expiresAt) {
			// Entry expired — evict and treat as new.
			delete(m.data, key)
		} else {
			return true, nil // duplicate
		}
	}

	m.data[key] = entry{expiresAt: now.Add(ttl)}
	return false, nil
}

// Ping returns m.Err, allowing tests to simulate unavailability.
func (m *MemoryStore) Ping(_ context.Context) error { return m.Err }

// Close is a no-op for the in-memory store.
func (m *MemoryStore) Close() error { return nil }
