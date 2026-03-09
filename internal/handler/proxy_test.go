package handler_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/handler"
	"github.com/yourorg/dedup-service/internal/store"
)

// mockBackend returns a test server that echoes 200 with a JSON body.
func mockBackend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","source":"upstream"}`)) // #nosec G104
	}))
}

// serveProxy runs a request through the ProxyDedupHandler using a real
// HTTP test server (required because httputil.ReverseProxy needs a full
// http.ResponseWriter, not just an httptest.ResponseRecorder).
func serveProxy(h *handler.ProxyDedupHandler, method, uri, body string) *http.Response {
	router := gin.New()
	router.Any("/*path", h.Handle)
	ts := httptest.NewServer(router)
	defer ts.Close()

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+uri, reqBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// ── Proxy mode: body-based dedup ──────────────────────────────────────────────

func TestProxyFirstRequestProxied(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	h := newProxyDedupHandler(t, baseCfg(), store.NewMemory(), backend.URL)
	resp := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)
	assertRespJSONField(t, resp, "source", "upstream")
}

func TestProxyDuplicateBodyRejected(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	s := store.NewMemory()
	h := newProxyDedupHandler(t, baseCfg(), s, backend.URL)

	r1 := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	r1.Body.Close()

	resp := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusConflict)
}

func TestProxyDifferentBodyNotDuplicate(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	s := store.NewMemory()
	h := newProxyDedupHandler(t, baseCfg(), s, backend.URL)

	r1 := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	r1.Body.Close()

	resp := serveProxy(h, "POST", "/api/orders", `{"amount":999}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)
}

func TestProxySameBodyDifferentURINotDuplicate(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	s := store.NewMemory()
	h := newProxyDedupHandler(t, baseCfg(), s, backend.URL)

	r1 := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	r1.Body.Close()

	resp := serveProxy(h, "POST", "/api/payments", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)
}

func TestProxyGETBypassesDedup(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	s := store.NewMemory()
	h := newProxyDedupHandler(t, baseCfg(), s, backend.URL)

	r1 := serveProxy(h, "GET", "/api/orders", "")
	r1.Body.Close()
	assertRespStatus(t, r1, http.StatusOK)

	r2 := serveProxy(h, "GET", "/api/orders", "")
	defer r2.Body.Close()
	assertRespStatus(t, r2, http.StatusOK)
}

func TestProxyFailOpenProxies(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	cfg := baseCfg()
	cfg.FailOpen = true
	h := newProxyDedupHandler(t, cfg, s, backend.URL)

	resp := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusOK)
	assertRespJSONField(t, resp, "source", "upstream")
}

func TestProxyFailClosedRejects(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	s := store.NewMemory()
	s.Err = store.ErrUnavailable
	cfg := baseCfg()
	cfg.FailOpen = false
	h := newProxyDedupHandler(t, cfg, s, backend.URL)

	resp := serveProxy(h, "POST", "/api/orders", `{"amount":100}`)
	defer resp.Body.Close()
	assertRespStatus(t, resp, http.StatusInternalServerError)
}

func TestProxyWindowExpiry(t *testing.T) {
	backend := mockBackend(t)
	defer backend.Close()

	cfg := baseCfg()
	cfg.DedupWindow = 40 * time.Millisecond
	s := store.NewMemory()
	h := newProxyDedupHandler(t, cfg, s, backend.URL)

	r1 := serveProxy(h, "POST", "/api/orders", `{"ref":"x"}`)
	r1.Body.Close()

	r2 := serveProxy(h, "POST", "/api/orders", `{"ref":"x"}`)
	r2.Body.Close()
	assertRespStatus(t, r2, http.StatusConflict)

	time.Sleep(80 * time.Millisecond)

	r3 := serveProxy(h, "POST", "/api/orders", `{"ref":"x"}`)
	defer r3.Body.Close()
	assertRespStatus(t, r3, http.StatusOK)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newProxyDedupHandler(t *testing.T, cfg *config.Config, s store.Store, backendURL string) *handler.ProxyDedupHandler {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	return handler.NewProxyDedup(cfg, s, silentLogger(), u)
}

func assertRespStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected HTTP %d, got %d (body: %s)", want, resp.StatusCode, string(body))
	}
}

func assertRespJSONField(t *testing.T, resp *http.Response, field, want string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var m map[string]string
	if err := json.NewDecoder(strings.NewReader(string(body))).Decode(&m); err != nil {
		t.Fatalf("could not decode JSON: %v", err)
	}
	if m[field] != want {
		t.Errorf("expected %s=%q, got %q", field, want, m[field])
	}
}
