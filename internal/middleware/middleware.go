// Package middleware provides Gin middleware for the dedup service.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/metrics"
)

// RequestIDHeader is the HTTP header used for request correlation.
const RequestIDHeader = "X-Request-ID"

// hstsHeaderValue is the default Strict-Transport-Security policy.
const hstsHeaderValue = "max-age=31536000; includeSubDomains"

// cspHeaderValue is a restrictive CSP suitable for JSON-only API responses.
const cspHeaderValue = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

const (
	// Gin context keys
	requestIDKey = "request_id"

	// Zerolog field keys
	logFieldRequestID  = "request_id"
	logFieldMethod     = "method"
	logFieldPath       = "path"
	logFieldStatus     = "status"
	logFieldDurationMS = "duration_ms"
	logFieldPanic      = "panic"
	logMessage         = "http"
	logMessagePanic    = "panic recovered"

	// JSON field keys and values
	jsonKeyError      = "error"
	jsonErrorInternal = "internal_error"
	jsonErrorNotFound = "not_found"
)

// statusText maps commonly seen HTTP status codes to their string form,
// avoiding a strconv.Itoa allocation on every request.
var statusText = map[int]string{
	200: "200", 201: "201", 204: "204",
	301: "301", 302: "302", 304: "304",
	400: "400", 401: "401", 403: "403", 404: "404",
	405: "405", 408: "408", 409: "409",
	500: "500", 502: "502", 503: "503", 504: "504",
}

func statusStr(code int) string {
	if s, ok := statusText[code]; ok {
		return s
	}
	return strconv.Itoa(code)
}

// RequestID returns Gin middleware that ensures every request has a unique
// X-Request-ID header. If the client provides one, it is reused; otherwise
// a random 16-byte hex ID is generated. The ID is added to the response
// headers and stored in the Gin context for downstream use.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			var buf [16]byte
			_, _ = rand.Read(buf[:]) // #nosec G104 -- crypto/rand.Read never returns error in Go 1.20+
			id = hex.EncodeToString(buf[:])
		}
		c.Set(requestIDKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// Logging returns Gin middleware that emits a structured zerolog line for every
// request including the HTTP method, path, response status, and wall-clock duration.
// If disableRequestLogging is true, only errors and slow requests (>100ms) are logged.
// This is useful for low-latency/high-throughput deployments like high-frequency trading.
func Logging(logger zerolog.Logger, disableRequestLogging bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		duration := time.Since(start)

		// If request logging disabled, only log errors and slow requests.
		if disableRequestLogging {
			if status >= 400 || duration > 100*time.Millisecond {
				logger.Warn().
					Str(logFieldRequestID, c.GetString(requestIDKey)).
					Str(logFieldMethod, c.Request.Method).
					Str(logFieldPath, c.Request.URL.Path).
					Int(logFieldStatus, status).
					Int64(logFieldDurationMS, duration.Milliseconds()).
					Msg(logMessage)
			}
			return
		}

		// Choose log level based on status and latency to reduce I/O
		// at high throughput. Normal 2xx goes to Debug; slow or error
		// responses are elevated so they always surface.
		var evt *zerolog.Event
		switch {
		case status >= 500:
			evt = logger.Error()
		case (status >= 400 && status != http.StatusConflict) || duration > 100*time.Millisecond:
			evt = logger.Warn()
		default:
			evt = logger.Debug()
		}
		evt.
			Str(logFieldRequestID, c.GetString(requestIDKey)).
			Str(logFieldMethod, c.Request.Method).
			Str(logFieldPath, c.Request.URL.Path).
			Int(logFieldStatus, status).
			Int64(logFieldDurationMS, duration.Milliseconds()).
			Msg(logMessage)
	}
}

// Recovery returns Gin middleware that catches panics, logs them via zerolog,
// and returns HTTP 500 rather than crashing the process.
func Recovery(logger zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error().
					Interface(logFieldPanic, rec).
					Str(logFieldMethod, c.Request.Method).
					Str(logFieldPath, c.Request.URL.Path).
					Msg(logMessagePanic)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					jsonKeyError: jsonErrorInternal,
				})
			}
		}()
		c.Next()
	}
}

// Metrics returns Gin middleware that records Prometheus metrics for every request.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := statusStr(c.Writer.Status())
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		method := c.Request.Method

		metrics.RequestsTotal.WithLabelValues(method, path, status).Inc()
		metrics.RequestDuration.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	}
}

// SecurityHeaders returns Gin middleware that sets various security-related
// headers to sensible defaults for APIs.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			c.Header("Strict-Transport-Security", hstsHeaderValue)
		}
		c.Header("Content-Security-Policy", cspHeaderValue)
		c.Next()
	}
}

// NotFound returns a Gin handler for unmatched routes.
func NotFound() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			jsonKeyError: jsonErrorNotFound,
		})
	}
}
