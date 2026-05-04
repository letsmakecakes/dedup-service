# Grafana Setup for Dedup Service

This guide covers how to configure Prometheus to scrape the dedup service and import the pre-built Grafana dashboard.

## Prerequisites

- Prometheus installed and running (tested with 2.x+)
- Grafana installed and running (tested with 10.x+)
- Dedup service running on port `8081`

---

## 1. Configure Prometheus

Add the following scrape job to your `prometheus.yml`. Replace `<dedup-host>` with the hostname or IP of the machine running the dedup service.

```yaml
global:
  scrape_interval: 5s
  evaluation_interval: 5s

scrape_configs:
  - job_name: "dedup-service"
    metrics_path: /metrics
    static_configs:
      - targets: ["<dedup-host>:8081"]
        labels:
          service: "dedup-service"
```

**Example** — if Prometheus and the dedup service run on the same host:

```yaml
static_configs:
  - targets: ["localhost:8081"]
    labels:
      service: "dedup-service"
```

After editing, reload Prometheus:

```bash
# Signal reload without restart
kill -HUP $(pgrep prometheus)

# Or via the HTTP API if --web.enable-lifecycle is set
curl -X POST http://localhost:9090/-/reload
```

Verify the target is UP at `http://localhost:9090/targets`.

---

## 2. Add Prometheus as a Grafana Datasource

1. Open Grafana → **Connections** → **Data sources** → **Add data source**
2. Select **Prometheus**
3. Set **URL** to your Prometheus address, e.g. `http://localhost:9090`
4. Leave **Access** as `Server (default)`
5. Click **Save & test** — you should see "Successfully queried the Prometheus API"

---

## 3. Import the Pre-built Dashboard

The dashboard JSON is at `monitoring/grafana/dashboards/dedup-service.json`.

**Option A — Grafana UI**

1. Open Grafana → **Dashboards** → **Import**
2. Click **Upload dashboard JSON file**
3. Select `monitoring/grafana/dashboards/dedup-service.json`
4. Under **Prometheus**, select the datasource you added in step 2
5. Click **Import**

**Option B — Grafana Provisioning (recommended for server deployments)**

Copy the dashboard JSON to Grafana's provisioned dashboards directory and add a provisioning config:

```bash
# Copy dashboard
sudo cp monitoring/grafana/dashboards/dedup-service.json \
    /var/lib/grafana/dashboards/

# Copy provisioning config
sudo cp monitoring/grafana/provisioning/dashboards/dashboard.yml \
    /etc/grafana/provisioning/dashboards/

# Copy datasource config (edit URL first if needed)
sudo cp monitoring/grafana/provisioning/datasources/prometheus.yml \
    /etc/grafana/provisioning/datasources/
```

Edit `/etc/grafana/provisioning/datasources/prometheus.yml` and set the correct Prometheus URL:

```yaml
apiVersion: 1

datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://localhost:9090   # <-- update to your Prometheus address
    isDefault: true
    editable: true
```

Then restart Grafana:

```bash
sudo systemctl restart grafana-server
```

---

## 4. Dashboard Panels

The dashboard is organised into five sections:

### Overview

| Panel | Metric | What to watch |
|---|---|---|
| Request Rate | `dedup_http_requests_total` | Steady baseline; sudden drops indicate the service stopped receiving traffic |
| Request Latency | `dedup_http_request_duration_seconds` | p99 > 5ms warrants investigation for a trading workload |

### Deduplication

| Panel | Metric | What to watch |
|---|---|---|
| Dedup Outcomes | `dedup_checks_total` by `outcome` | Normal traffic shows mostly `allowed`; a surge in `duplicate` means retries are happening; `error` means Redis is unreachable |
| Duplicate Rate | ratio of `duplicate` / (`allowed` + `duplicate`) | Spikes above ~5% during market open may indicate network instability |
| L1 Cache Hit Rate | `dedup_cache_hits_total` / total | Should be >90% under normal load; low values mean the in-process cache TTL is too short or memory pressure is evicting entries |

### Redis / Store

| Panel | Metric | What to watch |
|---|---|---|
| Store (Redis) Latency | `dedup_store_latency_seconds` | p99 > 2ms on a local Redis is abnormal |
| L1 Cache Hits vs Misses | `dedup_cache_hits_total`, `dedup_cache_misses_total` | Hits should dominate; a divergence indicates cache churn |

