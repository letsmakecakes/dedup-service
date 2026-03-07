package handler_test

import (
	"io"
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

func benchCfg() *config.Config {
	cfg := &config.Config{
		DedupWindow:    10 * time.Second,
		MaxBodyBytes:   65536,
		FailOpen:       true,
		ExcludeMethods: []string{"GET", "HEAD", "OPTIONS"},
	}
	cfg.BuildExcludeSet()
	return cfg
}

func BenchmarkDedupHandler_Unique(b *testing.B) {
	gin.SetMode(gin.TestMode)
	logger := zerolog.New(io.Discard)
	s := store.NewMemory()
	h := handler.NewDedup(benchCfg(), s, logger)

	router := gin.New()
	router.POST("/dedup-check", h.Handle)

	bodies := make([]string, b.N)
	for i := range bodies {
		bodies[i] = `{"id":"` + strings.Repeat("a", 10) + `","n":` + strings.Repeat("0", i%10+1) + `}`
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(bodies[i]))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkDedupHandler_Duplicate(b *testing.B) {
	gin.SetMode(gin.TestMode)
	logger := zerolog.New(io.Discard)
	s := store.NewMemory()
	h := handler.NewDedup(benchCfg(), s, logger)

	router := gin.New()
	router.POST("/dedup-check", h.Handle)

	body := `{"amount":100}`

	// Seed the first request so subsequent ones are duplicates
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(body))
	router.ServeHTTP(rr, req)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/dedup-check", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkDedupHandler_ExcludedMethod(b *testing.B) {
	gin.SetMode(gin.TestMode)
	logger := zerolog.New(io.Discard)
	s := store.NewMemory()
	h := handler.NewDedup(benchCfg(), s, logger)

	router := gin.New()
	router.Any("/dedup-check", h.Handle)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/dedup-check", nil)
		router.ServeHTTP(rr, req)
	}
}

func BenchmarkHealthHandler(b *testing.B) {
	gin.SetMode(gin.TestMode)
	logger := zerolog.New(io.Discard)
	s := store.NewMemory()
	h := handler.NewHealth(s, logger)

	router := gin.New()
	router.GET("/healthz", h.Handle)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/healthz", nil)
		router.ServeHTTP(rr, req)
	}
}
