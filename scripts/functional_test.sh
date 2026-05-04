#!/bin/bash
# ============================================================================
# functional_test.sh — Functional tests for the dedup-service
#
# Prerequisites:
#   - Redis running on localhost:6379
#   - dedup-service running on localhost:8081
#   - curl
#
# Usage:
#   bash scripts/functional_test.sh
#   BASE_URL=http://localhost:9090 bash scripts/functional_test.sh
# ============================================================================

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8081}"
TEST_PATH="${TEST_PATH:-/api/orders}"
RUN_ID="${RUN_ID:-$(date +%s)}"
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

assert_header_present() {
    local description="$1" value="$2"
    TOTAL=$((TOTAL + 1))
    if [ -n "$value" ]; then
        PASS=$((PASS + 1))
        echo "  $(green PASS)  $description"
    else
        FAIL=$((FAIL + 1))
        echo "  $(red FAIL)  $description (header missing)"
    fi
}

separator() {
    echo ""
    echo "$(bold "─── $1 ───")"
    echo ""
}

# Flush Redis to ensure clean state
flush_redis() {
    redis-cli FLUSHALL > /dev/null 2>&1 || true
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

# ── 2. First request is allowed and redirected ───────────────────────

echo ""
echo "$(bold '2. First Request Allowed (X-Accel Header Present)')"
response_headers=$(mktemp)
status=$(curl -s -D "$response_headers" -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"test-1-${RUN_ID}\",\"amount\":100}" \
    "$BASE_URL$TEST_PATH")
assert_status "First POST is allowed (200)" 200 "$status"
redirect=$(grep -i '^X-Accel-Redirect:' "$response_headers" | head -1 | cut -d':' -f2- | tr -d '\r' | sed 's/^ *//')
assert_header_present "X-Accel-Redirect header is present" "$redirect"
rm -f "$response_headers"

# ── 3. Duplicate request is rejected ─────────────────────────────────

echo ""
echo "$(bold '3. Duplicate Request Rejected')"
response=$(curl -s -w "\n%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"test-1-${RUN_ID}\",\"amount\":100}" \
    "$BASE_URL$TEST_PATH")
body=$(echo "$response" | head -1)
status=$(echo "$response" | tail -1)
assert_status "Same POST is rejected (409)" 409 "$status"
assert_json_field "Error field is duplicate_request" "$body" "error" "duplicate_request"

# ── 4. Different body is allowed ─────────────────────────────────────

echo ""
echo "$(bold '4. Different Body Allowed')"
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"test-2-${RUN_ID}\",\"amount\":200}" \
    "$BASE_URL$TEST_PATH")
assert_status "Different body is allowed (200)" 200 "$status"

# ── 5. Different URI is allowed ──────────────────────────────────────

echo ""
echo "$(bold '5. Different URI Allowed')"
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"test-1-${RUN_ID}\",\"amount\":100}" \
    "$BASE_URL$TEST_PATH?ref=different")
assert_status "Same body, different query param is allowed (200)" 200 "$status"

# ── 6. Non-dedup methods are allowed and redirected ──────────────────

echo ""
echo "$(bold '6. Excluded Methods Are Allowed (X-Accel)')"
for method in GET HEAD OPTIONS; do
    response_headers=$(mktemp)
    if [ "$method" = "HEAD" ]; then
        status=$(curl -s -I -D "$response_headers" -o /dev/null -w "%{http_code}" --max-time 3 "$BASE_URL$TEST_PATH")
    else
        status=$(curl -s -D "$response_headers" -o /dev/null -w "%{http_code}" --max-time 3 -X "$method" "$BASE_URL$TEST_PATH")
    fi
    assert_status "$method $TEST_PATH returns 200" 200 "$status"
    redirect=$(grep -i '^X-Accel-Redirect:' "$response_headers" | head -1 | cut -d':' -f2- | tr -d '\r' | sed 's/^ *//')
    assert_header_present "$method $TEST_PATH has X-Accel-Redirect" "$redirect"
    rm -f "$response_headers"
done

echo ""
echo "$(bold '7. Auth Headers Ignored (Same Body = Duplicate)')"
flush_redis
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer userA" \
    -d "{\"id\":\"auth-test-${RUN_ID}\",\"amount\":500}" \
    "$BASE_URL$TEST_PATH")
assert_status "First POST with Bearer userA (200)" 200 "$status"

status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer userB" \
    -d "{\"id\":\"auth-test-${RUN_ID}\",\"amount\":500}" \
    "$BASE_URL$TEST_PATH")
assert_status "Same body with Bearer userB is duplicate (409)" 409 "$status"

# ── 8. Prometheus Metrics ─────────────────────────────────────────────

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

# ── 10. Unknown route is handled by X-Accel handler ──────────────────

echo ""
echo "$(bold '10. Unknown Route Is Allowed for Upstream Forwarding')"
response_headers=$(mktemp)
status=$(curl -s -D "$response_headers" -o /dev/null -w "%{http_code}" "$BASE_URL/nonexistent")
assert_status "GET /nonexistent returns 200" 200 "$status"
redirect=$(grep -i '^X-Accel-Redirect:' "$response_headers" | head -1 | cut -d':' -f2- | tr -d '\r' | sed 's/^ *//')
assert_header_present "Unknown route includes X-Accel-Redirect" "$redirect"
rm -f "$response_headers"

# ── Summary ──────────────────────────────────────────────────────────

separator "FUNCTIONAL TEST SUMMARY"
echo "  Total: $TOTAL  |  $(green "Passed: $PASS")  |  $(red "Failed: $FAIL")"

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
