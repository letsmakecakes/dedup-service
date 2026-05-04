#!/bin/bash
# ============================================================================
# test_service.sh — Runs functional tests + load tests for the dedup-service
#
# This is a convenience wrapper that runs both scripts in sequence.
# You can also run them independently:
#   bash scripts/functional_test.sh
#   bash scripts/load_test.sh
#
# Prerequisites:
#   - Redis running on localhost:6379
#   - dedup-service running on localhost:8081
#   - hey  (go install github.com/rakyll/hey@latest)
#   - curl
#
# Environment variables:
#   BASE_URL  — service URL (default: http://localhost:8081)
#   LOAD_N    — total requests for load tests (default: 2000)
#   LOAD_C    — concurrency for load tests (default: 50)
# ============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Run functional tests first — abort if any fail
bash "$SCRIPT_DIR/functional_test.sh"

# Run load tests
bash "$SCRIPT_DIR/load_test.sh"
