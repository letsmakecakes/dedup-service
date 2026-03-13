# API Specification - dedup-service (X-Accel-Redirect Mode)

Base URL: `http://<host>:8081` (configurable via `server.listen_addr`)

All responses include `X-Request-ID`.

## Core Behavior

The service is a dedup gate for Nginx and handles unmatched routes via catch-all routing.

Fingerprint formula:

```
SHA-256( Method | URI+Query | Body[:max_body_bytes] )
```

Request outcomes:

- Allowed: `200 OK` and `X-Accel-Redirect: /internal/upstream{original_uri}`
- Duplicate: `409 Conflict` with JSON error body
- Store unavailable + fail-open: `200 OK` with redirect header
- Store unavailable + fail-closed: `500 Internal Server Error`

## Endpoints

### `ANY /*` (catch-all dedup)

This is the primary dedup endpoint used by Nginx `proxy_pass` in X-Accel mode.

Responses:

| Status | Condition |
|---|---|
| `200 OK` | Request is allowed (new fingerprint) |
| `200 OK` | Excluded method (`GET`, `HEAD`, `OPTIONS`) |
| `200 OK` | Store error with fail-open |
| `409 Conflict` | Duplicate within dedup window |
| `500 Internal Server Error` | Store error with fail-closed |

Duplicate response body:

```json
{
  "error": "duplicate_request",
  "details": "an identical request was received within the deduplication window"
}
```

Fail-closed store error body:

```json
{
  "error": "store_unavailable",
  "details": "deduplication store is unreachable; request rejected (fail-closed mode)"
}
```

### `GET /healthz`

Health/readiness probe. Pings the store.

`200 OK`:

```json
{
  "status": "ok",
  "cache_size": 0
}
```

`503 Service Unavailable`:

```json
{
  "error": "store_unavailable",
  "details": "..."
}
```

### `GET /metrics`

Prometheus text exposition endpoint.

### `GET /debug/pprof/*`

Go runtime profiling endpoints.

## Headers

Request headers:

- `X-Request-ID` (optional): client correlation ID; preserved if provided

Response headers:

- `X-Request-ID`: request correlation ID
- `X-Accel-Redirect`: present for allowed catch-all requests

## Error Model

```json
{
  "error": "<error_code>",
  "details": "<human-readable description>"
}
```

Error codes:

| Code | HTTP Status | Meaning |
|---|---|---|
| `duplicate_request` | 409 | Fingerprint already exists within dedup window |
| `store_unavailable` | 500 or 503 | Redis/store is unreachable |
| `internal_error` | 500 | Panic recovered by middleware |

## Quick Examples

Allowed request:

```bash
curl -s -D - -o /dev/null -X POST http://localhost:8081/api/orders \
  -H "Content-Type: application/json" \
  -d '{"order_id":12345}'
```

Duplicate request:

```bash
curl -s -w "\nHTTP %{http_code}\n" -X POST http://localhost:8081/api/orders \
  -H "Content-Type: application/json" \
  -d '{"order_id":12345}'
```

Health check:

```bash
curl -s http://localhost:8081/healthz
```

Metrics:

```bash
curl -s http://localhost:8081/metrics | grep dedup_
```
