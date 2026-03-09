package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/fingerprint"
	"github.com/yourorg/dedup-service/internal/metrics"
	"github.com/yourorg/dedup-service/internal/store"
)

// ProxyDedupHandler acts as a reverse proxy with integrated dedup checking.
// Unlike DedupHandler (designed for Nginx auth_request), this handler receives
// the full client request — including the body — so the fingerprint covers
// method + URI + body.
//
// Flow: read body → fingerprint → check store → proxy allowed | reject duplicate.
type ProxyDedupHandler struct {
	cfg    *config.Config
	store  store.Store
	logger zerolog.Logger
	proxy  *httputil.ReverseProxy
}

// NewProxyDedup constructs a ProxyDedupHandler that forwards allowed requests
// to upstreamURL.
func NewProxyDedup(cfg *config.Config, s store.Store, logger zerolog.Logger, upstreamURL *url.URL) *ProxyDedupHandler {
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	// Preserve the original Host header from the client.
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Host = upstreamURL.Host
	}

	return &ProxyDedupHandler{
		cfg:    cfg,
		store:  s,
		logger: logger,
		proxy:  proxy,
	}
}

// Handle is the Gin handler for reverse-proxy dedup mode.
func (h *ProxyDedupHandler) Handle(c *gin.Context) {
	// Skip dedup for excluded methods — proxy directly.
	if h.cfg.IsMethodExcluded(c.Request.Method) {
		metrics.DedupChecksTotal.WithLabelValues("excluded").Inc()
		h.proxy.ServeHTTP(c.Writer, c.Request)
		return
	}

	// Buffer the body so we can fingerprint it and still forward it.
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

	// Build fingerprint directly from pooled buffer.
	fp := &fingerprint.Request{
		Method: c.Request.Method,
		URI:    c.Request.RequestURI,
		Body:   (*bufp)[:bodyLen],
	}
	key := fp.RedisKey()

	// Copy body for proxying before returning buffer to pool.
	bodyBytes := make([]byte, bodyLen)
	copy(bodyBytes, (*bufp)[:bodyLen])
	fingerprint.PutBodyBuf(bufp)

	h.logger.Debug().
		Str("key", key).
		Str("method", c.Request.Method).
		Str("uri", c.Request.RequestURI).
		Int("body_bytes", bodyLen).
		Msg("proxy dedup check")

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

		if h.cfg.FailOpen {
			// Restore body and proxy on failure when fail-open.
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			h.proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		c.Data(http.StatusInternalServerError, jsonContentType, storeErrJSON)
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

	// Restore body and proxy to upstream.
	h.logger.Debug().Str("key", key).Msg("request allowed — proxying")
	metrics.DedupChecksTotal.WithLabelValues("allowed").Inc()
	c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	h.proxy.ServeHTTP(c.Writer, c.Request)
}

// handleStoreErr applies fail-open or fail-closed behaviour.
func (h *ProxyDedupHandler) handleStoreErr(c *gin.Context) {
	if h.cfg.FailOpen {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusInternalServerError, jsonContentType, storeErrJSON)
}
