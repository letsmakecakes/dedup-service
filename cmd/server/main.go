// Command server is the deduplication sidecar for the Nginx API Gateway.
//
// It exposes the following endpoints:
//
//	POST /dedup-check   — Nginx auth_request target; returns 200 (allow) or 409 (duplicate)
//	GET  /healthz       — liveness/readiness; returns 200 when Redis is reachable
//	GET  /metrics       — Prometheus metrics endpoint
//
// Configuration is loaded from config.json via Viper (see internal/config/config.go).
// Set server.log_level to "debug" for verbose per-request fingerprint logging.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	_ "go.uber.org/automaxprocs"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/yourorg/dedup-service/internal/config"
	"github.com/yourorg/dedup-service/internal/handler"
	"github.com/yourorg/dedup-service/internal/middleware"
	"github.com/yourorg/dedup-service/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ── Config ────────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// ── Logger (zerolog + lumberjack rotation) ────────────────────────────────
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	// Ensure the log directory exists.
	logDir := filepath.Dir(cfg.LogFile)
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return fmt.Errorf("creating log directory %s: %w", logDir, err)
	}

	// Rotating file writer.
	logRotator := &lumberjack.Logger{
		Filename:   cfg.LogFile,
		MaxSize:    cfg.LogMaxSizeMB, // megabytes
		MaxBackups: cfg.LogMaxBackups,
		MaxAge:     cfg.LogMaxAgeDays, // days
		Compress:   cfg.LogCompress,
	}
	defer logRotator.Close()

	// Write to both stdout and the rotated log file.
	multiWriter := io.MultiWriter(os.Stdout, logRotator)
	logger := zerolog.New(multiWriter).With().Timestamp().Logger()

	logger.Info().
		Str("addr", cfg.ListenAddr).
		Str("redis", cfg.RedisAddr).
		Int("redis_db", cfg.RedisDB).
		Str("dedup_window", cfg.DedupWindow.String()).
		Int64("max_body_bytes", cfg.MaxBodyBytes).
		Bool("fail_open", cfg.FailOpen).
		Strs("exclude_methods", cfg.ExcludeMethods).
		Msg("starting dedup-service")

	// ── Store ─────────────────────────────────────────────────────────────────
	s, closeStore, err := initStore(cfg, logger)
	if err != nil {
		return err
	}
	// ── Performance tuning ───────────────────────────────────────────────────────
	if cfg.GOGC > 0 {
		old := debug.SetGCPercent(cfg.GOGC)
		logger.Info().Int("gogc_old", old).Int("gogc_new", cfg.GOGC).Msg("GC tuning applied")
	}

	// Wrap store with L1 local cache for duplicate lookups.
	var dedupStore store.Store = s
	if cfg.LocalCacheEnabled {
		cached := store.NewCached(s)
		dedupStore = cached
		// Replace closeStore so we stop the sweep goroutine and close Redis.
		origClose := closeStore
		closeStore = func() {
			_ = cached.Close() // stops sweep + closes backend
			_ = origClose      // already closed by cached.Close(), suppress lint
		}
		logger.Info().Msg("L1 local cache enabled")
	}
	defer closeStore()

	// ── Gin router ────────────────────────────────────────────────────────────
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Global middleware
	router.Use(middleware.Recovery(logger))
	router.Use(middleware.RequestID())
	router.Use(middleware.Logging(logger))
	router.Use(middleware.Metrics())

	// Not-found handler
	router.NoRoute(middleware.NotFound())

	// Routes
	dedupH := handler.NewDedup(cfg, dedupStore, logger)
	healthH := handler.NewHealth(dedupStore, logger)

	router.POST("/dedup-check", dedupH.Handle)
	router.GET("/healthz", healthH.Handle)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// pprof endpoints for runtime profiling (CPU, memory, goroutines, etc.).
	pprofGroup := router.Group("/debug/pprof")
	{
		pprofGroup.GET("/", gin.WrapF(pprof.Index))
		pprofGroup.GET("/cmdline", gin.WrapF(pprof.Cmdline))
		pprofGroup.GET("/profile", gin.WrapF(pprof.Profile))
		pprofGroup.GET("/symbol", gin.WrapF(pprof.Symbol))
		pprofGroup.GET("/trace", gin.WrapF(pprof.Trace))
		pprofGroup.GET("/allocs", gin.WrapH(pprof.Handler("allocs")))
		pprofGroup.GET("/block", gin.WrapH(pprof.Handler("block")))
		pprofGroup.GET("/goroutine", gin.WrapH(pprof.Handler("goroutine")))
		pprofGroup.GET("/heap", gin.WrapH(pprof.Handler("heap")))
		pprofGroup.GET("/mutex", gin.WrapH(pprof.Handler("mutex")))
		pprofGroup.GET("/threadcreate", gin.WrapH(pprof.Handler("threadcreate")))
	}

	// ── HTTP server ───────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  5 * cfg.RedisReadTimeout,
		WriteTimeout: 5 * cfg.RedisWriteTimeout,
		IdleTimeout:  60 * cfg.RedisDialTimeout,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info().Str("addr", cfg.ListenAddr).Msg("server listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("http server: %w", err)
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return err
		}
	case sig := <-quit:
		logger.Info().Str("signal", sig.String()).Msg("shutdown signal received")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info().Msg("server stopped")
	return nil
}

// initStore connects to Redis. On failure:
//   - If FailOpen=true:  log a warning and fall back to a permanently-erroring
//     MemoryStore so that the handler's fail-open path kicks in for every request.
//   - If FailOpen=false: return a hard error and refuse to start.
func initStore(cfg *config.Config, logger zerolog.Logger) (store.Store, func(), error) {
	redisStore, err := store.NewRedis(cfg)
	if err != nil {
		if !cfg.FailOpen {
			return nil, nil, fmt.Errorf("redis unavailable and fail-open is disabled: %w", err)
		}
		logger.Warn().
			Err(err).
			Str("redis_addr", cfg.RedisAddr).
			Msg("redis unavailable at startup; running fail-open (all requests allowed)")
		// Return a MemoryStore whose Err is permanently set so every IsDuplicate
		// call returns ErrUnavailable, triggering the handler's fail-open branch.
		mem := store.NewMemory()
		mem.Err = store.ErrUnavailable
		return mem, func() {}, nil
	}

	closeFunc := func() {
		if err := redisStore.Close(); err != nil {
			logger.Warn().Err(err).Msg("redis close error")
		}
	}
	return redisStore, closeFunc, nil
}
