# API Specification — dedup-service

Base URL: `http://<host>:8081` (configurable via `server.listen_addr`)

All responses use `Content-Type: application/json; charset=utf-8` unless otherwise noted.

Every response includes an `X-Request-ID` header. If the client supplies one, it is echoed back; otherwise a random 32-character hex ID is generated.

---

## Endpoints

### POST `/dedup-check`

Nginx `auth_request` target. Determines whether an incoming request is a duplicate based on its SHA-256 fingerprint.

**Fingerprint formula:**

```
SHA-256( Method | URI+Query | Body[:max_body_bytes] )
```

Client IP and Authorization headers are **not** included — identical requests from different callers are considered duplicates.

#### Request

| Header / Field | Required | Description |
|---|---|---|
| `X-Request-ID` | No | Client-provided correlation ID (preserved if present) |
| Request body | No | Forwarded by Nginx; first 64 KB (default) is hashed |

The request method, URI, and body are extracted from the proxied original request headers set by Nginx (`X-Original-Method`, `X-Original-URI`, and the forwarded body).

#### Responses

| Status | Condition | Body |
|---|---|---|
| `200 OK` | Request is **new** (allowed) | _(empty)_ |
| `200 OK` | Method is excluded (`GET`, `HEAD`, `OPTIONS` by default) | _(empty)_ |
| `200 OK` | Store error + **fail-open** mode enabled | _(empty)_ |
| `409 Conflict` | Request is a **duplicate** within the dedup window | See below |
| `500 Internal Server Error` | Store error + **fail-closed** mode | See below |

**409 Conflict:**
```json
{
  "error": "duplicate_request",
  "details": "an identical request was received within the deduplication window"
}
```

**500 Internal Server Error (fail-closed):**
```json
{
  "error": "store_unavailable",
  "details": "deduplication store is unreachable; request rejected (fail-closed mode)"
}
```

#### Dedup Behaviour

- First request with a given fingerprint → `200` (allowed), fingerprint stored in Redis with TTL = `dedup.window`.
- Subsequent identical requests within the window → `409` (blocked).
- After TTL expires, the same request is treated as new again.
- If L1 local cache is enabled, known duplicates are resolved in-process without a Redis round-trip.

---

### GET `/healthz`

Liveness / readiness probe. Pings the Redis store and returns status.

#### Request

No body or query parameters required.

#### Responses

**200 OK (without L1 cache):**
```json
{
  "status": "ok"
}
```

**200 OK (with L1 cache enabled):**
```json
{
  "status": "ok",
  "cache_size": 1042
}
```

| Field | Type | Description |
|---|---|---|
| `status` | string | Always `"ok"` when healthy |
| `cache_size` | integer | Number of entries in the L1 local cache (only present when `performance.local_cache` is `true`) |

**503 Service Unavailable:**
```json
{
  "error": "store_unavailable",
  "details": "dial tcp 127.0.0.1:6379: connect: connection refused"
}
```

| Field | Type | Description |
|---|---|---|
| `error` | string | Error category |
| `details` | string | Underlying error message from Redis ping |

---

### GET `/metrics`

Prometheus metrics endpoint. Returns metrics in the [Prometheus text exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/).

**Content-Type:** `text/plain; version=0.0.4; charset=utf-8`

#### Exported Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `dedup_http_requests_total` | Counter | `method`, `path`, `status` | Total HTTP requests processed |
| `dedup_http_request_duration_seconds` | Histogram | `method`, `path` | Request latency (HFT buckets: 100µs → 250ms) |
| `dedup_checks_total` | Counter | `outcome` | Dedup check outcomes |
| `dedup_store_latency_seconds` | Histogram | `operation` | Redis operation latency |
| `dedup_cache_hits_total` | Counter | — | L1 cache hits (Redis call avoided) |
| `dedup_cache_misses_total` | Counter | — | L1 cache misses (fell through to Redis) |

**`outcome` label values:** `allowed`, `duplicate`, `error`, `excluded`

**`operation` label values:** `is_duplicate`

**Histogram buckets (seconds):** `0.0001`, `0.00025`, `0.0005`, `0.001`, `0.0025`, `0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`

---

### GET `/debug/pprof/*`

Go runtime profiling endpoints. Available in all builds.

| Path | Description |
|---|---|
| `/debug/pprof/` | Index page listing available profiles |
| `/debug/pprof/cmdline` | Running command line |
| `/debug/pprof/profile` | CPU profile (default 30s, use `?seconds=N`) |
| `/debug/pprof/symbol` | Symbol lookup |
| `/debug/pprof/trace` | Execution trace (use `?seconds=N`) |
| `/debug/pprof/heap` | Heap memory profile |
| `/debug/pprof/goroutine` | Goroutine stack dump |
| `/debug/pprof/allocs` | Allocation profile |

---

### Unmatched Routes

Any request to an undefined path returns:

**404 Not Found:**
```json
{
  "error": "not_found"
}
```

---

## Common Headers

### Request Headers

| Header | Description |
|---|---|
| `X-Request-ID` | Optional. Client-provided correlation ID. If absent, the service generates one. |
| `Content-Type` | Not validated. Body is read raw for fingerprinting. |

### Response Headers

| Header | Description |
|---|---|
| `X-Request-ID` | Correlation ID for the request (client-provided or auto-generated) |
| `Content-Type` | `application/json; charset=utf-8` for JSON responses |

---

## Error Model

All error responses follow a consistent JSON structure:

```json
{
  "error": "<error_code>",
  "details": "<human-readable description>"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `error` | string | Yes | Machine-readable error code |
| `details` | string | No | Human-readable explanation (omitted when empty) |

### Error Codes

| Code | HTTP Status | Meaning |
|---|---|---|
| `duplicate_request` | 409 | Request fingerprint matches one within the dedup window |
| `store_unavailable` | 500 or 503 | Redis is unreachable (500 on dedup-check fail-closed, 503 on healthz) |
| `internal_error` | 500 | Unhandled panic recovered by middleware |
| `not_found` | 404 | No route matches the request path |

---

## Usage Examples

### Check a request (unique)

```bash
curl -s -w "\nHTTP %{http_code}\n" -X POST http://localhost:8081/dedup-check \
  -H "Content-Type: application/json" \
  -d '{"order_id": 12345}'
```

```
HTTP 200
```

### Check a request (duplicate)

```bash
# Same request again within the dedup window
curl -s -w "\nHTTP %{http_code}\n" -X POST http://localhost:8081/dedup-check \
  -H "Content-Type: application/json" \
  -d '{"order_id": 12345}'
```

```json
{"error":"duplicate_request","details":"an identical request was received within the deduplication window"}
HTTP 409
```

### Health check

```bash
curl -s http://localhost:8081/healthz | jq .
```

```json
{
  "status": "ok",
  "cache_size": 0
}
```

### Health check (Redis down)

```bash
curl -s -w "\nHTTP %{http_code}\n" http://localhost:8081/healthz
```

```json
{"error":"store_unavailable","details":"dial tcp 127.0.0.1:6379: connect: connection refused"}
HTTP 503
```

### With client-provided request ID

```bash
curl -s -D - -X POST http://localhost:8081/dedup-check \
  -H "X-Request-ID: txn-abc-123" \
  -d '{"amount": 99.99}'
```

```
HTTP/1.1 200 OK
X-Request-ID: txn-abc-123
```

### Scrape Prometheus metrics

```bash
curl -s http://localhost:8081/metrics | grep dedup_
```
