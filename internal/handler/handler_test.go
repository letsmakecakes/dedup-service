package handler_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/handler"
	"github.com/yourorg/dedup-service/internal/store"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func silentLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

func baseCfg() *config.Config {
	cfg := &config.Config{
		DedupWindow:    10 * time.Second,
		MaxBodyBytes:   65536,
		FailOpen:       true,
		ExcludeMethods: []string{"GET", "HEAD", "OPTIONS"},
	}
	cfg.BuildExcludeSet()
	return cfg
}

// serveDedup runs a single request through the DedupHandler using a Gin engine.
func serveDedup(h *handler.DedupHandler, method, uri, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, uri, reqBody)
	router.ServeHTTP(rr, req)
	return rr
}

// serveHealth runs a single request through the HealthHandler using a Gin engine.
func serveHealth(h *handler.HealthHandler) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	router := gin.New()
	router.GET("/healthz", h.Handle)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(rr, req)
	return rr
}

func newDedup(cfg *config.Config, s store.Store) *handler.DedupHandler {
	return handler.NewDedup(cfg, s, silentLogger())
}

// ── Allow / reject ────────────────────────────────────────────────────────────

func TestFirstRequestAllowed(t *testing.T) {
	h := newDedup(baseCfg(), store.NewMemory())
	rr := serveDedup(h, "POST", "/dedup-check", `{"amount":100}`)
	assertStatus(t, rr, http.StatusOK)
}

func TestDuplicateRequestRejected(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)
	serveDedup(h, "POST", "/dedup-check", `{"amount":100}`)
	rr := serveDedup(h, "POST", "/dedup-check", `{"amount":100}`)
	assertStatus(t, rr, http.StatusConflict)
	assertJSONError(t, rr, "duplicate_request")
}

func TestDifferentBodyNotDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)
	serveDedup(h, "POST", "/dedup-check", `{"amount":100}`)
	rr := serveDedup(h, "POST", "/dedup-check", `{"amount":999}`)
	assertStatus(t, rr, http.StatusOK)
}

func TestDifferentURINotDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)
	// Note: Gin routing means the URI path is matched to /dedup-check,
	// but the fingerprint uses RequestURI which includes the full path.
	// We use query params to differentiate.
	serveDedup(h, "POST", "/dedup-check?id=1", `{}`)
	rr := serveDedup(h, "POST", "/dedup-check?id=2", `{}`)
	assertStatus(t, rr, http.StatusOK)
}

func TestDifferentMethodNotDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)
	serveDedup(h, "POST", "/dedup-check", `{}`)
	rr := serveDedup(h, "PUT", "/dedup-check", `{}`)
	assertStatus(t, rr, http.StatusOK)
}

// ── Identity headers do not affect dedup ─────────────────────────────────────

func TestDifferentAuthIsDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)

	rr1 := httptest.NewRecorder()
	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	r1 := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(`{"amount":100}`))
	r1.Header.Set("Authorization", "Bearer userA")
	router.ServeHTTP(rr1, r1)

	rr2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(`{"amount":100}`))
	r2.Header.Set("Authorization", "Bearer userB")
	router.ServeHTTP(rr2, r2)

	assertStatus(t, rr2, http.StatusConflict)
}

func TestDifferentClientIPIsDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)

	rr1 := httptest.NewRecorder()
	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	r1 := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(`{"amount":100}`))
	r1.RemoteAddr = "1.2.3.4:5000"
	router.ServeHTTP(rr1, r1)

	rr2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(`{"amount":100}`))
	r2.RemoteAddr = "9.9.9.9:5000"
	router.ServeHTTP(rr2, r2)

	assertStatus(t, rr2, http.StatusConflict)
}

// ── Excluded methods ──────────────────────────────────────────────────────────

func TestExcludedMethodsNeverDeduplicated(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		for i := 0; i < 5; i++ {
			rr := serveDedup(h, method, "/dedup-check", "")
			assertStatus(t, rr, http.StatusOK)
		}
	}
}

// ── Fail-open / fail-closed ───────────────────────────────────────────────────

func TestFailOpenOnStoreError(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	cfg := baseCfg()
	cfg.FailOpen = true
	rr := serveDedup(newDedup(cfg, s), "POST", "/dedup-check", `{}`)
	assertStatus(t, rr, http.StatusOK)
}

func TestFailClosedOnStoreError(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	cfg := baseCfg()
	cfg.FailOpen = false
	rr := serveDedup(newDedup(cfg, s), "POST", "/dedup-check", `{}`)
	assertStatus(t, rr, http.StatusInternalServerError)
	assertJSONError(t, rr, "store_unavailable")
}

