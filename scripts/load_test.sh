#!/bin/bash
# ============================================================================
# load_test.sh — Load tests for the dedup-service
#
# Prerequisites:
#   - Redis running on localhost:6379  (e.g. docker run -d -p 6379:6379 redis:7-alpine)
#   - dedup-service running on localhost:8081
#   - hey  (go install github.com/rakyll/hey@latest)
#   - go   (for unique-payload load test)
#
# Usage:
#   bash scripts/load_test.sh
#   LOAD_N=5000 LOAD_C=100 bash scripts/load_test.sh
#   BASE_URL=http://localhost:9090 bash scripts/load_test.sh
# ============================================================================

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8081}"
LOAD_N="${LOAD_N:-2000}"
LOAD_C="${LOAD_C:-50}"

# ── Helpers ────────────────────────────────────────────────────────────────────

bold()   { printf "\033[1m%s\033[0m" "$*"; }
red()    { printf "\033[31m%s\033[0m" "$*"; }
yellow() { printf "\033[33m%s\033[0m" "$*"; }

separator() {
    echo ""
    echo "$(bold "─── $1 ───")"
    echo ""
}

# Flush Redis to ensure clean state
flush_redis() {
    docker exec redis-test redis-cli FLUSHALL > /dev/null 2>&1 || true
}

# ── Pre-flight checks ─────────────────────────────────────────────────────────

# Verify service is reachable
status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$BASE_URL/healthz" 2>/dev/null || echo "000")
if [ "$status" != "200" ]; then
    echo "  $(red "ERROR: Service at $BASE_URL is not reachable (HTTP $status)")"
    echo "  Make sure the dedup-service is running."
    exit 1
fi

# Check if hey is available
if ! command -v hey &> /dev/null; then
    echo "  $(yellow 'ERROR: hey not found. Install with: go install github.com/rakyll/hey@latest')"
    exit 1
fi

# ============================================================================
#  LOAD TESTS
# ============================================================================

separator "LOAD TESTS"

flush_redis

echo "  Config: $LOAD_N requests, $LOAD_C concurrency"
echo "  Target: $BASE_URL"
echo ""

# ── Load test 1: GET /healthz ────────────────────────────────────────

echo "$(bold '1. GET /healthz')"
echo ""
hey -n "$LOAD_N" -c "$LOAD_C" "$BASE_URL/healthz" 2>&1 | grep -E "Requests/sec|Fastest|Slowest|Average|Total:|Status code|10%|50%|90%|95%|99%"
echo ""

# ── Load test 2: POST /api/orders (duplicate bodies → 409) ──────────

echo "$(bold '2. POST /api/orders (duplicate detection → 409s)')"
echo ""
hey -n "$LOAD_N" -c "$LOAD_C" \
    -m POST \
    -H "Content-Type: application/json" \
    -d '{"id":"loadtest-dup","data":"same-payload"}' \
	"$BASE_URL/api/orders" 2>&1 | grep -E "Requests/sec|Fastest|Slowest|Average|Total:|Status code|10%|50%|90%|95%|99%"
echo ""

# ── Load test 3: POST /api/orders (unique bodies → 200s) ─────────────

echo "$(bold '3. POST /api/orders (unique payloads → 200s)')"
echo ""
echo "  Running custom Go load test for unique payloads..."
echo ""

LOADTEST_SRC=$(mktemp --suffix=.go)
cat > "$LOADTEST_SRC" << 'GOEOF'
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	total := 2000
	concurrency := 50
	url := "http://localhost:8081/api/orders"

	if v := os.Getenv("LOAD_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			total = n
		}
	}
	if v := os.Getenv("LOAD_C"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			concurrency = n
		}
	}
	if v := os.Getenv("BASE_URL"); v != "" {
		url = v + "/api/orders"
	}

	var (
		ok200, dup409, other int64
		wg                   sync.WaitGroup
		sem                  = make(chan struct{}, concurrency)
		latencies            = make([]time.Duration, total)
	)

	start := time.Now()
	for i := 0; i < total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			body := fmt.Sprintf(`{"id":"lt-uniq-%d","ts":"%d"}`, idx, time.Now().UnixNano())
			t0 := time.Now()
			resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
			latencies[idx] = time.Since(t0)
			if err != nil {
				atomic.AddInt64(&other, 1)
				return
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case 200:
				atomic.AddInt64(&ok200, 1)
			case 409:
				atomic.AddInt64(&dup409, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var sum time.Duration
	mn, mx := latencies[0], latencies[0]
	for _, l := range latencies {
		sum += l
		if l < mn { mn = l }
		if l > mx { mx = l }
	}

	sorted := make([]time.Duration, total)
	copy(sorted, latencies)
	for i := 1; i < len(sorted); i++ {
		k := sorted[i]; j := i - 1
		for j >= 0 && sorted[j] > k { sorted[j+1] = sorted[j]; j-- }
		sorted[j+1] = k
	}

	fmt.Printf("  Total:        %d requests\n", total)
	fmt.Printf("  Concurrency:  %d\n", concurrency)
	fmt.Printf("  Elapsed:      %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Requests/sec: %.2f\n\n", float64(total)/elapsed.Seconds())
	fmt.Printf("  Latency (avg):  %v\n", (sum / time.Duration(total)).Round(100*time.Microsecond))
	fmt.Printf("  Latency (min):  %v\n", mn.Round(100*time.Microsecond))
	fmt.Printf("  Latency (max):  %v\n", mx.Round(100*time.Microsecond))
	fmt.Printf("  Latency p50:    %v\n", sorted[total*50/100].Round(100*time.Microsecond))
	fmt.Printf("  Latency p90:    %v\n", sorted[total*90/100].Round(100*time.Microsecond))
	fmt.Printf("  Latency p95:    %v\n", sorted[total*95/100].Round(100*time.Microsecond))
	fmt.Printf("  Latency p99:    %v\n\n", sorted[total*99/100].Round(100*time.Microsecond))
	fmt.Printf("  Status codes:\n")
	fmt.Printf("    [200] %d responses\n", ok200)
	if dup409 > 0 { fmt.Printf("    [409] %d responses\n", dup409) }
	if other > 0  { fmt.Printf("    [other] %d responses\n", other) }
}
GOEOF

LOAD_N="$LOAD_N" LOAD_C="$LOAD_C" BASE_URL="$BASE_URL" go run "$LOADTEST_SRC"
rm -f "$LOADTEST_SRC"

echo ""

# ── Summary ──────────────────────────────────────────────────────────

separator "LOAD TEST COMPLETE"
echo "  All load tests finished successfully."
echo "  Tip: set LOAD_N and LOAD_C env vars to adjust (e.g. LOAD_N=5000 LOAD_C=100)"
