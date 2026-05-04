// Command server is the deduplication service for the Nginx API Gateway.
//
// It exposes the following endpoints:
//
//	GET  /healthz       — liveness/readiness; returns 200 when Redis is reachable
//	GET  /metrics       — Prometheus metrics endpoint
//
// Configuration is loaded from config.json via Viper (see internal/config/config.go).
// Set server.log_level to "debug" for verbose per-request fingerprint logging.
package main

import (
	"context"
	"crypto/tls"
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
		Bool("tls_enabled", cfg.TLSEnabled).
		Str("tls_min_version", cfg.TLSMinVersion).
		Str("redis", cfg.RedisAddr).
		Int("redis_db", cfg.RedisDB).
		Str("dedup_window", cfg.DedupWindow.String()).
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
	router.Use(middleware.SecurityHeaders())
	router.Use(middleware.Logging(logger, cfg.DisableRequestLogging))
	router.Use(middleware.Metrics())

	// Routes
	healthH := handler.NewHealth(dedupStore, logger)
	router.GET("/healthz", healthH.Handle)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// ── X-Accel-Redirect mode only: Nginx sends full request (with body);
	//    service returns X-Accel-Redirect for allowed, 409 for duplicate.
	//    Nginx then internally redirects allowed requests to the upstream.
	xaccelH := handler.NewXAccelDedup(cfg, dedupStore, logger, cfg.XAccelRedirectPrefix)
	logger.Info().
		Str("redirect_prefix", cfg.XAccelRedirectPrefix).
		Msg("X-Accel-Redirect mode enabled (body-based dedup, Nginx forwards)")
	router.NoRoute(xaccelH.Handle)

	// ── pprof server (localhost-only, separate from main listener) ───────────
	// pprof is intentionally NOT registered on the main router. It binds only
	// to 127.0.0.1:6060 so it is unreachable from Nginx or external traffic.
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	pprofMux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	pprofMux.Handle("/debug/pprof/block", pprof.Handler("block"))
	pprofMux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	pprofMux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	pprofMux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	pprofMux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	pprofSrv := &http.Server{Addr: "127.0.0.1:6060", Handler: pprofMux}
	go func() {
		logger.Info().Str("addr", pprofSrv.Addr).Msg("pprof server listening (localhost only)")
		if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn().Err(err).Msg("pprof server stopped")
		}
	}()

	// ── HTTP server ───────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      router,
		ReadTimeout:  5 * cfg.RedisReadTimeout,
		WriteTimeout: 5 * cfg.RedisWriteTimeout,
		IdleTimeout:  60 * cfg.RedisDialTimeout,
	}
	if cfg.TLSEnabled {
		var tlsMinVersion uint16 = tls.VersionTLS12
		if cfg.TLSMinVersion == "1.3" {
			tlsMinVersion = tls.VersionTLS13
		}
		srv.TLSConfig = &tls.Config{MinVersion: tlsMinVersion}
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		if cfg.TLSEnabled {
			logger.Info().
				Str("addr", cfg.ListenAddr).
				Str("cert_file", cfg.TLSCertFile).
				Str("key_file", cfg.TLSKeyFile).
				Str("tls_min_version", cfg.TLSMinVersion).
				Msg("https server listening")
		} else {
			logger.Info().Str("addr", cfg.ListenAddr).Msg("http server listening")
		}

		var err error
		if cfg.TLSEnabled {
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	_ = pprofSrv.Shutdown(ctx)

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