// ── Window expiry ─────────────────────────────────────────────────────────────

func TestDedupWindowExpiry(t *testing.T) {
	cfg := baseCfg()
	cfg.DedupWindow = 40 * time.Millisecond
	s := store.NewMemory()
	h := newDedup(cfg, s)

	serveDedup(h, "POST", "/dedup-check", `{"ref":"x"}`)
	rr := serveDedup(h, "POST", "/dedup-check", `{"ref":"x"}`)
	assertStatus(t, rr, http.StatusConflict)

	time.Sleep(80 * time.Millisecond)

	rr2 := serveDedup(h, "POST", "/dedup-check", `{"ref":"x"}`)
	assertStatus(t, rr2, http.StatusOK)
}

// ── Response headers ──────────────────────────────────────────────────────────

func TestConflictResponseHasJSONContentType(t *testing.T) {
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)
	serveDedup(h, "POST", "/dedup-check", `{}`)
	rr := serveDedup(h, "POST", "/dedup-check", `{}`)
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

// ── X-Original-* header support (Nginx auth_request) ──────────────────────────

func TestXOriginalMethodOverridesRequestMethod(t *testing.T) {
	// Simulate Nginx auth_request: sub-request arrives as GET, but
	// X-Original-Method is POST → handler should treat as POST (dedup-active).
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)

	rr := httptest.NewRecorder()
	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	// First request via GET with X-Original-Method: POST.
	r1 := httptest.NewRequest("GET", "/dedup-check", strings.NewReader(`{"k":"v"}`))
	r1.Header.Set("X-Original-Method", "POST")
	r1.Header.Set("X-Original-URI", "/api/orders")
	router.ServeHTTP(rr, r1)
	assertStatus(t, rr, http.StatusOK)

	// Same payload → duplicate. Behind auth_request → 403 (Nginx maps to 409).
	rr2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/dedup-check", strings.NewReader(`{"k":"v"}`))
	r2.Header.Set("X-Original-Method", "POST")
	r2.Header.Set("X-Original-URI", "/api/orders")
	router.ServeHTTP(rr2, r2)
	assertStatus(t, rr2, http.StatusForbidden)
}

func TestXOriginalMethodGETStillExcluded(t *testing.T) {
	// If the original method is GET, it should still be excluded.
	h := newDedup(baseCfg(), store.NewMemory())

	rr := httptest.NewRecorder()
	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	req := httptest.NewRequest("GET", "/dedup-check", nil)
	req.Header.Set("X-Original-Method", "GET")
	router.ServeHTTP(rr, req)
	assertStatus(t, rr, http.StatusOK)
}

func TestXOriginalURIUsedInFingerprint(t *testing.T) {
	// Different X-Original-URI → different fingerprint → not a duplicate.
	s := store.NewMemory()
	h := newDedup(baseCfg(), s)

	rr := httptest.NewRecorder()
	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	r1 := httptest.NewRequest("GET", "/dedup-check", strings.NewReader(`{}`))
	r1.Header.Set("X-Original-Method", "POST")
	r1.Header.Set("X-Original-URI", "/api/orders")
	router.ServeHTTP(rr, r1)
	assertStatus(t, rr, http.StatusOK)

	rr2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/dedup-check", strings.NewReader(`{}`))
	r2.Header.Set("X-Original-Method", "POST")
	r2.Header.Set("X-Original-URI", "/api/payments")
	router.ServeHTTP(rr2, r2)
	assertStatus(t, rr2, http.StatusOK) // different URI, not duplicate
}

// ── /healthz ──────────────────────────────────────────────────────────────────

func TestHealthOK(t *testing.T) {
	h := handler.NewHealth(store.NewMemory(), silentLogger())
	rr := serveHealth(h)
	assertStatus(t, rr, http.StatusOK)
	assertJSONField(t, rr, "status", "ok")
}

func TestHealthUnhealthy(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	h := handler.NewHealth(s, silentLogger())
	rr := serveHealth(h)
	assertStatus(t, rr, http.StatusServiceUnavailable)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func assertStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Errorf("expected HTTP %d, got %d (body: %s)", want, rr.Code, rr.Body.String())
	}
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, wantError string) {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("could not decode JSON response: %v", err)
	}
	if body["error"] != wantError {
		t.Errorf("expected error=%q, got %q", wantError, body["error"])
	}
}

func assertJSONField(t *testing.T, rr *httptest.ResponseRecorder, field, want string) {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("could not decode JSON response: %v", err)
	}
	if body[field] != want {
		t.Errorf("expected %s=%q, got %q", field, want, body[field])
	}
}
