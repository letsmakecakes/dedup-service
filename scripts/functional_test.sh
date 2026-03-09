#!/bin/bash
# ============================================================================
# functional_test.sh — Functional tests for the dedup-service
#
# Prerequisites:
#   - Redis running on localhost:6379  (e.g. docker run -d -p 6379:6379 redis:7-alpine)
#   - dedup-service running on localhost:8081
#   - curl
#
# Usage:
#   bash scripts/functional_test.sh
#   BASE_URL=http://localhost:9090 bash scripts/functional_test.sh
# ============================================================================

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8081}"
PASS=0
FAIL=0
TOTAL=0

# ── Helpers ────────────────────────────────────────────────────────────────────

green()  { printf "\033[32m%s\033[0m" "$*"; }
red()    { printf "\033[31m%s\033[0m" "$*"; }
bold()   { printf "\033[1m%s\033[0m" "$*"; }

assert_status() {
    local description="$1" expected="$2" actual="$3"
    TOTAL=$((TOTAL + 1))
    if [ "$actual" -eq "$expected" ]; then
        PASS=$((PASS + 1))
        echo "  $(green PASS)  $description (HTTP $actual)"
    else
        FAIL=$((FAIL + 1))
        echo "  $(red FAIL)  $description (expected $expected, got $actual)"
    fi
}

assert_json_field() {
    local description="$1" body="$2" field="$3" expected="$4"
    TOTAL=$((TOTAL + 1))
    actual=$(echo "$body" | grep -o "\"$field\":\"[^\"]*\"" | head -1 | cut -d'"' -f4)
    if [ "$actual" = "$expected" ]; then
        PASS=$((PASS + 1))
        echo "  $(green PASS)  $description ($field=$actual)"
    else
        FAIL=$((FAIL + 1))
        echo "  $(red FAIL)  $description (expected $field=$expected, got $field=$actual)"
    fi
}

separator() {
    echo ""
    echo "$(bold "─── $1 ───")"
    echo ""
}

# Flush Redis to ensure clean state
flush_redis() {
    docker exec redis-test redis-cli FLUSHALL > /dev/null 2>&1 || true
}

# ============================================================================
#  FUNCTIONAL TESTS
# ============================================================================

separator "FUNCTIONAL TESTS"
flush_redis

# ── 1. Health check ──────────────────────────────────────────────────

echo "$(bold '1. Health Check')"
status=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/healthz")
assert_status "GET /healthz returns 200" 200 "$status"

body=$(curl -s "$BASE_URL/healthz")
assert_json_field "Response contains status=ok" "$body" "status" "ok"

# ── 2. First request is allowed ──────────────────────────────────────

echo ""
echo "$(bold '2. First Request Allowed')"
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d '{"id":"test-1","amount":100}' \
    "$BASE_URL/dedup-check")
assert_status "First POST is allowed (200)" 200 "$status"

# ── 3. Duplicate request is rejected ─────────────────────────────────

echo ""
echo "$(bold '3. Duplicate Request Rejected')"
response=$(curl -s -w "\n%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d '{"id":"test-1","amount":100}' \
    "$BASE_URL/dedup-check")
body=$(echo "$response" | head -1)
status=$(echo "$response" | tail -1)
assert_status "Same POST is rejected (409)" 409 "$status"
assert_json_field "Error field is duplicate_request" "$body" "error" "duplicate_request"

# ── 4. Different body is allowed ─────────────────────────────────────

echo ""
echo "$(bold '4. Different Body Allowed')"
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d '{"id":"test-2","amount":200}' \
    "$BASE_URL/dedup-check")
assert_status "Different body is allowed (200)" 200 "$status"

# ── 5. Different URI is allowed ──────────────────────────────────────

echo ""
echo "$(bold '5. Different URI Allowed')"
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d '{"id":"test-1","amount":100}' \
    "$BASE_URL/dedup-check?ref=different")
assert_status "Same body, different query param is allowed (200)" 200 "$status"

# ── 6. Non-POST methods return 404 ───────────────────────────────────

echo ""
echo "$(bold '6. Non-POST/GET Methods Return 404 (Router-Level Rejection)')"
for method in PUT DELETE PATCH; do
    status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 3 -X "$method" "$BASE_URL/dedup-check")
    assert_status "$method /dedup-check returns 404" 404 "$status"
done

# GET /dedup-check is registered for Nginx auth_request sub-requests.
# Without X-Original-Method, the method stays GET (excluded) → 200.
status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 3 -X GET "$BASE_URL/dedup-check")
assert_status "GET /dedup-check returns 200 (auth_request route)" 200 "$status"

# ── 7. Different auth headers still deduplicated ─────────────────────

echo ""
echo "$(bold '7. Auth Headers Ignored (Same Body = Duplicate)')"
flush_redis
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer userA" \
    -d '{"id":"auth-test","amount":500}' \
    "$BASE_URL/dedup-check")
assert_status "First POST with Bearer userA (200)" 200 "$status"

status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer userB" \
    -d '{"id":"auth-test","amount":500}' \
    "$BASE_URL/dedup-check")
assert_status "Same body with Bearer userB is duplicate (409)" 409 "$status"

# ── 8. Metrics endpoint ──────────────────────────────────────────────

echo ""
echo "$(bold '8. Prometheus Metrics')"
status=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/metrics")
assert_status "GET /metrics returns 200" 200 "$status"

body=$(curl -s "$BASE_URL/metrics")
TOTAL=$((TOTAL + 1))
if echo "$body" | grep -q "dedup_checks_total"; then
    PASS=$((PASS + 1))
    echo "  $(green PASS)  Metrics contain dedup_checks_total"
else
    FAIL=$((FAIL + 1))
    echo "  $(red FAIL)  Metrics missing dedup_checks_total"
fi

# ── 9. pprof endpoint ────────────────────────────────────────────────

echo ""
echo "$(bold '9. pprof Endpoint')"
status=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/debug/pprof/heap")
assert_status "GET /debug/pprof/heap returns 200" 200 "$status"

# ── 10. 404 for unknown routes ───────────────────────────────────────

echo ""
echo "$(bold '10. Unknown Route Returns 404')"
status=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/nonexistent")
assert_status "GET /nonexistent returns 404" 404 "$status"

# ── Summary ──────────────────────────────────────────────────────────

separator "FUNCTIONAL TEST SUMMARY"
echo "  Total: $TOTAL  |  $(green "Passed: $PASS")  |  $(red "Failed: $FAIL")"

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
