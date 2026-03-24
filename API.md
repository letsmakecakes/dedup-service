# API Specification - dedup-service (X-Accel-Redirect Mode)

Base URL: `http://<host>:8081` or `https://<host>:8081` (configurable via `server.listen_addr`; set `server.tls_enabled=true` for HTTPS)

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

## Curl Command Cases

Set a base URL first:

```bash
BASE_URL=https://localhost:8081
```

If you are using a self-signed certificate in local development, add `-k` to curl commands.

### 1. Health check

```bash
curl -s "$BASE_URL/healthz"
```

### 2. First request is allowed (200 + X-Accel-Redirect)

```bash
curl -s -D - -o /dev/null -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -d '{"id":"case-001","amount":100}'
```

### 3. Duplicate request is rejected (409)

```bash
# First call seeds the dedup key
curl -s -o /dev/null -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -d '{"id":"case-dup","amount":100}'

# Second call is duplicate
curl -s -w "\nHTTP %{http_code}\n" -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -d '{"id":"case-dup","amount":100}'
```

### 4. Different body is allowed

```bash
curl -s -w "\nHTTP %{http_code}\n" -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -d '{"id":"case-002","amount":200}'
```

### 5. Same body + different URI/query is allowed

```bash
curl -s -w "\nHTTP %{http_code}\n" -X POST "$BASE_URL/api/orders?ref=alt" \
  -H "Content-Type: application/json" \
  -d '{"id":"case-dup","amount":100}'
```

### 6. Excluded methods are allowed (GET/HEAD/OPTIONS)

```bash
curl -s -D - -o /dev/null -X GET "$BASE_URL/api/orders"
curl -s -I "$BASE_URL/api/orders"
curl -s -D - -o /dev/null -X OPTIONS "$BASE_URL/api/orders"
```

### 7. Authorization header does not affect dedup key

```bash
# First request with user A
curl -s -w "\nHTTP %{http_code}\n" -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer userA" \
  -d '{"id":"case-auth","amount":500}'

# Same body with user B still duplicates
curl -s -w "\nHTTP %{http_code}\n" -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer userB" \
  -d '{"id":"case-auth","amount":500}'
```

### 8. Provide custom request ID

```bash
curl -s -D - -o /dev/null -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -H "X-Request-ID: req-demo-123" \
  -d '{"id":"case-rid","amount":700}'
```

### 9. Metrics endpoint

```bash
curl -s "$BASE_URL/metrics" | grep dedup_
```

### 10. pprof endpoint

```bash
curl -s -o /dev/null -w "HTTP %{http_code}\n" "$BASE_URL/debug/pprof/heap"
```

### 11. Unknown route is forwarded (still 200 if allowed)

```bash
curl -s -D - -o /dev/null "$BASE_URL/nonexistent"
```

### 12. Fail-open vs fail-closed behavior check

```bash
# Stop Redis, then call endpoint:
curl -s -w "\nHTTP %{http_code}\n" -X POST "$BASE_URL/api/orders" \
  -H "Content-Type: application/json" \
  -d '{"id":"case-store","amount":900}'
```

Expected:

- `dedup.fail_open=true` -> `200` with `X-Accel-Redirect`
- `dedup.fail_open=false` -> `500` with `store_unavailable`
