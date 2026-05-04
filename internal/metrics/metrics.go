// Package metrics provides Prometheus instrumentation for the dedup service.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// hftBuckets provides sub-millisecond resolution for high-frequency workloads.
// Range: 100µs → 250ms (vs default DefBuckets 5ms → 10s).
var hftBuckets = []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25}

var (
	// RequestsTotal counts all incoming HTTP requests by method, path, and status code.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dedup",
		Name:      "http_requests_total",
		Help:      "Total number of HTTP requests processed, labelled by method, path, and status code.",
	}, []string{"method", "path", "status"})

	// RequestDuration observes HTTP request latency in seconds.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dedup",
		Name:      "http_request_duration_seconds",
		Help:      "Histogram of HTTP request durations in seconds.",
		Buckets:   hftBuckets,
	}, []string{"method", "path"})

	// DedupChecksTotal counts dedup-check outcomes.
	DedupChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dedup",
		Name:      "checks_total",
		Help:      "Total dedup checks by outcome: allowed, duplicate, error, excluded.",
	}, []string{"outcome"})

	// StoreLatency observes Redis/store operation latency.
	StoreLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dedup",
		Name:      "store_latency_seconds",
		Help:      "Histogram of store (Redis) operation durations.",
		Buckets:   hftBuckets,
	}, []string{"operation"})

	// CacheHitsTotal counts L1 local cache hits (Redis round-trip avoided).
	CacheHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "dedup",
		Name:      "cache_hits_total",
		Help:      "Total L1 cache hits (Redis round-trip avoided).",
	})

	// CacheMissesTotal counts L1 cache misses (fell through to Redis).
	CacheMissesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "dedup",
		Name:      "cache_misses_total",
		Help:      "Total L1 cache misses (fell through to Redis).",
	})

	// Pre-resolved counters for fixed dedup outcomes — avoids a label
	// map lookup on every request.
	DedupAllowed   = DedupChecksTotal.WithLabelValues("allowed")
	DedupDuplicate = DedupChecksTotal.WithLabelValues("duplicate")
	DedupError     = DedupChecksTotal.WithLabelValues("error")
	DedupExcluded  = DedupChecksTotal.WithLabelValues("excluded")

	// Pre-resolved observer for the IsDuplicate store call.
	StoreIsDuplicateLatency = StoreLatency.WithLabelValues("is_duplicate")
)
