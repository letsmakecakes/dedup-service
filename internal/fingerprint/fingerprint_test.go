package fingerprint_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/dedup-service/internal/fingerprint"
)

// req builds a raw HTTP request exactly as Nginx forwards it — method, URI,
// and body set directly, no X-Original-* or identity headers.
func req(method, uri, body string) *http.Request {
	if body != "" {
		return httptest.NewRequest(method, uri, strings.NewReader(body))
	}
	return httptest.NewRequest(method, uri, nil)
}

// ── Determinism ───────────────────────────────────────────────────────────────

func TestDeterminism(t *testing.T) {
	fp1, _ := fingerprint.FromHTTP(req("POST", "/api/orders", `{"amount":100}`))
	fp2, _ := fingerprint.FromHTTP(req("POST", "/api/orders", `{"amount":100}`))
	if fp1.Hash() != fp2.Hash() {
		t.Errorf("same inputs produced different hashes:\n  %s\n  %s", fp1.Hash(), fp2.Hash())
	}
}

// ── Each field differentiates ─────────────────────────────────────────────────

func TestMethodDifferentiates(t *testing.T) {
	fp1, _ := fingerprint.FromHTTP(req("POST", "/api/orders", `{}`))
	fp2, _ := fingerprint.FromHTTP(req("PUT", "/api/orders", `{}`))
	assertDifferent(t, "method", fp1, fp2)
}

func TestURIDifferentiates(t *testing.T) {
	fp1, _ := fingerprint.FromHTTP(req("POST", "/api/orders/1", `{}`))
	fp2, _ := fingerprint.FromHTTP(req("POST", "/api/orders/2", `{}`))
	assertDifferent(t, "URI", fp1, fp2)
}

func TestQueryStringDifferentiates(t *testing.T) {
	fp1, _ := fingerprint.FromHTTP(req("POST", "/api/search?q=foo", `{}`))
	fp2, _ := fingerprint.FromHTTP(req("POST", "/api/search?q=bar", `{}`))
	assertDifferent(t, "query string", fp1, fp2)
}

func TestBodyDifferentiates(t *testing.T) {
	fp1, _ := fingerprint.FromHTTP(req("POST", "/api/orders", `{"amount":100}`))
	fp2, _ := fingerprint.FromHTTP(req("POST", "/api/orders", `{"amount":200}`))
	assertDifferent(t, "body", fp1, fp2)
}

// ── Identity headers must NOT affect the fingerprint ──────────────────────────

// Authorization, session headers, and client IP must all be ignored —
// they are not part of the fingerprint by design.
func TestAuthHeaderIgnored(t *testing.T) {
	r1 := req("POST", "/api/orders", `{"amount":100}`)
	r1.Header.Set("Authorization", "Bearer userA")

	r2 := req("POST", "/api/orders", `{"amount":100}`)
	r2.Header.Set("Authorization", "Bearer userB")

	fp1, _ := fingerprint.FromHTTP(r1)
	fp2, _ := fingerprint.FromHTTP(r2)
	assertSame(t, "Authorization header must not affect fingerprint", fp1, fp2)
}

func TestSessionHeaderIgnored(t *testing.T) {
	r1 := req("POST", "/api/orders", `{"amount":100}`)
	r1.Header.Set("X-Device-ID", "device-A")

	r2 := req("POST", "/api/orders", `{"amount":100}`)
	r2.Header.Set("X-Device-ID", "device-B")

	fp1, _ := fingerprint.FromHTTP(r1)
	fp2, _ := fingerprint.FromHTTP(r2)
	assertSame(t, "Session header must not affect fingerprint", fp1, fp2)
}

func TestClientIPIgnored(t *testing.T) {
	r1 := req("POST", "/api/orders", `{"amount":100}`)
	r1.RemoteAddr = "1.2.3.4:9000"

	r2 := req("POST", "/api/orders", `{"amount":100}`)
	r2.RemoteAddr = "9.9.9.9:9000"

	fp1, _ := fingerprint.FromHTTP(r1)
	fp2, _ := fingerprint.FromHTTP(r2)
	assertSame(t, "Client IP must not affect fingerprint", fp1, fp2)
}

func TestXOriginalHeadersIgnored(t *testing.T) {
	base := req("POST", "/api/orders", `{}`)
	fpBase, _ := fingerprint.FromHTTP(base)

	withProxy := req("POST", "/api/orders", `{}`)
	withProxy.Header.Set("X-Original-Method", "DELETE")
	withProxy.Header.Set("X-Original-URI", "/api/other")
	withProxy.Header.Set("X-Original-Auth", "Bearer other")
	fpWithProxy, _ := fingerprint.FromHTTP(withProxy)

	assertSame(t, "X-Original-* headers must not affect fingerprint", fpBase, fpWithProxy)
}

// ── Full body hashing ────────────────────────────────────────────────────────

func TestBodyReadFully(t *testing.T) {
	const oldMaxBodyBytes = 65536
	large := strings.Repeat("x", oldMaxBodyBytes+100)
	fp, err := fingerprint.FromHTTP(req("POST", "/api/orders", large))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(fp.Body), len(large); got != want {
		t.Errorf("body should be read fully, got %d bytes, want %d", got, want)
	}
}

// ── Redis key format ──────────────────────────────────────────────────────────

func TestRedisKeyPrefix(t *testing.T) {
	fp, _ := fingerprint.FromHTTP(req("POST", "/api/orders", `{}`))
	key := fp.RedisKey()
	if !strings.HasPrefix(key, "dedup:") {
		t.Errorf("Redis key missing 'dedup:' prefix: %s", key)
	}
	// "dedup:" (6) + SHA-256 hex (64) = 70
	if len(key) != 70 {
		t.Errorf("unexpected Redis key length %d: %s", len(key), key)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

type hasher interface{ Hash() string }

func assertDifferent(t *testing.T, field string, a, b hasher) {
	t.Helper()
	if a.Hash() == b.Hash() {
		t.Errorf("different %s should produce different hashes, but both gave %s", field, a.Hash())
	}
}

func assertSame(t *testing.T, msg string, a, b hasher) {
	t.Helper()
	if a.Hash() != b.Hash() {
		t.Errorf("%s:\n  got  %s\n  want %s", msg, a.Hash(), b.Hash())
	}
}
