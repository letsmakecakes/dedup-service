package middleware

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func silentLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

// ── RequestID ─────────────────────────────────────────────────────────────────

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	router := gin.New()
	router.Use(RequestID())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, c.GetString("request_id"))
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(rr, req)

	id := rr.Header().Get(RequestIDHeader)
	if id == "" {
		t.Fatal("expected X-Request-ID to be generated")
	}
	if len(id) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("expected 32 hex chars, got %d: %q", len(id), id)
	}
	// Should also be in the body (set via Gin context).
	if rr.Body.String() != id {
		t.Errorf("context request_id=%q doesn't match header=%q", rr.Body.String(), id)
	}
}

func TestRequestID_ReusesClientProvided(t *testing.T) {
	router := gin.New()
	router.Use(RequestID())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set(RequestIDHeader, "client-id-123")
	router.ServeHTTP(rr, req)

	if rr.Header().Get(RequestIDHeader) != "client-id-123" {
		t.Errorf("expected client-provided ID, got %q", rr.Header().Get(RequestIDHeader))
	}
}

// ── Logging ───────────────────────────────────────────────────────────────────

func TestLogging_DoesNotPanic(t *testing.T) {
	router := gin.New()
	router.Use(Logging(silentLogger()))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// ── Recovery ──────────────────────────────────────────────────────────────────

func TestRecovery_CatchesPanic(t *testing.T) {
	router := gin.New()
	router.Use(Recovery(silentLogger()))
	router.GET("/panic", func(c *gin.Context) {
		panic("test panic")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panic", nil)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rr.Code)
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────────

func TestMetrics_DoesNotPanic(t *testing.T) {
	router := gin.New()
	router.Use(Metrics())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// ── CSP ──────────────────────────────────────────────────────────────────────

func TestCSP_SetsHeader(t *testing.T) {
	router := gin.New()
	router.Use(CSP())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Security-Policy"); got != cspHeaderValue {
		t.Errorf("expected CSP header %q, got %q", cspHeaderValue, got)
	}
}

// ── HSTS ─────────────────────────────────────────────────────────────────────

func TestHSTS_SetsHeaderForTLSRequest(t *testing.T) {
	router := gin.New()
	router.Use(HSTS())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.TLS = &tls.ConnectionState{}
	router.ServeHTTP(rr, req)

	if got := rr.Header().Get("Strict-Transport-Security"); got != hstsHeaderValue {
		t.Errorf("expected HSTS header %q, got %q", hstsHeaderValue, got)
	}
}

func TestHSTS_DoesNotSetHeaderForHTTPRequest(t *testing.T) {
	router := gin.New()
	router.Use(HSTS())
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(rr, req)

	if got := rr.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("expected no HSTS header on plain HTTP, got %q", got)
	}
}

// ── NotFound ──────────────────────────────────────────────────────────────────

func TestNotFound_Returns404(t *testing.T) {
	router := gin.New()
	router.NoRoute(NotFound())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/does-not-exist", nil)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

// ── statusStr ─────────────────────────────────────────────────────────────────

func TestStatusStr(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{200, "200"},
		{404, "404"},
		{409, "409"},
		{500, "500"},
		{418, "418"}, // not in map, falls through to strconv
	}
	for _, tt := range tests {
		got := statusStr(tt.code)
		if got != tt.want {
			t.Errorf("statusStr(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}
