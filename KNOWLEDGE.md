# Dedup Service Knowledge (X-Accel-Redirect Only)

## 1. Scope

This repository supports one runtime mode only:

- X-Accel-Redirect mode

Legacy reverse-proxy and sidecar/auth-request modes have been removed from runtime code.

## 2. High-Level Flow

1. Nginx forwards full client request (method + URI + body) to dedup service.
2. Dedup service computes fingerprint and checks Redis via atomic NX write.
3. If allowed, service returns `200` with `X-Accel-Redirect`.
4. Nginx internally redirects to upstream backend, preserving method and body.
5. If duplicate, service returns `409` with JSON error.

## 3. Fingerprint

Formula:

```
SHA-256( Method | URI+Query | Body )
```

Included:

- Method
- URI + query
- Full raw body

Excluded by design:

- Client IP
- Authorization header
- Other headers

## 4. Store and Caching

- L2 source of truth: Redis using atomic SET NX with TTL (`dedup.window`).
- L1 local cache: in-process sharded cache to short-circuit known duplicates.

Behavior:

- First request for key: allowed and stored with TTL.
- Repeated request within window: duplicate (`409`).

## 5. Endpoints

- `ANY /*`: catch-all dedup handler (primary request path)
- `GET /healthz`: readiness/liveness
- `GET /metrics`: Prometheus metrics
- `GET /debug/pprof/*`: profiling endpoints (localhost-only, port 6060)

## 6. Responses

Allowed request:

- `200 OK`
- `X-Accel-Redirect: /internal/upstream{request_uri}`

Duplicate request:

- `409 Conflict`
- Body:

```json
{
  "error": "duplicate_request",
  "details": "an identical request was received within the deduplication window"
}
```

Store unavailable (fail-closed):

- `500 Internal Server Error`

Health failure:

- `503 Service Unavailable`

## 7. Configuration

Config is loaded from `config.json` (Viper). If absent, defaults are used.

Important keys:

- `dedup.window`
- `dedup.fail_open`
- `dedup.exclude_methods`
- `proxy.x_accel_redirect_prefix`
- `performance.local_cache`
- `performance.store_timeout`

`proxy.x_accel_redirect_prefix` is required and defaults to `/internal/upstream`.

## 8. Nginx Contract

Required Nginx behavior:

- Forward full request body to dedup service (`proxy_pass` defaults are sufficient).
- Keep original request method before redirect.
- In internal upstream location, restore original method with `proxy_method`.
- Use `internal` location for redirect target prefix.

## 9. Observability

Primary metrics:

- `dedup_http_requests_total`
- `dedup_http_request_duration_seconds`
- `dedup_checks_total` (`allowed`, `duplicate`, `error`, `excluded`)
- `dedup_store_latency_seconds`
- `dedup_cache_hits_total`
- `dedup_cache_misses_total`

## 10. Testing

Recommended commands:

```bash
go test ./...
bash scripts/functional_test.sh
bash scripts/load_test.sh
```

Functional/load scripts are aligned to X-Accel-only behavior and use generic API paths (for example `/api/orders`).
