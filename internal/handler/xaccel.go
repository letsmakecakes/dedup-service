package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/fingerprint"
	"github.com/yourorg/dedup-service/internal/metrics"
	"github.com/yourorg/dedup-service/internal/store"
)

// XAccelDedupHandler handles requests in X-Accel-Redirect mode.
//
// Nginx sends the full client request (including body) to this handler.
// The handler fingerprints method + URI + body, checks Redis, and returns:
//   - 200 with X-Accel-Redirect header → Nginx internally redirects to the
//     real upstream (method and body are preserved across the redirect).
//   - 409 Conflict → Nginx returns the duplicate error to the client.
//
// Nginx remains the router and forwards to target services via internal redirect.
type XAccelDedupHandler struct {
	cfg            *config.Config
	store          store.Store
	logger         zerolog.Logger
	redirectPrefix string // e.g. "/internal/upstream"
}

// NewXAccelDedup constructs an XAccelDedupHandler.
func NewXAccelDedup(cfg *config.Config, s store.Store, logger zerolog.Logger, redirectPrefix string) *XAccelDedupHandler {
	return &XAccelDedupHandler{
		cfg:            cfg,
		store:          s,
		logger:         logger,
		redirectPrefix: redirectPrefix,
	}
}

// Handle is the Gin handler for X-Accel-Redirect dedup mode.
func (h *XAccelDedupHandler) Handle(c *gin.Context) {
	// Skip dedup for excluded methods — redirect to upstream immediately.
	if h.cfg.IsMethodExcluded(c.Request.Method) {
		metrics.DedupChecksTotal.WithLabelValues("excluded").Inc()
		h.allow(c)
		return
	}

	// Stream and hash the request body without buffering the entire body in memory.
	// This avoids large buffer allocations for the hot request path.
	var body io.Reader = c.Request.Body
	if body == nil {
		body = io.NopCloser(strings.NewReader(""))
	}
	body = io.LimitReader(body, h.cfg.MaxBodyBytes)

	key, bodyLen, err := fingerprint.StreamingRedisKey(c.Request.Method, c.Request.RequestURI, body)
	if err != nil {
		h.logger.Error().Err(err).Msg("failed to read or hash request body")
		metrics.DedupChecksTotal.WithLabelValues("error").Inc()
		h.handleStoreErr(c)
		return
	}

	h.logger.Debug().
		Str("key", key).
		Str("method", c.Request.Method).
		Str("uri", c.Request.RequestURI).
		Int64("body_bytes", bodyLen).
		Msg("xaccel dedup check")

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
			Str("method", c.Request.Method).
			Str("uri", c.Request.RequestURI).
			Msg("duplicate request blocked")
		metrics.DedupChecksTotal.WithLabelValues("duplicate").Inc()
		c.Data(http.StatusConflict, jsonContentType, duplicateJSON)
		return
	}

	h.logger.Debug().Str("key", key).Msg("request allowed — X-Accel-Redirect to upstream")
	metrics.DedupChecksTotal.WithLabelValues("allowed").Inc()
	h.allow(c)
}

// allow returns 200 with an X-Accel-Redirect header so Nginx forwards to the
// real upstream backend via an internal redirect.
func (h *XAccelDedupHandler) allow(c *gin.Context) {
	c.Header("X-Accel-Redirect", h.redirectPrefix+c.Request.RequestURI)
	c.Header("Content-Length", "0")
	c.Status(http.StatusOK)
}

// handleStoreErr applies fail-open or fail-closed behaviour.
func (h *XAccelDedupHandler) handleStoreErr(c *gin.Context) {
	if h.cfg.FailOpen {
		h.allow(c)
		return
	}
	c.Data(http.StatusInternalServerError, jsonContentType, storeErrJSON)
}