### HTTP Status Codes

| Panel | Metric | What to watch |
|---|---|---|
| Status Code Distribution | `dedup_http_requests_total` by `status` | 200 = allowed, 409 = duplicate blocked, 500 = store error |
| Error Rate (5xx) | `dedup_http_requests_total{status=~"5.."}` | Any sustained 5xx rate requires immediate attention |

### Go Runtime / System

| Panel | Metric | What to watch |
|---|---|---|
| CPU Usage | `process_cpu_seconds_total` | Should be < 1 core at normal load |
| Memory Usage | `go_memstats_heap_inuse_bytes`, `process_resident_memory_bytes` | Steady growth without a ceiling indicates a leak |
| Goroutines & Threads | `go_goroutines`, `go_threads` | Goroutine count should be stable; sustained growth means goroutine leak |
| GC Pause Duration | `go_gc_duration_seconds` | Max pause > 1ms at this workload level should prompt GC tuning (adjust `GOGC` in config) |
| Heap Objects | `go_memstats_heap_objects` | Useful alongside GC pause to identify allocation pressure |
| Open File Descriptors | `process_open_fds` | Should stay well below `process_max_fds`; climbing FDs indicate a connection leak |

---

## 5. Recommended Alert Rules

Add these to your Prometheus `rules.yml`:

```yaml
groups:
  - name: dedup-service
    rules:

      # Store is returning errors — fail-open is active
      - alert: DedupStoreErrors
        expr: rate(dedup_checks_total{outcome="error"}[1m]) > 0
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "Dedup service store errors detected"
          description: "Redis is returning errors. Service is operating in fail-open mode."

      # Duplicate rate spike — unusual level of client retries
      - alert: DedupHighDuplicateRate
        expr: |
          sum(rate(dedup_checks_total{outcome="duplicate"}[5m]))
          /
          sum(rate(dedup_checks_total{outcome=~"allowed|duplicate"}[5m]))
          > 0.10
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "High duplicate request rate (>10%)"
          description: "More than 10% of requests are being identified as duplicates. Possible network instability."

      # Redis latency degradation
      - alert: DedupStoreLatencyHigh
        expr: |
          histogram_quantile(0.99,
            sum(rate(dedup_store_latency_seconds_bucket[1m])) by (le)
          ) > 0.005
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Redis p99 latency above 5ms"
          description: "Store (Redis) p99 latency has exceeded 5ms."

      # Sustained 5xx responses
      - alert: DedupHigh5xxRate
        expr: |
          sum(rate(dedup_http_requests_total{status=~"5.."}[1m]))
          /
          sum(rate(dedup_http_requests_total[1m]))
          > 0.01
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Dedup service 5xx error rate above 1%"
          description: "More than 1% of HTTP responses are 5xx errors."

      # Service is down (no scrape data)
      - alert: DedupServiceDown
        expr: up{job="dedup-service"} == 0
        for: 30s
        labels:
          severity: critical
        annotations:
          summary: "Dedup service is unreachable"
          description: "Prometheus cannot scrape the dedup service metrics endpoint."
```

Load the rules file in `prometheus.yml`:

```yaml
rule_files:
  - "rules.yml"
```

---

## 6. Useful PromQL Queries

Quick queries for the Grafana Explore view or ad-hoc debugging:

```promql
# Overall request rate
sum(rate(dedup_http_requests_total[1m]))

# Duplicate rate over last 5 minutes
sum(rate(dedup_checks_total{outcome="duplicate"}[5m]))
  / sum(rate(dedup_checks_total{outcome=~"allowed|duplicate"}[5m]))

# Redis p99 latency
histogram_quantile(0.99, sum(rate(dedup_store_latency_seconds_bucket[1m])) by (le))

# L1 cache hit rate
rate(dedup_cache_hits_total[1m])
  / (rate(dedup_cache_hits_total[1m]) + rate(dedup_cache_misses_total[1m]))

# 5xx error rate
sum(rate(dedup_http_requests_total{status=~"5.."}[1m]))
  / sum(rate(dedup_http_requests_total[1m]))

# Per-endpoint p95 latency
histogram_quantile(0.95,
  sum(rate(dedup_http_request_duration_seconds_bucket[1m])) by (le, path)
)
```
