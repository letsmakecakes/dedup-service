// Package handler implements the HTTP handlers exposed by the dedup service.
//
// Endpoints:
//
//	POST /dedup-check  — called by Nginx auth_request; returns 200 (allow) or 409 (duplicate)
//	GET  /healthz      — liveness/readiness; returns 200 when Redis is reachable
package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/fingerprint"
	"github.com/yourorg/dedup-service/internal/metrics"
	"github.com/yourorg/dedup-service/internal/store"
)

// ── Response types ────────────────────────────────────────────────────────────

type errorBody struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

type healthBody struct {
	Status string `json:"status"`
}

const jsonContentType = "application/json; charset=utf-8"

// Pre-serialised JSON responses — avoids json.Marshal per-request for static bodies.
var (
	duplicateJSON = []byte(`{"error":"duplicate_request","details":"an identical request was received within the deduplication window"}`)
	storeErrJSON  = []byte(`{"error":"store_unavailable","details":"deduplication store is unreachable; request rejected (fail-closed mode)"}`)
	healthOKJSON  = []byte(`{"status":"ok"}`)
)

// ── DedupHandler ──────────────────────────────────────────────────────────────

// DedupHandler handles POST /dedup-check.
type DedupHandler struct {
	cfg    *config.Config
	store  store.Store
	logger zerolog.Logger
}

// NewDedup constructs a DedupHandler.
func NewDedup(cfg *config.Config, s store.Store, logger zerolog.Logger) *DedupHandler {
	return &DedupHandler{cfg: cfg, store: s, logger: logger}
}

// Handle is the Gin handler function for POST /dedup-check.
func (h *DedupHandler) Handle(c *gin.Context) {
	// When behind Nginx auth_request, the sub-request arrives as GET to
	// /dedup-check. Use X-Original-* headers to recover the client's values.
	// Also track whether we're in auth_request mode to return the correct
	// status code (auth_request only handles 200, 401, 403).
	behindAuthRequest := false
	if m := c.GetHeader("X-Original-Method"); m != "" {
		c.Request.Method = m
		behindAuthRequest = true
	}
	if u := c.GetHeader("X-Original-URI"); u != "" {
		c.Request.RequestURI = u
	}

	// Skip dedup for explicitly excluded methods (GET, HEAD, OPTIONS).
	if h.cfg.IsMethodExcluded(c.Request.Method) {
		metrics.DedupChecksTotal.WithLabelValues("excluded").Inc()
		c.Status(http.StatusOK)
		return
	}

	// Build fingerprint from method + URI + body.
	// NOTE: In auth_request mode the body is always empty (Nginx does not
	// forward it), so the fingerprint effectively covers method + URI only.
	fp, err := fingerprint.FromHTTP(c.Request)
	if err != nil {
		h.logger.Error().Err(err).
			Str("method", c.Request.Method).
			Str("uri", c.Request.RequestURI).
			Msg("fingerprint extraction failed")
		metrics.DedupChecksTotal.WithLabelValues("error").Inc()
		h.handleStoreErr(c)
		return
	}

	key := fp.RedisKey()

	h.logger.Debug().
		Str("key", key).
		Str("method", fp.Method).
		Str("uri", fp.URI).
		Int("body_bytes", len(fp.Body)).
		Msg("dedup check")

	// Atomic check-and-set with explicit deadline.
	storeCtx := c.Request.Context()
	if h.cfg.StoreTimeout > 0 {
		var cancel context.CancelFunc
		storeCtx, cancel = context.WithTimeout(storeCtx, h.cfg.StoreTimeout)
		defer cancel()
	}
	t0 := time.Now()
	isDuplicate, err := h.store.IsDuplicate(storeCtx, key, h.cfg.DedupWindow)
	metrics.StoreLatency.WithLabelValues("is_duplicate").Observe(time.Since(t0).Seconds())
	if err != nil {
		if errors.Is(err, store.ErrUnavailable) {
			h.logger.Warn().Err(err).
				Bool("fail_open", h.cfg.FailOpen).
				Msg("store unavailable")
		} else {
			h.logger.Error().Err(err).Msg("store error")
		}
		metrics.DedupChecksTotal.WithLabelValues("error").Inc()
		h.handleStoreErr(c)
		return
	}

	if isDuplicate {
		h.logger.Debug().
			Str("key", key).
			Str("method", fp.Method).
			Str("uri", fp.URI).
			Msg("duplicate request blocked")
		metrics.DedupChecksTotal.WithLabelValues("duplicate").Inc()
		// Nginx auth_request only accepts 200, 401, 403. Return 403 so
		// Nginx triggers error_page → @duplicate_rejected → 409 to client.
		// For direct calls (no X-Original-Method), return 409 directly.
		if behindAuthRequest {
			c.Status(http.StatusForbidden)
		} else {
			c.Data(http.StatusConflict, jsonContentType, duplicateJSON)
		}
		return
	}

	h.logger.Debug().Str("key", key).Msg("request allowed")
	metrics.DedupChecksTotal.WithLabelValues("allowed").Inc()
	c.Status(http.StatusOK)
}

// handleStoreErr applies fail-open or fail-closed behaviour on store errors.
func (h *DedupHandler) handleStoreErr(c *gin.Context) {
	if h.cfg.FailOpen {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusInternalServerError, jsonContentType, storeErrJSON)
}

// ── HealthHandler ─────────────────────────────────────────────────────────────

// HealthHandler handles GET /healthz.
type HealthHandler struct {
	store  store.Store
	logger zerolog.Logger
}

// NewHealth constructs a HealthHandler.
func NewHealth(s store.Store, logger zerolog.Logger) *HealthHandler {
	return &HealthHandler{store: s, logger: logger}
}

// Handle is the Gin handler function for GET /healthz.
func (h *HealthHandler) Handle(c *gin.Context) {
	if err := h.store.Ping(c.Request.Context()); err != nil {
		h.logger.Warn().Err(err).Msg("health check: store ping failed")
		c.JSON(http.StatusServiceUnavailable, errorBody{
			Error:   "store_unavailable",
			Details: err.Error(),
		})
		return
	}

	// Include cache stats if the store is a CachedStore.
	if cs, ok := h.store.(*store.CachedStore); ok {
		c.JSON(http.StatusOK, gin.H{
			"status":     "ok",
			"cache_size": cs.CacheLen(),
		})
		return
	}
	c.Data(http.StatusOK, jsonContentType, healthOKJSON)
}
