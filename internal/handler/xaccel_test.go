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

const testRedirectPrefix = "/internal/upstream"

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

func assertRespStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected HTTP %d, got %d (body: %s)", want, resp.StatusCode, string(body))
	}
}

// serveXAccel runs a request through the XAccelDedupHandler using a real test server.
func serveXAccel(h *handler.XAccelDedupHandler, method, uri, body string) *http.Response {
	router := gin.New()
	router.Any("/*path", h.Handle)
	ts := httptest.NewServer(router)
	defer ts.Close()

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+uri, reqBody)
	// Prevent Go's HTTP client from following the internal redirect header.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func newXAccelHandler(t *testing.T, cfg *config.Config, s store.Store) *handler.XAccelDedupHandler {
	t.Helper()
	return handler.NewXAccelDedup(cfg, s, silentLogger(), testRedirectPrefix)
}

// ── X-Accel-Redirect mode tests ───────────────────────────────────────────────

func TestXAccelFirstRequestAllowed(t *testing.T) {
	h := newXAccelHandler(t, baseCfg(), store.NewMemory())
	resp := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)

	redirect := resp.Header.Get("X-Accel-Redirect")
	if redirect != testRedirectPrefix+"/api/orders" {
		t.Errorf("expected X-Accel-Redirect %q, got %q", testRedirectPrefix+"/api/orders", redirect)
	}
}

func TestXAccelDuplicateBodyRejected(t *testing.T) {
	s := store.NewMemory()
	h := newXAccelHandler(t, baseCfg(), s)

	r1 := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	r1.Body.Close()

	resp := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusConflict)

	if resp.Header.Get("X-Accel-Redirect") != "" {
		t.Error("duplicate should NOT have X-Accel-Redirect header")
	}
}

func TestXAccelDifferentBodyNotDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newXAccelHandler(t, baseCfg(), s)

	r1 := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	r1.Body.Close()

	resp := serveXAccel(h, "POST", "/api/orders", `{"amount":999}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)

	if resp.Header.Get("X-Accel-Redirect") == "" {
		t.Error("different body should have X-Accel-Redirect header")
	}
}

func TestXAccelSameBodyDifferentURINotDuplicate(t *testing.T) {
	s := store.NewMemory()
	h := newXAccelHandler(t, baseCfg(), s)

	r1 := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	r1.Body.Close()

	resp := serveXAccel(h, "POST", "/api/payments", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)

	want := testRedirectPrefix + "/api/payments"
	if got := resp.Header.Get("X-Accel-Redirect"); got != want {
		t.Errorf("expected X-Accel-Redirect %q, got %q", want, got)
	}
}

func TestXAccelGETBypassesDedup(t *testing.T) {
	s := store.NewMemory()
	h := newXAccelHandler(t, baseCfg(), s)

	r1 := serveXAccel(h, "GET", "/api/orders", "")
	r1.Body.Close()
	assertRespStatus(t, r1, http.StatusOK)

	r2 := serveXAccel(h, "GET", "/api/orders", "")
	defer r2.Body.Close()
	assertRespStatus(t, r2, http.StatusOK)

	// Both should have X-Accel-Redirect (GET is excluded from dedup).
	for _, r := range []*http.Response{r1, r2} {
		if r.Header.Get("X-Accel-Redirect") == "" {
			t.Error("GET should always have X-Accel-Redirect header")
		}
	}
}

func TestXAccelFailOpenRedirects(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	cfg := baseCfg()
	cfg.FailOpen = true
	h := newXAccelHandler(t, cfg, s)

	resp := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)

	if resp.Header.Get("X-Accel-Redirect") == "" {
		t.Error("fail-open should have X-Accel-Redirect header")
	}
}

func TestXAccelFailClosedRejects(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	cfg := baseCfg()
	cfg.FailOpen = false
	h := newXAccelHandler(t, cfg, s)

	resp := serveXAccel(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusInternalServerError)
}

func TestXAccelWindowExpiry(t *testing.T) {
	cfg := baseCfg()
	cfg.DedupWindow = 40 * time.Millisecond
	s := store.NewMemory()
	h := newXAccelHandler(t, cfg, s)

	r1 := serveXAccel(h, "POST", "/api/orders", `{"ref":"x"}`)
	r1.Body.Close()

	r2 := serveXAccel(h, "POST", "/api/orders", `{"ref":"x"}`)
	r2.Body.Close()
	assertRespStatus(t, r2, http.StatusConflict)

	time.Sleep(80 * time.Millisecond)

	r3 := serveXAccel(h, "POST", "/api/orders", `{"ref":"x"}`)
	defer r3.Body.Close()
	assertRespStatus(t, r3, http.StatusOK)

	if r3.Header.Get("X-Accel-Redirect") == "" {
		t.Error("after window expiry, should have X-Accel-Redirect header")
	}
}

func TestXAccelRedirectPrefixConcatenation(t *testing.T) {
	h := newXAccelHandler(t, baseCfg(), store.NewMemory())

	resp := serveXAccel(h, "POST", "/api/v2/orders?ref=abc", `{"x":1}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)

	want := testRedirectPrefix + "/api/v2/orders?ref=abc"
	if got := resp.Header.Get("X-Accel-Redirect"); got != want {
		t.Errorf("expected X-Accel-Redirect %q, got %q", want, got)
	}
}

func TestHealthOK(t *testing.T) {
	h := handler.NewHealth(store.NewMemory(), silentLogger())

	rr := httptest.NewRecorder()
	router := gin.New()
	router.GET("/healthz", h.Handle)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP %d, got %d", http.StatusOK, rr.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("could not decode JSON response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", body["status"])
	}
}

func TestHealthUnhealthy(t *testing.T) {
	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	h := handler.NewHealth(s, silentLogger())

	rr := httptest.NewRecorder()
	router := gin.New()
	router.GET("/healthz", h.Handle)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}
}
