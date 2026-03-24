package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yourorg/dedup-service/internal/config"
)

// writeConfigFile creates a temporary JSON config file and returns its path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	// Load with a non-existent config file path to exercise defaults only.
	cfg, err := config.Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatalf("Load() with defaults failed: %v", err)
	}
	if cfg.ListenAddr != ":8081" {
		t.Errorf("expected default ListenAddr :8081, got %s", cfg.ListenAddr)
	}
	if cfg.DedupWindow != 10*time.Second {
		t.Errorf("expected default DedupWindow 10s, got %s", cfg.DedupWindow)
	}
	if !cfg.FailOpen {
		t.Error("expected FailOpen=true by default")
	}
	if cfg.XAccelRedirectPrefix != "/internal/upstream" {
		t.Errorf("expected default XAccelRedirectPrefix /internal/upstream, got %s", cfg.XAccelRedirectPrefix)
	}
	if cfg.TLSEnabled {
		t.Error("expected TLSEnabled=false by default")
	}
	if cfg.TLSMinVersion != "1.2" {
		t.Errorf("expected default TLSMinVersion 1.2, got %s", cfg.TLSMinVersion)
	}
}

func TestIsMethodExcluded(t *testing.T) {
	cfg, _ := config.Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	for _, m := range []string{"GET", "get", "HEAD", "OPTIONS"} {
		if !cfg.IsMethodExcluded(m) {
			t.Errorf("expected method %q to be excluded", m)
		}
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if cfg.IsMethodExcluded(m) {
			t.Errorf("expected method %q to NOT be excluded", m)
		}
	}
}

func TestJSONOverrides(t *testing.T) {
	path := writeConfigFile(t, `{
		"server": { "listen_addr": ":9090" },
		"dedup": {
			"window": "30s",
			"fail_open": false,
			"exclude_methods": ["GET", "POST"]
		}
	}`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.ListenAddr)
	}
	if cfg.DedupWindow != 30*time.Second {
		t.Errorf("expected 30s, got %s", cfg.DedupWindow)
	}
	if cfg.FailOpen {
		t.Error("expected FailOpen=false")
	}
	if !cfg.IsMethodExcluded("POST") {
		t.Error("expected POST excluded via config override")
	}
}

func TestValidationRejectsZeroWindow(t *testing.T) {
	path := writeConfigFile(t, `{
		"dedup": { "window": "0s" }
	}`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected validation error for zero window")
	}
}

func TestValidationRejectsEmptyAddr(t *testing.T) {
	path := writeConfigFile(t, `{
		"server": { "listen_addr": "" }
	}`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected validation error for empty listen addr")
	}
}

func TestValidationRejectsTLSEnabledWithoutCertOrKey(t *testing.T) {
	path := writeConfigFile(t, `{
		"server": {
			"tls_enabled": true,
			"tls_min_version": "1.2"
		}
	}`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected validation error when tls is enabled without cert/key")
	}
}

func TestValidationRejectsInvalidTLSMinVersion(t *testing.T) {
	path := writeConfigFile(t, `{
		"server": {
			"tls_min_version": "1.1"
		}
	}`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected validation error for invalid tls min version")
	}
}
