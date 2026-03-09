// Package config loads and validates all runtime configuration from a JSON file
// using Viper. Every field has a documented default so the service can run with
// zero configuration against a local Redis instance.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all runtime configuration for the dedup service.
type Config struct {
	// ── Server ────────────────────────────────────────────────────────────────
	ListenAddr      string        // HTTP bind address, e.g. ":8081"
	ShutdownTimeout time.Duration // Graceful shutdown drain period
	LogLevel        string        // debug | info | warn | error

	// ── Logging ──────────────────────────────────────────────────────────────
	LogFile       string // path to log file, e.g. "log/app.log"
	LogMaxSizeMB  int    // max size in MB before rotation
	LogMaxBackups int    // max number of old log files to keep
	LogMaxAgeDays int    // max days to retain old log files
	LogCompress   bool   // whether to gzip rotated files

	// ── Redis ─────────────────────────────────────────────────────────────────
	RedisAddr         string        // host:port
	RedisPassword     string        // empty = no auth
	RedisDB           int           // logical DB index (0–15)
	RedisDialTimeout  time.Duration // TCP connection timeout
	RedisReadTimeout  time.Duration // socket read timeout
	RedisWriteTimeout time.Duration // socket write timeout
	RedisPoolSize     int           // connection pool size per CPU
	RedisMinIdle      int           // minimum idle connections

	// ── Deduplication ─────────────────────────────────────────────────────────
	DedupWindow    time.Duration // Redis TTL for fingerprint keys
	MaxBodyBytes   int64         // body bytes read for hashing; remainder discarded
	FailOpen       bool          // true = allow requests when Redis is unreachable
	ExcludeMethods []string      // HTTP methods that bypass dedup entirely
	// excludeSet is an O(1) lookup table built from ExcludeMethods.
	excludeSet map[string]struct{}

	// ── Proxy mode ───────────────────────────────────────────────────────────
	UpstreamURL string // Backend URL for reverse-proxy mode (e.g. "http://localhost:9000").
	// When set, the service acts as a reverse proxy: requests pass
	// through dedup and allowed ones are forwarded to the upstream.
	// When empty, the service operates in sidecar/auth_request mode.

	// ── X-Accel-Redirect mode ────────────────────────────────────────────────
	XAccelRedirectPrefix string // Internal Nginx location prefix (e.g. "/internal/upstream").
	// When set, the service returns X-Accel-Redirect headers for
	// allowed requests so Nginx forwards to the real upstream.
	// Body is available for fingerprinting in this mode.

	// ── Performance ──────────────────────────────────────────────────────────
	LocalCacheEnabled bool          // L1 in-process cache for duplicate lookups
	GOGC              int           // Go GC target percentage (0 = use Go default of 100)
	StoreTimeout      time.Duration // context deadline for store (Redis) calls
}

