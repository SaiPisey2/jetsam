package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteDefaultRefusesToOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jetsam.yaml")
	if err := WriteDefault(p); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	// Overwriting would silently discard an operator's edited config.
	if err := WriteDefault(p); err == nil {
		t.Fatal("second WriteDefault succeeded, want an error")
	}
}

func TestLoadRejectsAnEmptyURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("prometheus:\n  url: \"\"\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted an empty prometheus.url, want an error")
	}
}

func TestWriteDefaultCreatesAPrivateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jetsam.yaml")
	if err := WriteDefault(p); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	// This file grows a credential in a later version (a GitHub token),
	// so it must never be world- or group-readable, from its first write.
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want %o", got, 0o600)
	}
}

func TestMinWindowDefaultsAndParsesAsAGoDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jetsam.yaml")

	// Default when unset.
	os.WriteFile(path, []byte("prometheus:\n  url: http://x:9090\n"), 0o600)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.QueryLog.MinWindow != 720*time.Hour {
		t.Errorf("MinWindow = %v, want 720h", c.QueryLog.MinWindow)
	}

	// "30d" is NOT a Go duration and must fail loudly rather than silently
	// becoming zero, which would license every drop.
	os.WriteFile(path, []byte("prometheus:\n  url: http://x:9090\nquery_log:\n  min_window: 30d\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("min_window: 30d was accepted; d is not a Go duration unit and this must be an error")
	}
}

func TestGrafanaTokenIsNotAConfigField(t *testing.T) {
	// The credential comes from $GRAFANA_TOKEN. A token in the config file
	// gets committed.
	b, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `yaml:"token"`) {
		t.Error("config has a token field; credentials come from the environment only")
	}
}
