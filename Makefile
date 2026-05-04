# =============================================================================
# dedup-service
# =============================================================================
BINARY  := dedup-service
CMD     := ./cmd/server
LDFLAGS := -s -w

.PHONY: build build-linux run \
        test test-short test-cover \
        fmt vet lint tidy security \
        functional-test load-test \
        pprof clean help

# ── Build ─────────────────────────────────────────────────────────────────────

## build: compile binary for the host OS/arch
build:
	mkdir -p bin
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

## build-linux: cross-compile a static Linux/amd64 binary
build-linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 $(CMD)

## run: start the service locally (requires Redis on localhost:6379)
run:
	go run $(CMD)

# ── Test ──────────────────────────────────────────────────────────────────────

## test: run all unit tests with race detector
test:
	go test ./... -race -count=1

## test-short: run tests without race detector (faster feedback)
test-short:
	go test ./... -count=1

## test-cover: run tests and open coverage report
test-cover:
	go test ./... -coverprofile=coverage.out -race -count=1
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## functional-test: run the functional test suite against a live service
functional-test:
	@bash scripts/functional_test.sh

## load-test: run the load test suite against a live service
load-test:
	@bash scripts/load_test.sh

# ── Code quality ──────────────────────────────────────────────────────────────

## fmt: format all Go source files
fmt:
	gofmt -w -s .

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

## tidy: tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

## security: run gosec static security scanner
security:
	gosec -fmt json -out gosec-report.json ./... 2>gosec-stderr.log || true
	@echo "Report written to gosec-report.json"

# ── Monitoring ────────────────────────────────────────────────────────────────

## pprof: open the pprof UI (service must be running; pprof is on 127.0.0.1:6060)
pprof:
	@echo "pprof: http://127.0.0.1:6060/debug/pprof/"
	@open http://127.0.0.1:6060/debug/pprof/ 2>/dev/null || \
		xdg-open http://127.0.0.1:6060/debug/pprof/ 2>/dev/null || true

# ── Housekeeping ──────────────────────────────────────────────────────────────

## clean: remove build artefacts
clean:
	rm -rf bin/ coverage.out coverage.html gosec-report.json gosec-stderr.log

## help: list all available targets
help:
	@echo "Usage: make <target>"
	@echo ""
	@grep -E '^## ' Makefile | sed 's/## /  /'
