// Package handler implements the HTTP handlers exposed by the dedup service.
//
// Endpoints:
//
//	GET /healthz  — liveness/readiness; returns 200 when Redis is reachable
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/store"
)

const jsonContentType = "application/json; charset=utf-8"

// Pre-serialised JSON responses to avoid json.Marshal per request.
var (
	duplicateJSON  = []byte(`{"error":"duplicate_request","details":"an identical request was received within the deduplication window"}`)
	storeErrJSON   = []byte(`{"error":"store_unavailable","details":"deduplication store is unreachable; request rejected (fail-closed mode)"}`)
	healthOKJSON   = []byte(`{"status":"ok"}`)
	healthErrJSON  = []byte(`{"error":"store_unavailable"}`)
)

type errorBody struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

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
		c.Data(http.StatusServiceUnavailable, jsonContentType, healthErrJSON)
		return
	}

	if cs, ok := h.store.(*store.CachedStore); ok {
		c.JSON(http.StatusOK, gin.H{
			"status":     "ok",
			"cache_size": cs.CacheLen(),
		})
		return
	}
	c.Data(http.StatusOK, jsonContentType, healthOKJSON)
}
