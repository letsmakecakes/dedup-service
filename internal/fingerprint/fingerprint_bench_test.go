package fingerprint_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/dedup-service/internal/fingerprint"
)

func BenchmarkFromHTTP_SmallBody(b *testing.B) {
	body := `{"amount":100}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest("POST", "/api/orders?ref=abc", strings.NewReader(body))
		fp, _ := fingerprint.FromHTTP(r)
		_ = fp.RedisKey()
	}
}

func BenchmarkFromHTTP_LargeBody(b *testing.B) {
	body := strings.Repeat("x", 65536)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest("POST", "/api/orders?ref=abc", strings.NewReader(body))
		fp, _ := fingerprint.FromHTTP(r)
		_ = fp.RedisKey()
	}
}

func BenchmarkHash(b *testing.B) {
	fp := &fingerprint.Request{
		Method: "POST",
		URI:    "/api/orders?ref=abc",
		Body:   []byte(`{"amount":100}`),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fp.Hash()
	}
}

func BenchmarkRedisKey(b *testing.B) {
	fp := &fingerprint.Request{
		Method: "POST",
		URI:    "/api/orders?ref=abc",
		Body:   []byte(`{"amount":100}`),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fp.RedisKey()
	}
}
