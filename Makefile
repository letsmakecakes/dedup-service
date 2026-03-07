BINARY    := dedup-service
CMD       := ./cmd/server
.PHONY: build run test test-cover lint tidy clean monitoring-up monitoring-down help

## build: compile binary for the host OS/arch
build:
	mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) $(CMD)

## run: start the service locally (requires Redis on localhost:6379)
run:
	go run $(CMD)

## test: run all unit tests with race detector
test:
	go test ./... -v -race -count=1

## test-cover: run tests and display coverage summary
test-cover:
	go test ./... -coverprofile=coverage.out -race -count=1
	go tool cover -func=coverage.out

## lint: run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

## tidy: tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

## clean: remove build artefacts
clean:
	rm -rf bin/ coverage.out

## monitoring-up: start Prometheus + Grafana (scrapes dedup-service on localhost:8081)
monitoring-up:
	MSYS_NO_PATHCONV=1 docker run -d --name prometheus \
		-p 9090:9090 \
		-v "$(CURDIR)/monitoring/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
		prom/prometheus:latest
	MSYS_NO_PATHCONV=1 docker run -d --name grafana \
		-p 3000:3000 \
		-e GF_SECURITY_ADMIN_USER=admin \
		-e GF_SECURITY_ADMIN_PASSWORD=admin \
		-v "$(CURDIR)/monitoring/grafana/provisioning:/etc/grafana/provisioning:ro" \
		-v "$(CURDIR)/monitoring/grafana/dashboards:/var/lib/grafana/dashboards:ro" \
		grafana/grafana:latest
	@echo ""
	@echo "  Prometheus: http://localhost:9090"
	@echo "  Grafana:    http://localhost:3000  (admin / admin)"
	@echo ""

## monitoring-down: stop and remove Prometheus + Grafana containers
monitoring-down:
	docker rm -f prometheus grafana 2>/dev/null || true

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
