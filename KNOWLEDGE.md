# Dedup Service — Complete Working Knowledge

> **Document version**: March 2026
> **Go version**: 1.26.0 · **Framework**: Gin v1.12.0 · **Module**: `github.com/yourorg/dedup-service`

---

## Table of Contents

1. [What It Does](#1-what-it-does)
2. [Architecture Overview](#2-architecture-overview)
3. [Operating Modes](#3-operating-modes)
4. [Fingerprinting Algorithm](#4-fingerprinting-algorithm)
5. [Two-Tier Caching](#5-two-tier-caching)
6. [Request Flow (X-Accel-Redirect Mode)](#6-request-flow-x-accel-redirect-mode)
7. [Configuration Reference](#7-configuration-reference)
8. [API Reference](#8-api-reference)
9. [Nginx Integration](#9-nginx-integration)
10. [Prometheus Metrics](#10-prometheus-metrics)
11. [Grafana Dashboard](#11-grafana-dashboard)
12. [Project Structure](#12-project-structure)
13. [Code Walkthrough](#13-code-walkthrough)
14. [Performance Optimizations](#14-performance-optimizations)
15. [Load Test Results](#15-load-test-results)
16. [Running Locally](#16-running-locally)
17. [Docker Deployment](#17-docker-deployment)
18. [Testing](#18-testing)
19. [Makefile Targets](#19-makefile-targets)
20. [Troubleshooting](#20-troubleshooting)
21. [Design Decisions & Trade-offs](#21-design-decisions--trade-offs)
22. [X-Accel-Redirect — How It Works Under the Hood](#22-x-accel-redirect--how-it-works-under-the-hood)
23. [Capacity Planning & Redis Sizing](#23-capacity-planning--redis-sizing)

---

## 1. What It Does

The dedup service is a lightweight sidecar that sits in front of your API (via Nginx) and **prevents duplicate requests** from reaching your backend. When a client sends the same POST twice within a configurable time window (default: 10 seconds), the second request is rejected with `409 Conflict`.

**Example**: A payment API receives `POST /api/pay {"amount":100,"ref":"abc"}`. If the same request arrives again within 10 seconds — whether from a retry, double-click, or network glitch — the dedup service blocks it before it ever reaches your backend.

**Key properties**:
- Fingerprints are computed from **method + URI + request body** (no client identity)
- Uses **Redis SET NX** for distributed, atomic dedup across multiple service instances
- Supports **fail-open** mode: if Redis goes down, requests are allowed through (availability over correctness)
- GET, HEAD, and OPTIONS are excluded from dedup by default

---

## 2. Architecture Overview

```
                       ┌─────────────────────────────────────────────────────┐
                       │                    Nginx                           │
                       │                                                     │
  Client ──POST───────►│ location / {                                        │
                       │   proxy_pass http://dedup_service;                  │
                       │ }                                                   │
                       │                                                     │
                       │ location /internal/upstream {  ◄── X-Accel-Redirect │
                       │   internal;                                         │
                       │   proxy_method $original_method;                    │
                       │   proxy_pass http://backend;                        │
                       │ }                                                   │
                       └────────────┬───────────────────────────┬────────────┘
                                    │                           │
                                    ▼                           ▼
                       ┌────────────────────┐       ┌───────────────────┐
                       │   Dedup Service    │       │   Your Backend    │
                       │   (:8081)          │       │   (:9000)         │
                       │                    │       │                   │
                       │ ┌────────────────┐ │       │  Receives only    │
                       │ │  L1 LocalCache │ │       │  non-duplicate    │
                       │ │  (256 shards)  │ │       │  requests         │
                       │ └───────┬────────┘ │       │                   │
                       │         │ miss     │       └───────────────────┘
                       │         ▼          │
                       │ ┌────────────────┐ │
                       │ │  L2 Redis      │ │
                       │ │  SET key NX PX │ │
                       │ └────────────────┘ │
                       └────────────────────┘
```

---

## 3. Operating Modes

The service supports three modes, selected via config/environment variables:

### 3.1 X-Accel-Redirect Mode (Recommended)

**Set in config.json**: `"proxy": { "x_accel_redirect_prefix": "/internal/upstream" }`

Nginx sends the full client request (including body) to the dedup service. The service fingerprints method + URI + body and returns:
- **200** + `X-Accel-Redirect: /internal/upstream{URI}` → Nginx internally redirects to the backend
- **409** → Nginx returns the duplicate error to the client

**Advantages**: Full body available for fingerprinting. Nginx remains the router. Works for any URI dynamically — no per-route config needed.

### 3.2 Reverse Proxy Mode

**Set in config.json**: `"proxy": { "upstream_url": "http://localhost:9000" }`

The dedup service acts as a reverse proxy. Requests pass through dedup, and allowed ones are forwarded to the upstream. No Nginx required. The client's URI path is preserved, but the **destination host is fixed** to the configured upstream URL — all traffic goes to the same backend.

### 3.3 Sidecar / Auth-Request Mode (Legacy)

**Set in config.json**: Leave both `proxy.x_accel_redirect_prefix` and `proxy.upstream_url` empty (default)

Nginx uses `auth_request` to call `POST /dedup-check`. The body is **NOT** available (Nginx limitation), so fingerprinting covers method + URI only.

**Routes registered**: `POST /dedup-check` and `GET /dedup-check` (for Nginx auth_request sub-requests).

### Mode Priority

```
XAccelRedirectPrefix set?  → X-Accel-Redirect mode
         ↓ no
UpstreamURL set?           → Reverse proxy mode
         ↓ no
                           → Sidecar mode
```

---

## 4. Fingerprinting Algorithm

**Formula**: `Redis key = "dedup:" + hex(SHA-256(method | URI | body[:64KB]))`

```
SHA-256(
    "POST"                                  ← HTTP method
    "/api/orders?ref=abc"                   ← Full URI with query string
    {"id":"order-123","amount":100}         ← Request body (first 64KB)
)
→ "dedup:a3f2b8c1d9e0..."  (70-byte Redis key)
```

**What's included**:
- HTTP method (POST, PUT, DELETE, PATCH)
- Full URI with query parameters
- Request body (up to `max_body_bytes`, default 64 KB)

**What's intentionally excluded**:
- Client IP address
- Authorization / session headers
- Any other headers

**Why**: Deduplication targets the *resource operation*, not the caller. If two different users submit identical payment requests, that's still a duplicate. Per-caller isolation should be handled at the authorization layer, not the dedup layer.

**Performance**: The fingerprint uses `sync.Pool` for both SHA-256 hash objects and 64 KB body buffers, plus `unsafe` zero-copy string hashing. Result: ~0 allocations per request on the hot path.

---

## 5. Two-Tier Caching

### L1: LocalCache (In-Process)

- **256 shards** with FNV-1a hash distribution
- Per-shard `sync.RWMutex` — minimal lock contention at high concurrency
- Stores `key → expiry_timestamp_nanoseconds`
- Lazy eviction on read + periodic sweep every 10 seconds
- ~100ns lookup time

### L2: Redis (Distributed)

- `SET key 1 NX PX <ttl_ms>` — atomic set-if-not-exists with TTL
- Redis is the **source of truth** for cross-instance consistency
- Connection pool: 100 connections, 20 min idle

### Lookup Flow

```
Request arrives
    │
    ▼
L1 cache.Contains(key)?
    ├── YES → return duplicate (cache hit, no Redis call)
    └── NO  → L2 Redis SET NX
                ├── OK (key created) → not duplicate, cache.Set(key, ttl)
                └── Nil (key exists) → duplicate, cache.Set(key, ttl)
```

**Guarantees**:
- No false positives: L1 only caches Redis-confirmed results
- False negatives are caught: L1 miss falls through to Redis
- Cross-instance: Redis ensures consistency even with multiple service instances

### Enabling/Disabling

- **Enabled by default**: `performance.local_cache: true` in config.json
- Set to `false` to bypass L1 and always hit Redis

---

## 6. Request Flow (X-Accel-Redirect Mode)

Step-by-step for `POST /api/orders {"id":"123","amount":100}`:

```
1. Client → Nginx (:80)
2. Nginx: set $original_method = POST
3. Nginx: proxy_pass → dedup-service (:8081)
4. Dedup service:
   a. Middleware: assign X-Request-ID, start timer
   b. XAccelDedupHandler.Handle():
      - POST not in ExcludeMethods → proceed
      - Read body from pooled 64KB buffer
      - Compute: SHA-256("POST" | "/api/orders" | body) → key
      - L1 cache miss → Redis: SET dedup:a3f2... 1 NX PX 10000
      - Redis returns OK → first occurrence
      - Return 200 + X-Accel-Redirect: /internal/upstream/api/orders
   c. Middleware: log request, record metrics
5. Nginx: receives X-Accel-Redirect header
6. Nginx: internal redirect to /internal/upstream/api/orders
   - Strips /internal/upstream prefix → /api/orders
   - Restores proxy_method = POST (from $original_method)
   - proxy_pass → backend (:9000)
7. Backend: receives POST /api/orders with original body
8. Backend → response → Nginx → Client

If same request arrives within 10s:
   Step 4 → L1 cache hit (or Redis returns Nil) → return 409 Conflict
   Client receives: {"error":"duplicate_request","details":"..."}
```

---

## 7. Configuration Reference

Configuration is loaded exclusively from `config.json` (via Viper). There are **no environment variable overrides** — all settings must be in the JSON file. If the file is missing, hardcoded defaults are used.

Two config files are provided:
- `config.json` — local development (Redis at `localhost:6379`, sidecar mode)
- `config.docker.json` — Docker/K8s (Redis at `redis:6379`, X-Accel mode with `/internal/upstream`)

### Server

| Field | Default | Description |
|-------|---------|-------------|
| `server.listen_addr` | `:8081` | HTTP bind address |
| `server.log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `server.shutdown_timeout` | `10s` | Graceful shutdown drain period |

### Logging

| Field | Default | Description |
|-------|---------|-------------|
| `log.file` | `log/app.log` | Log file path |
| `log.max_size_mb` | `50` | Max size before rotation |
| `log.max_backups` | `5` | Old log files to keep |
| `log.max_age_days` | `30` | Days to retain old logs |
| `log.compress` | `true` | Gzip rotated files |

### Redis

| Field | Default | Description |
|-------|---------|-------------|
| `redis.addr` | `localhost:6379` | Redis host:port |
| `redis.password` | *(empty)* | Redis auth password |
| `redis.db` | `0` | Logical database (0-15) |
| `redis.dial_timeout` | `2s` | TCP connection timeout |
| `redis.read_timeout` | `200ms` | Socket read timeout |
| `redis.write_timeout` | `200ms` | Socket write timeout |
| `redis.pool_size` | `100` | Connection pool size |
| `redis.min_idle` | `20` | Minimum idle connections |

### Deduplication

| Field | Default | Description |
|-------|---------|-------------|
| `dedup.window` | `10s` | TTL for fingerprint keys in Redis |
| `dedup.max_body_bytes` | `65536` | Max body bytes read for hashing |
| `dedup.fail_open` | `true` | Allow requests when Redis is unreachable |
| `dedup.exclude_methods` | `["GET","HEAD","OPTIONS"]` | Methods that bypass dedup |

### Proxy / X-Accel

| Field | Default | Description |
|-------|---------|-------------|
| `proxy.x_accel_redirect_prefix` | *(empty)* | Nginx internal location prefix (e.g. `/internal/upstream`) |
| `proxy.upstream_url` | *(empty)* | Backend URL for reverse proxy mode (e.g. `http://backend:9000`) |

### Performance

| Field | Default | Description |
|-------|---------|-------------|
| `performance.local_cache` | `true` | Enable L1 in-process cache |
| `performance.gogc` | `200` | Go garbage collector target percentage |
| `performance.store_timeout` | `500ms` | Context deadline for store operations |

### Example config.json

```json
{
  "server": { "listen_addr": ":8081", "log_level": "info", "shutdown_timeout": "10s" },
  "redis": { "addr": "localhost:6379", "pool_size": 100 },
  "dedup": { "window": "10s", "max_body_bytes": 65536, "fail_open": true },
  "proxy": { "x_accel_redirect_prefix": "/internal/upstream" },
  "performance": { "local_cache": true, "gogc": 200, "store_timeout": "500ms" }
}
```

### Window Sizing Guide

| Use Case | Recommended Window |
|----------|-------------------|
| Payment processing | 5–10m |
| General API dedup | 1–5m |
| Fast / idempotent APIs | 10–60s |
| Background jobs / batch | 5–30m |

---

## 8. API Reference

### Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/dedup-check` | Sidecar mode: Nginx auth_request target |
| GET | `/dedup-check` | Sidecar mode: Nginx auth_request sub-request |
| ANY | `/*` | X-Accel / Proxy mode: catch-all dedup handler |
| GET | `/healthz` | Liveness/readiness probe (pings Redis) |
| GET | `/metrics` | Prometheus scrape endpoint |
| GET | `/debug/pprof/*` | Go runtime profiling (CPU, memory, goroutines) |

### Response Codes

| Code | Meaning | When |
|------|---------|------|
| **200 OK** | Request allowed | First occurrence, excluded method, or fail-open + Redis error |
| **409 Conflict** | Duplicate detected | Same fingerprint seen within dedup window |
| **403 Forbidden** | Duplicate (auth_request) | Behind Nginx auth_request + duplicate |
| **500 Internal Server Error** | Store error | Fail-closed + Redis unreachable |
| **503 Service Unavailable** | Health check failed | `/healthz` when Redis is down |

### Response Bodies

**Duplicate (409)**:
```json
{
  "error": "duplicate_request",
  "details": "an identical request was received within the deduplication window"
}
```

**Store error, fail-closed (500)**:
```json
{
  "error": "store_unavailable",
  "details": "deduplication store is unreachable; request rejected (fail-closed mode)"
}
```

**Health OK (200)**:
```json
{"status": "ok", "cache_size": 42}
```

**Health error (503)**:
```json
{
  "error": "store_unavailable",
  "details": "redis: connection refused"
}
```

### Request Headers

| Header | Used By | Purpose |
|--------|---------|---------|
| `X-Request-ID` | All modes | Client-provided request ID (auto-generated if absent) |
| `X-Original-Method` | Sidecar mode | Nginx auth_request: original HTTP method |
| `X-Original-URI` | Sidecar mode | Nginx auth_request: original request URI |

### Response Headers

| Header | Mode | Purpose |
|--------|------|---------|
| `X-Accel-Redirect` | X-Accel mode | Nginx internal redirect path |
| `X-Request-Id` | All modes | Request correlation ID |

---

## 9. Nginx Integration

### X-Accel-Redirect Config (nginx/dedup.conf)

```nginx
# Bodies up to 64KB held in memory (match DEDUP_MAX_BODY_BYTES)
client_body_buffer_size   64k;
client_max_body_size      10m;

upstream dedup_service {
    server 127.0.0.1:8081;
    keepalive 32;
}

upstream backend_service {
    server 127.0.0.1:9000;
    keepalive 32;
}

server {
    listen 80;

    # Health/metrics bypass dedup
    location = /healthz { proxy_pass http://dedup_service/healthz; }
    location = /metrics  { proxy_pass http://dedup_service/metrics; }

    # All requests → dedup service
    location / {
        set $original_method $request_method;  # Save before X-Accel changes it
        proxy_pass http://dedup_service;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
    }

    # Internal: proxy to backend (only reachable via X-Accel-Redirect)
    location /internal/upstream {
        internal;
        rewrite ^/internal/upstream(.*)$ $1 break;
        proxy_method $original_method;         # Restore original method
        proxy_pass http://backend_service;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
    }
}
```

**Critical detail**: The `set $original_method $request_method` line must be in the `location /` block (before `proxy_pass`), and `proxy_method $original_method` must be in the `location /internal/upstream` block. Without this, X-Accel-Redirect changes all methods to GET.

### Docker Test Config (nginx/test.conf)

Uses `host.docker.internal:8081` and `host.docker.internal:9000` instead of `127.0.0.1` for Docker Desktop compatibility.

---

## 10. Prometheus Metrics

All metrics use the `dedup_` namespace.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dedup_http_requests_total` | Counter | `method`, `path`, `status` | Total HTTP requests |
| `dedup_http_request_duration_seconds` | Histogram | `method`, `path` | Request latency |
| `dedup_checks_total` | Counter | `outcome` | Dedup outcomes |
| `dedup_store_latency_seconds` | Histogram | `operation` | Redis operation latency |
| `dedup_cache_hits_total` | Counter | — | L1 cache hits |
| `dedup_cache_misses_total` | Counter | — | L1 cache misses |

### `dedup_checks_total` Outcome Values

| Outcome | Meaning |
|---------|---------|
| `allowed` | First occurrence — request passed through |
| `duplicate` | Fingerprint already exists — request blocked |
| `error` | Store error (fail-open allowed, fail-closed rejected) |
| `excluded` | Method in exclude list (GET, HEAD, OPTIONS) |

### Histogram Buckets (HFT — High-Frequency Trading)

Sub-millisecond precision: `0.1ms, 0.25ms, 0.5ms, 1ms, 2.5ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms`

### Example PromQL Queries

```promql
# Request rate by endpoint
rate(dedup_http_requests_total[1m])

# Duplicate rate
rate(dedup_checks_total{outcome="duplicate"}[5m])
  / (rate(dedup_checks_total{outcome="allowed"}[5m]) + rate(dedup_checks_total{outcome="duplicate"}[5m]))

# p99 latency
histogram_quantile(0.99, rate(dedup_http_request_duration_seconds_bucket[5m]))

# L1 cache hit rate
rate(dedup_cache_hits_total[5m])
  / (rate(dedup_cache_hits_total[5m]) + rate(dedup_cache_misses_total[5m]))

# Redis latency p95
histogram_quantile(0.95, rate(dedup_store_latency_seconds_bucket[5m]))
```

---

## 11. Grafana Dashboard

Pre-provisioned at `monitoring/grafana/dashboards/dedup-service.json`.

### Panels

| Section | Panel | Metric Used |
|---------|-------|-------------|
| **Overview** | Request Rate | `dedup_http_requests_total` |
| **Overview** | Request Latency (p50/p95/p99) | `dedup_http_request_duration_seconds` |
| **Deduplication** | Dedup Outcomes | `dedup_checks_total` |
| **Deduplication** | Duplicate Rate (gauge) | `dedup_checks_total` ratio |
| **Deduplication** | L1 Cache Hit Rate (gauge) | `dedup_cache_hits_total / (hits+misses)` |
| **Redis/Store** | Store Latency (p50/p95/p99) | `dedup_store_latency_seconds` |
| **Redis/Store** | L1 Hits vs Misses | `dedup_cache_hits_total`, `dedup_cache_misses_total` |
| **HTTP Status** | Status Code Distribution | `dedup_http_requests_total` by status |
| **HTTP Status** | Error Rate (5xx) | `dedup_http_requests_total{status=~"5.."}` |
| **Endpoints** | /dedup-check Latency | endpoint-specific histogram |
| **Endpoints** | /healthz Latency | endpoint-specific histogram |
| **Go Runtime** | CPU, Memory, Goroutines, GC Pause, Heap Objects, Open FDs | `process_*`, `go_*` |

---

## 12. Project Structure

```
dedup-service/
├── cmd/server/
│   └── main.go                    # Entrypoint, router, graceful shutdown
├── internal/
│   ├── config/
│   │   ├── config.go              # Viper JSON config (no env overrides) + validation
│   │   └── config_test.go         # Defaults, override, validation tests
│   ├── fingerprint/
│   │   ├── fingerprint.go         # SHA-256 fingerprint with pooling
│   │   ├── fingerprint_test.go    # Determinism, field diff, truncation tests
│   │   └── fingerprint_bench_test.go
│   ├── handler/
│   │   ├── handler.go             # DedupHandler (sidecar) + HealthHandler
│   │   ├── handler_test.go        # 9 tests
│   │   ├── handler_bench_test.go
│   │   ├── xaccel.go              # XAccelDedupHandler (X-Accel-Redirect mode)
│   │   ├── xaccel_test.go         # 8 tests
│   │   ├── proxy.go               # ProxyDedupHandler (reverse proxy mode)
│   │   └── proxy_test.go          # 8 tests
│   ├── metrics/
│   │   └── metrics.go             # 6 Prometheus metrics
│   ├── middleware/
│   │   ├── middleware.go           # RequestID, Logging, Recovery, Metrics
│   │   └── middleware_test.go
│   └── store/
│       ├── store.go               # Store interface, RedisStore, MemoryStore
│       ├── store_test.go          # MemoryStore + concurrency tests
│       ├── localcache.go          # 256-shard FNV-1a L1 cache
│       ├── localcache_test.go
│       ├── cached_store.go        # L1→L2 wrapper with background sweep
│       ├── cached_store_test.go
│       └── cache_test.go          # 19 cache tests
├── nginx/
│   ├── dedup.conf                 # Production Nginx config (X-Accel mode)
│   └── test.conf                  # Docker test config (host.docker.internal)
├── monitoring/
│   ├── prometheus.yml             # Prometheus scrape config
│   └── grafana/
│       ├── dashboards/dedup-service.json  # Pre-provisioned dashboard
│       └── provisioning/                  # Datasource + dashboard provisioning
├── scripts/
│   ├── functional_test.sh         # 17 functional test cases
│   ├── load_test.sh               # Load test runner (hey + custom Go)
│   ├── mock_backend.py            # Python mock backend (:9000)
│   └── test_service.sh            # Full test suite wrapper
├── config.json                    # Local dev config
├── config.docker.json             # Docker/K8s config (redis:6379)
├── .env.example                   # All env vars documented
├── .gitignore                     # Build artifacts, logs, IDE files
├── Dockerfile                     # Multi-stage build (alpine:3.21)
├── docker-compose.yml             # dedup + redis + nginx
├── Makefile                       # build/test/lint/monitoring targets
├── go.mod / go.sum                # Go module dependencies
├── README.md                      # Project overview
└── API.md                         # API specification
```

---

## 13. Code Walkthrough

### 13.1 Startup (cmd/server/main.go)

```
main() → run()
  1. config.Load()                    ← JSON config only + validation
  2. zerolog + lumberjack setup       ← structured logging + file rotation
  3. GOGC tuning                      ← runtime/debug.SetGCPercent(cfg.GOGC)
  4. store.NewRedis(cfg)              ← connects to Redis, validates with Ping
     └── if fails + FailOpen:         ← fall back to MemoryStore
  5. store.NewCached(redisStore)      ← wrap with L1 cache (if enabled)
  6. gin.New() + middleware chain:
     │  Recovery → RequestID → Logging → Metrics
  7. Register routes (mode-dependent)
  8. Start HTTP server
  9. Wait for SIGINT/SIGTERM
  10. Graceful shutdown (drain timeout)
```

### 13.2 Middleware Chain (internal/middleware/)

Every request passes through these in order:

1. **Recovery**: Catches panics → 500 JSON + stack trace log
2. **RequestID**: Sets `X-Request-ID` (client-provided or 16-byte hex). Used for **request correlation** — tracing a single request across Client → Nginx → Dedup Service → Backend. The same ID appears in Nginx access logs, dedup service structured logs (zerolog `request_id` field), backend logs, and the response to the client. If the client sends an `X-Request-ID` header, it's reused; otherwise a random 16-byte hex string is generated.
3. **Logging**: Structured zerolog per-request (status-aware log levels)
4. **Metrics**: Prometheus counters + histograms

**Log level logic**:
- `status >= 500` → ERROR
- `status >= 400 && status != 409` → WARN (409 is expected behavior, not an error)
- Everything else → DEBUG

### 13.3 Handler (internal/handler/)

Three implementations of the same pattern:

```go
type Store interface {
    IsDuplicate(ctx context.Context, key string, ttl time.Duration) (bool, error)
    Ping(ctx context.Context) error
    Close() error
}
```

All handlers follow:
1. Check if method is excluded
2. Read body (X-Accel/Proxy) or skip (sidecar — body unavailable)
3. Compute fingerprint
4. Call `store.IsDuplicate()` with context deadline (`StoreTimeout`)
5. Return allow/duplicate/error

### 13.4 Store (internal/store/)

**RedisStore.IsDuplicate()**:
```go
err := s.client.SetArgs(ctx, key, 1, redis.SetArgs{
    Mode: "NX",
    TTL:  ttl,
}).Err()
if errors.Is(err, redis.Nil) {
    return true, nil     // key existed → duplicate
}
if err != nil {
    return false, ErrUnavailable
}
return false, nil        // key created → not duplicate
```

**CachedStore.IsDuplicate()** (L1 → L2):
```go
if s.cache.Contains(key) {
    metrics.CacheHitsTotal.Inc()
    return true, nil
}
metrics.CacheMissesTotal.Inc()

dup, err := s.backend.IsDuplicate(ctx, key, ttl)
s.cache.Set(key, ttl)  // cache regardless of result
return dup, err
```

### 13.5 Fingerprint (internal/fingerprint/)

```go
func (fp *Request) Hash() string {
    h := hashPool.Get().(hash.Hash)
    h.Reset()
    defer hashPool.Put(h)

    // Zero-copy string writes via unsafe
    io.WriteString(h, fp.Method)
    io.WriteString(h, fp.URI)
    h.Write(fp.Body)

    var buf [sha256.Size]byte
    return hex.EncodeToString(h.Sum(buf[:0]))
}
```

---

## 14. Performance Optimizations

### Applied Optimizations (Profiling-Driven)

| Optimization | Impact | File |
|---|---|---|
| Duplicate log `Info → Debug` | 62% CPU was in console I/O; eliminated | handler.go, xaccel.go, proxy.go |
| 409 excluded from WARN | Middleware was logging every 409 as warning | middleware.go |
| Pooled body reads (`GetBodyBuf/PutBodyBuf`) | Eliminated 40MB `io.ReadAll` allocations | xaccel.go, proxy.go, fingerprint.go |
| `hash.Hash` interface pool | Avoid SHA-256 object allocation per request | fingerprint.go |
| 256-shard FNV-1a L1 cache | ~100ns lookups, minimal contention | localcache.go |
| `automaxprocs` | Auto-sets GOMAXPROCS to container CPU quota | main.go |
| GOGC=200 | Reduces GC frequency (trades memory for CPU) | config, main.go |
| Pre-serialized JSON responses | Avoids json.Marshal for static response bodies | handler.go |
| Zero-copy string hashing | `unsafe.Pointer` avoids string→[]byte copy | fingerprint.go |
| Single-allocation Redis key | `"dedup:" + hex` built in one allocation | fingerprint.go |

### Memory Layout

- Body pool: 64 KB reusable buffers (`sync.Pool`)
- Hash pool: `crypto/sha256` digest objects (`sync.Pool`)
- L1 cache: 256 shards × `map[string]int64` — keys are 70-byte strings, values are 8-byte timestamps

---

## 15. Load Test Results

**Environment**: Windows, AMD Ryzen 7 4800H, Docker Desktop (Redis + Nginx), Go 1.26.0

### Pre-Optimization vs Post-Optimization

| Scenario | Pre-Opt (req/s) | Post-Opt (req/s) | Speedup |
|---|---|---|---|
| GET /healthz 5000/50c | 1,404 (43×503) | 2,645 | 1.9× |
| POST duplicate 5000/50c | 1,972 | 12,164 | 6.2× |
| POST duplicate 10000/100c | 1,901 | 9,238 | 4.9× |
| POST duplicate 10000/200c | 971 (88 EOF) | 7,552 | 7.8× |
| POST unique 5000/50c | 538 | 518 | ~1× |
| Nginx E2E duplicate 5000/50c | 682 | 1,187 | 1.7× |

### Key Observations

- **Duplicate detection** is the hot path and benefits most from L1 cache + log optimization
- **Unique payloads** are Redis-bound (each request must write to Redis)
- **Nginx E2E** adds ~30ms overhead per request (auth subrequest + internal redirect)
- **Zero errors**: No 503s, no EOFs, no connection pool exhaustion at 200 concurrency

---

## 16. Running Locally

### Prerequisites
- Go 1.24+
- Docker (for Redis)
- `hey` (optional, for load testing): `go install github.com/rakyll/hey@latest`

### Quick Start

```bash
# 1. Start Redis
docker run -d --name redis-test -p 6379:6379 redis:7-alpine

# 2. Start the service (uses config.json — sidecar mode by default)
go run ./cmd/server

# 3. To use X-Accel mode, edit config.json:
#    "proxy": { "x_accel_redirect_prefix": "/internal/upstream" }
#    Then restart the service.

# 4. Test
curl -s http://localhost:8081/healthz
curl -X POST -H "Content-Type: application/json" -d '{"id":"test"}' http://localhost:8081/dedup-check
```

### With Nginx (Full E2E)

```bash
# 1. Start Redis
docker run -d --name redis-test -p 6379:6379 redis:7-alpine

# 2. Start mock backend
python scripts/mock_backend.py &

# 3. Start Nginx (Docker, uses test.conf with host.docker.internal)
docker run -d --name nginx-dedup -p 8080:80 \
  -v "$(pwd)/nginx/test.conf:/etc/nginx/conf.d/default.conf:ro" \
  nginx:1.27-alpine

# 4. Set X-Accel mode in config.json:
#    "proxy": { "x_accel_redirect_prefix": "/internal/upstream" }
#    Then start the service:
go run ./cmd/server

# 5. Test through Nginx
curl -X POST -H "Content-Type: application/json" \
  -d '{"id":"order-1","amount":100}' \
  http://localhost:8080/api/orders
# → {"status":"ok","source":"upstream","method":"POST","path":"/api/orders"}

curl -X POST -H "Content-Type: application/json" \
  -d '{"id":"order-1","amount":100}' \
  http://localhost:8080/api/orders
# → {"error":"duplicate_request","details":"..."}  (409)
```

---

## 17. Docker Deployment

### docker-compose.yml

```bash
docker compose up -d
```

Starts three services:
- **dedup-service** (`:8081`) — built from Dockerfile, non-root user
- **redis** (`:6379`) — redis:7-alpine with healthcheck
- **nginx** (`:8080`) — nginx:1.27-alpine, mounts `nginx/dedup.conf`

### Dockerfile

Multi-stage build:
1. **Build stage**: `golang:1.24-alpine` — compiles static binary with stripped symbols
2. **Runtime stage**: `alpine:3.21` — ~50 MB image, non-root `app` user

### Kubernetes

Mount an appropriate `config.json` via ConfigMap:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dedup-config
data:
  config.json: |
    {
      "server": { "listen_addr": ":8081" },
      "redis": { "addr": "redis-master.redis:6379" },
      "dedup": { "fail_open": true },
      "proxy": { "x_accel_redirect_prefix": "/internal/upstream" }
    }
```

```yaml
# In the Deployment spec:
volumeMounts:
  - name: config
    mountPath: /app/config.json
    subPath: config.json
volumes:
  - name: config
    configMap:
      name: dedup-config
```

---

## 18. Testing

### Unit Tests

```bash
make test
# or
go test ./... -v -race -count=1
```

**Test coverage by package**:

| Package | Tests | Key Scenarios |
|---------|-------|--------------|
| `config` | Defaults, env override, validation errors | |
| `fingerprint` | Determinism, field differentiation, body truncation, identity exclusion | |
| `handler` | Allow, reject, fail-open, fail-closed, expiry, auth_request headers | 9 tests |
| `handler (xaccel)` | Allow + X-Accel header, duplicate 409, excluded method, fail-open/closed | 8 tests |
| `handler (proxy)` | Allow + forward, duplicate 409, excluded bypass, fail-open/closed | 8 tests |
| `middleware` | RequestID generation, logging levels, panic recovery | |
| `store` | MemoryStore CRUD, concurrency (exactly-once), expiry reuse | |
| `store (cache)` | L1 hit/miss, backend propagation, sweep, concurrent access | 19 tests |

### Functional Tests

```bash
bash scripts/functional_test.sh
```

17 tests covering:
- Health check, first request allowed, duplicate rejected
- Different body/URI allowed, auth headers ignored
- Non-POST methods return 404 (PUT/DELETE/PATCH)
- GET /dedup-check returns 200 (auth_request route)
- Metrics endpoint, pprof endpoint, 404 for unknown routes

### Load Tests

```bash
bash scripts/load_test.sh
```

Uses `hey` for duplicate detection + custom Go program for unique payloads.

### Full Suite

```bash
bash scripts/test_service.sh
```

Runs functional tests first (aborts on failure), then load tests.

---

## 19. Makefile Targets

| Target | Description |
|--------|-------------|
| `make build` | Compile binary → `bin/dedup-service` (stripped, trimpath) |
| `make run` | `go run ./cmd/server` |
| `make test` | All unit tests with race detector |
| `make test-cover` | Tests + coverage report |
| `make lint` | golangci-lint |
| `make tidy` | go mod tidy + verify |
| `make clean` | Remove `bin/` and `coverage.out` |
| `make monitoring-up` | Start Prometheus (:9090) + Grafana (:3000) in Docker |
| `make monitoring-down` | Stop monitoring containers |
| `make help` | List all targets |

---

## 20. Troubleshooting

### MSYS Path Conversion (Windows/Git Bash)

**Symptom**: X-Accel-Redirect header contains `C:/Program Files/Git/internal/upstream/...` instead of `/internal/upstream/...`.

**Cause**: Git Bash (MSYS) was converting environment variables that look like Unix paths to Windows paths. This was relevant when the service used env var overrides.

**Current status**: No longer an issue. Config is now read exclusively from `config.json`, which is not subject to MSYS path conversion. Simply set the prefix in your JSON file:
```json
{ "proxy": { "x_accel_redirect_prefix": "/internal/upstream" } }
```

### 503 on /healthz Under Load

**Symptom**: Health check returns 503 during burst traffic.

**Cause**: Redis Ping() times out when the connection pool is saturated.

**Fix**: Increase `DEDUP_REDIS_POOL_SIZE` (default: 100) and `DEDUP_REDIS_MIN_IDLE` (default: 20).

### EOF Errors at High Concurrency

**Symptom**: `EOF` errors in logs at 200+ concurrency.

**Cause**: Connection pool exhaustion.

**Fix**: Increase pool size and ensure `StoreTimeout < RedisReadTimeout`.

### Duplicate Detection Not Working

**Checklist**:
1. Is the method excluded? (`GET`, `HEAD`, `OPTIONS` bypass dedup by default)
2. Is the dedup window too short? (default: 10s)
3. Is the body different? Even a different timestamp in the body changes the fingerprint
4. Is Redis running? Check `curl http://localhost:8081/healthz`
5. In sidecar mode: body is NOT available, only method + URI are fingerprinted

### Nginx Returns 404 After X-Accel-Redirect

**Checklist**:
1. Does the Nginx config have `location /internal/upstream { internal; ... }`?
2. Does the `rewrite` strip the prefix correctly?
3. Is the dedup service `config.json` set: `"proxy": { "x_accel_redirect_prefix": "/internal/upstream" }`?
4. Does the Nginx upstream point to the correct backend address?

---

## 21. Design Decisions & Trade-offs

### Why No Client Identity in Fingerprint?

The fingerprint intentionally excludes IP, Authorization, and session headers. Deduplication targets the *operation* (e.g., "create order #123"), not the caller. Two users submitting the same payment with the same reference ID should be caught. Per-caller isolation belongs at the authorization/routing layer.

**Trade-off**: Anonymous endpoints (no auth header) have global dedup scope — two different anonymous users posting the same body will collide. Solutions: exclude the route, configure a session header, or accept this behavior.

### Why Fail-Open by Default?

Availability is prioritized over correctness. If Redis goes down, requests pass through to the backend. The assumption is that a rare duplicate reaching the backend is less harmful than blocking all traffic.

**Trade-off**: During a Redis outage, duplicates may slip through. Set `DEDUP_FAIL_OPEN=false` for payment-critical services where duplicates are unacceptable.

### Why SHA-256 Instead of a Faster Hash?

SHA-256 provides collision resistance. With millions of requests per day, even a tiny collision probability (as with FNV or xxHash) could cause silent data loss. SHA-256's throughput (~400 MB/s on modern CPUs) is not the bottleneck — Redis round-trips are.

### Why 256 Shards for L1 Cache?

Without sharding, every goroutine reading or writing to the cache competes for a single `sync.RWMutex`. At 200+ concurrent requests, that mutex becomes a bottleneck — goroutines spend more time waiting for the lock than doing actual work.

With 256 shards, each shard has its own `RWMutex`. A request's key is hashed with FNV-1a to pick one of 256 shards, so on average at 200 concurrent goroutines, each shard sees ~1 goroutine — virtually no contention. Reads (the hot path for duplicate detection) can happen in parallel across different shards since `RLock` doesn't block other readers.

**Trade-off**: 256 map headers + 256 mutexes in memory — negligible overhead for a major concurrency win.

### Why Not Use Redis Lua Scripts?

`SET NX PX` is the perfect primitive for deduplication because it does exactly what we need in a **single atomic operation**: check if the fingerprint key exists, and set it with a TTL if it doesn't.

If two identical requests arrive at the same millisecond across different service instances, Redis guarantees only one `SET NX` succeeds — the other gets `Nil`. No race condition, no Lua script, no distributed lock needed.

```
Request A: SET dedup:abc123 1 NX PX 10000  →  OK    (first, allowed)
Request B: SET dedup:abc123 1 NX PX 10000  →  Nil   (duplicate, blocked)
```

The alternatives are worse:
- **GET then SET**: Race condition — two requests could both GET "not found" and both SET
- **Lua script**: Same atomicity but more complexity (script loading, caching, debugging)
- **WATCH/MULTI**: Optimistic locking with retries — unnecessary overhead for a simple case

`SET NX PX` gives atomic check-and-set + automatic expiry in one round-trip.

### Why Body is Capped at 64 KB?

Most API request bodies are under 10 KB. Reading beyond 64 KB provides diminishing returns for fingerprint uniqueness while increasing memory pressure and latency. The cap is configurable via `dedup.max_body_bytes` in config.json.

---

## 22. X-Accel-Redirect — How It Works Under the Hood

This section answers common questions about the X-Accel-Redirect mechanism.

### What is X-Accel-Redirect?

`X-Accel-Redirect` is **not** an Nginx configuration directive — it's a **response header** that backends send to Nginx. Nginx automatically recognizes this header in any `proxy_pass` response and performs an internal redirect. No special config is needed to enable it; the `internal;` directive on the target `location` block simply restricts it from being accessed directly by clients.

### Who reads the `proxy.x_accel_redirect_prefix` config?

Only the **Go dedup service** reads this config value. Nginx never reads it. The service uses it to construct the `X-Accel-Redirect` response header:

```
X-Accel-Redirect: <prefix> + <original_URI>
                  /internal/upstream + /api/orders
```

The Nginx config has a matching `location /internal/upstream { internal; ... }` block hardcoded. **The prefix value in config.json must match the Nginx location block** — if you change one, you must change the other.

### Why does the dedup service send the URI back to Nginx?

Nginx treats `proxy_pass` and the X-Accel internal redirect as **two independent steps**. When Nginx proxies to the dedup service, that subrequest is complete once it gets a response. The internal redirect to `/internal/upstream` is a brand new internal request — Nginx does not automatically carry over the original URI.

The `X-Accel-Redirect` header value *is* the URI for that new internal request. This is by design: the backend can tell Nginx where to redirect, potentially to a different path than the original.

### Is the original request body preserved?

Yes. Nginx buffers the client's request body internally and reuses it for the X-Accel internal redirect. The body flows through:

```
Client sends body → Nginx buffers it (client_body_buffer_size 64k)
  → proxy_pass to dedup service (body sent; dedup reads it for fingerprinting)
  → dedup returns 200 + X-Accel-Redirect header (no body in response)
  → Nginx internal redirect to /internal/upstream
  → proxy_pass to backend (same buffered body sent again)
```

The dedup service reads the body to compute the fingerprint but doesn't alter it from Nginx's perspective — Nginx has its own buffered copy.

### X-Accel vs Reverse Proxy: URI handling

| Aspect | X-Accel-Redirect | Reverse Proxy |
|--------|------------------|---------------|
| URI resolution | Dynamic — any URI from the client | Client URI preserved, host fixed |
| Destination | Nginx routes via `location` blocks | Single upstream URL in config |
| Multi-backend routing | Yes (configure in Nginx) | No (one fixed upstream) |
| Nginx required | Yes | No |
| Body available | Yes | Yes |

### How does Nginx send the whole request to the dedup service?

Through `proxy_pass` with its default settings. `proxy_pass` forwards **everything** by default:
- **Method**: The original HTTP method (POST, PUT, DELETE, etc.)
- **URI**: The full path + query string
- **Headers**: All client headers (Host, Content-Type, Authorization, etc.)
- **Body**: The entire request body (`proxy_pass_request_body` defaults to `on`)

Nginx first buffers the body in memory (up to `client_body_buffer_size 64k`) or spills to disk for larger bodies. Then it opens a connection to the dedup service and sends the full HTTP request. From the dedup service's perspective, the request looks identical to what the client sent. This is standard HTTP reverse proxying — there's no special mechanism.

### What happens after Nginx receives the X-Accel-Redirect header?

1. **Nginx intercepts the response** — it does NOT send the dedup service's 200 response to the client. The `X-Accel-Redirect` header is consumed internally and never reaches the client.
2. **Nginx starts a new internal request** to the path in the header (e.g. `/internal/upstream/api/orders`). This is not an HTTP redirect — it happens entirely inside Nginx.
3. **Nginx matches the path** against `location /internal/upstream { internal; ... }`:
   - `rewrite` strips the prefix → `/api/orders`
   - `proxy_method $original_method` restores the original method
4. **Nginx forwards to the backend** with the original method, stripped URI, buffered body, and client headers.
5. **Backend responds** → Nginx sends that response directly to the client.

The client sees only the backend's response — it never knows the dedup service existed.

### How is the HTTP method preserved? What about DELETE, PUT, etc.?

X-Accel-Redirect changes the method to GET (Nginx's default for internal redirects). The config handles this with a two-step approach:

1. **Before proxy_pass** (in `location /`):
   ```nginx
   set $original_method $request_method;  # saves POST, DELETE, PUT, PATCH, etc.
   ```
2. **After X-Accel-Redirect** (in `location /internal/upstream`):
   ```nginx
   proxy_method $original_method;  # restores the saved method
   ```

| Client sends | `$original_method` saved as | Backend receives |
|---|---|---|
| `POST /api/orders` | `POST` | `POST /api/orders` |
| `DELETE /api/users/5` | `DELETE` | `DELETE /api/users/5` |
| `PUT /api/items/3` | `PUT` | `PUT /api/items/3` |
| `PATCH /api/config` | `PATCH` | `PATCH /api/config` |

Without `proxy_method $original_method`, every request would arrive at the backend as GET.

---

## 23. Capacity Planning & Redis Sizing

### Traffic Analysis (50 Lakh – 1 Crore requests/day)

| Daily Volume | Avg req/s | Peak (5x burst) |
|---|---|---|
| 50 Lakh (5M) | ~58 | ~290 |
| 1 Crore (10M) | ~116 | ~580 |

### Current Capacity (from load tests on laptop)

| Scenario | Measured req/s |
|---|---|
| Duplicate detection (hot path) | 7,500 – 12,000 |
| Unique payloads (Redis-bound) | ~518 |
| Nginx E2E | ~1,187 |

A **single instance** handles 1 crore requests/day comfortably. Even worst-case (580 peak req/s, all unique) is within capacity — and production Redis is significantly faster than Docker Desktop Redis on a laptop.

### Redis Memory

With a 5-minute dedup window, only requests within the last 5 minutes exist in Redis:

| Daily Volume | Keys alive (5 min window) | Memory |
|---|---|---|
| 50 Lakh (5M) | ~17,360 | ~2.5 MB |
| 1 Crore (10M) | ~34,720 | ~5 MB |
| Peak burst (5x) | ~173,600 | ~25 MB |

Redis needs **< 25 MB of working memory** even at peak burst. The smallest instance (e.g. AWS `cache.t3.micro` with 0.5 GB) is more than sufficient.

### Redis CPU

Each request = 1 `SET NX PX` command (~1 µs on Redis). At 580 req/s peak, Redis uses < 0.1% CPU. A single Redis core handles ~100K ops/s.

### L1 Cache Impact (5 min window)

The in-process LocalCache holds 30x more keys than with a 10s window. At ~150 bytes/key, peak is ~26 MB in Go heap — still fine, but GC works slightly harder. The upside: longer window means more L1 cache hits, reducing Redis round-trips and **improving** throughput for duplicate detection.

The LocalCache sweep runs every 10 seconds (hardcoded). With a 5-minute TTL, expired keys sit at most 10s past expiry — negligible overhead.

### Trade-off: Legitimate Retries

A 5-minute window means legitimate retries within 5 minutes are blocked. If a user's first request genuinely fails (e.g. backend returns 500, network timeout), they cannot retry with the same payload for 5 minutes. The dedup service does not distinguish between "backend succeeded" and "backend failed" — it only knows the fingerprint was seen. This is acceptable for use cases like payment idempotency where retries should use a different idempotency key.

### Recommended Production Redis Setup

| Aspect | Recommendation |
|---|---|
| **Instance type** | Smallest available (e.g. `cache.t3.micro` or `cache.t4g.micro`) |
| **Memory** | 0.5 GB is more than enough |
| **High availability** | Redis with 1 replica (automatic failover) |
| **Persistence** | **Not needed** — keys expire in 10s; losing them on restart means a brief window where duplicates pass through |
| **Maxmemory policy** | `volatile-ttl` — evict keys closest to expiry first (all keys have TTL) |
| **Network** | Same VPC/subnet as the dedup service to minimize latency |
| **Connection pool** | 50 is enough for 580 peak req/s |
| **Dedup window** | 5 minutes (`"window": "5m"`) |

### Production Config

```json
{
  "redis": {
    "addr": "redis-primary.your-vpc:6379",
    "pool_size": 50,
    "min_idle": 10,
    "dial_timeout": "2s",
    "read_timeout": "200ms",
    "write_timeout": "200ms"
  },
  "dedup": {
    "fail_open": true
  }
}
```

### What NOT to worry about

- **Disk**: No persistence needed, no RDB/AOF
- **Cluster mode**: Single node handles this load trivially
- **Scaling**: ~100x current traffic before Redis becomes a concern
- **Key eviction**: All keys auto-expire via TTL, no memory growth risk

---

## Dependencies

| Dependency | Version | Purpose |
|------------|---------|---------|
| `github.com/gin-gonic/gin` | v1.12.0 | HTTP framework |
| `github.com/spf13/viper` | v1.21.0 | Configuration management |
| `github.com/rs/zerolog` | v1.34.0 | Structured JSON logging |
| `github.com/redis/go-redis/v9` | v9.18.0 | Redis client |
| `github.com/prometheus/client_golang` | v1.23.2 | Prometheus metrics |
| `go.uber.org/automaxprocs` | v1.6.0 | Container CPU detection |
| `gopkg.in/natefinish/lumberjack.v2` | v2.2.1 | Log file rotation |
