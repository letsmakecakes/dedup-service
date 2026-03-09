package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/fingerprint"
	"github.com/yourorg/dedup-service/internal/metrics"
	"github.com/yourorg/dedup-service/internal/store"
)

// XAccelDedupHandler handles requests in X-Accel-Redirect sidecar mode.
//
// Nginx sends the full client request (including body) to this handler.
// The handler fingerprints method + URI + body, checks Redis, and returns:
//   - 200 with X-Accel-Redirect header → Nginx internally redirects to the
//     real upstream (method and body are preserved across the redirect).
//   - 409 Conflict → Nginx returns the duplicate error to the client.
//
// This avoids the auth_request body limitation while keeping Nginx as the
// router that forwards to target services.
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

	// Read body using a pooled buffer to avoid per-request allocations.
	bufp := fingerprint.GetBodyBuf()
	var bodyLen int
	if c.Request.Body != nil {
		n, err := io.ReadFull(c.Request.Body, *bufp)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			fingerprint.PutBodyBuf(bufp)
			h.logger.Error().Err(err).Msg("failed to read request body")
			metrics.DedupChecksTotal.WithLabelValues("error").Inc()
			h.handleStoreErr(c)
			return
		}
		bodyLen = n
	}

	// Build fingerprint from pooled buffer — key is computed before returning buffer.
	fp := &fingerprint.Request{
		Method: c.Request.Method,
		URI:    c.Request.RequestURI,
		Body:   (*bufp)[:bodyLen],
	}
	key := fp.RedisKey()
	fingerprint.PutBodyBuf(bufp)

	h.logger.Debug().
		Str("key", key).
		Str("method", c.Request.Method).
		Str("uri", c.Request.RequestURI).
		Int("body_bytes", bodyLen).
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
			Str("method", fp.Method).
			Str("uri", fp.URI).
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