// Load reads configuration from the JSON file at configPath (default "config.json"
// in the working directory), applies defaults, and returns a validated Config.
// If configPath is empty, it searches for "config.json" in the working directory.
func Load(configPath ...string) (*Config, error) {
	v := viper.New()

	// ── Defaults ──────────────────────────────────────────────────────────────
	v.SetDefault("server.listen_addr", ":8081")
	v.SetDefault("server.log_level", "info")
	v.SetDefault("server.shutdown_timeout", "10s")

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.dial_timeout", "2s")
	v.SetDefault("redis.read_timeout", "200ms")
	v.SetDefault("redis.write_timeout", "200ms")

	v.SetDefault("performance.local_cache", true)
	v.SetDefault("performance.gogc", 0)
	v.SetDefault("performance.store_timeout", "500ms")
	v.SetDefault("redis.pool_size", 100)
	v.SetDefault("redis.min_idle", 20)

	v.SetDefault("log.file", "log/app.log")
	v.SetDefault("log.max_size_mb", 50)
	v.SetDefault("log.max_backups", 5)
	v.SetDefault("log.max_age_days", 30)
	v.SetDefault("log.compress", true)

	v.SetDefault("dedup.window", "10s")
	v.SetDefault("dedup.max_body_bytes", 65536)
	v.SetDefault("dedup.fail_open", true)
	v.SetDefault("dedup.exclude_methods", []string{"GET", "HEAD", "OPTIONS"})

	v.SetDefault("proxy.upstream_url", "")
	v.SetDefault("proxy.x_accel_redirect_prefix", "")

	// ── Environment variable overrides ────────────────────────────────────────
	// Bind specific env vars so the service can be configured without a JSON
	// file. The DEDUP_ prefix avoids collisions with other services.
	v.SetEnvPrefix("DEDUP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// ── Read JSON config file ─────────────────────────────────────────────────
	if len(configPath) > 0 && configPath[0] != "" {
		v.SetConfigFile(configPath[0])
	} else {
		v.SetConfigName("config")
		v.SetConfigType("json")
		v.AddConfigPath(".")
	}

	if err := v.ReadInConfig(); err != nil {
		// Config file is optional; if it doesn't exist, defaults are used.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			// When an explicit path is given via SetConfigFile and the file
			// doesn't exist, Viper returns a generic *os.PathError rather
			// than ConfigFileNotFoundError. Treat that as non-fatal too.
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("reading config file: %w", err)
			}
		}
	}

	// ── Parse durations ───────────────────────────────────────────────────────
	shutdownTimeout, err := time.ParseDuration(v.GetString("server.shutdown_timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid server.shutdown_timeout: %w", err)
	}
	dialTimeout, err := time.ParseDuration(v.GetString("redis.dial_timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid redis.dial_timeout: %w", err)
	}
	readTimeout, err := time.ParseDuration(v.GetString("redis.read_timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid redis.read_timeout: %w", err)
	}
	writeTimeout, err := time.ParseDuration(v.GetString("redis.write_timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid redis.write_timeout: %w", err)
	}
	dedupWindow, err := time.ParseDuration(v.GetString("dedup.window"))
	if err != nil {
		return nil, fmt.Errorf("invalid dedup.window: %w", err)
	}
	storeTimeout, err := time.ParseDuration(v.GetString("performance.store_timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid performance.store_timeout: %w", err)
	}

	// ── Normalise exclude methods ─────────────────────────────────────────────
	rawMethods := v.GetStringSlice("dedup.exclude_methods")
	methods := make([]string, 0, len(rawMethods))
	for _, m := range rawMethods {
		if t := strings.TrimSpace(strings.ToUpper(m)); t != "" {
			methods = append(methods, t)
		}
	}

	cfg := &Config{
		ListenAddr:      v.GetString("server.listen_addr"),
		ShutdownTimeout: shutdownTimeout,
		LogLevel:        v.GetString("server.log_level"),

		LogFile:       v.GetString("log.file"),
		LogMaxSizeMB:  v.GetInt("log.max_size_mb"),
		LogMaxBackups: v.GetInt("log.max_backups"),
		LogMaxAgeDays: v.GetInt("log.max_age_days"),
		LogCompress:   v.GetBool("log.compress"),

		RedisAddr:         v.GetString("redis.addr"),
		RedisPassword:     v.GetString("redis.password"),
		RedisDB:           v.GetInt("redis.db"),
		RedisDialTimeout:  dialTimeout,
		RedisReadTimeout:  readTimeout,
		RedisWriteTimeout: writeTimeout,
		RedisPoolSize:     v.GetInt("redis.pool_size"),
		RedisMinIdle:      v.GetInt("redis.min_idle"),

		DedupWindow:    dedupWindow,
		MaxBodyBytes:   v.GetInt64("dedup.max_body_bytes"),
		FailOpen:       v.GetBool("dedup.fail_open"),
		ExcludeMethods: methods,

		UpstreamURL:          v.GetString("proxy.upstream_url"),
		XAccelRedirectPrefix: v.GetString("proxy.x_accel_redirect_prefix"),

		LocalCacheEnabled: v.GetBool("performance.local_cache"),
		GOGC:              v.GetInt("performance.gogc"),
		StoreTimeout:      storeTimeout,
	}

	// Build O(1) method-exclusion set.
	cfg.BuildExcludeSet()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// IsMethodExcluded reports whether deduplication should be skipped for method.
// Uses an O(1) map lookup built at config load time.
func (c *Config) IsMethodExcluded(method string) bool {
	_, ok := c.excludeSet[strings.ToUpper(method)]
	return ok
}

// BuildExcludeSet initialises the internal excludeSet map from ExcludeMethods.
// This is called automatically by Load but must be called manually when a Config
// is constructed directly (e.g. in tests).
func (c *Config) BuildExcludeSet() {
	c.excludeSet = make(map[string]struct{}, len(c.ExcludeMethods))
	for _, m := range c.ExcludeMethods {
		c.excludeSet[strings.ToUpper(m)] = struct{}{}
	}
}

func (c *Config) validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("server.listen_addr must not be empty")
	}
	if c.RedisAddr == "" {
		return fmt.Errorf("redis.addr must not be empty")
	}
	if c.DedupWindow <= 0 {
		return fmt.Errorf("dedup.window must be a positive duration, got %s", c.DedupWindow)
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("dedup.max_body_bytes must be positive, got %d", c.MaxBodyBytes)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("server.shutdown_timeout must be positive, got %s", c.ShutdownTimeout)
	}
	if c.RedisPoolSize <= 0 {
		return fmt.Errorf("redis.pool_size must be positive, got %d", c.RedisPoolSize)
	}
	if c.StoreTimeout <= 0 {
		return fmt.Errorf("performance.store_timeout must be positive, got %s", c.StoreTimeout)
	}
	return nil
}
