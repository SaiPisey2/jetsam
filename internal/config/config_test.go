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

// TestNegativeMinWindowIsRefused: Load defaulted min_window only when it
// was zero, so "min_window: -1h" survived intact. A span of 0 then
// satisfies "Span >= -1h", which qualifies an empty query log, which
// grades every unread metric unqueried and licenses dropping all of them.
// The worst outcome this program has, reached by a typo.
func TestNegativeMinWindowIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jetsam.yaml")
	if err := os.WriteFile(path, []byte("prometheus:\n  url: http://localhost:9090\nquery_log:\n  path: /tmp/q.log\n  min_window: -1h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err == nil {
		t.Fatalf("Load accepted min_window = %s, want an error", cfg.QueryLog.MinWindow)
	}
	if !strings.Contains(err.Error(), "min_window") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// TestAggregateMinSeriesSavedDefaults pins the same "every field has a
// default that works" rule the rest of this file already holds Load to:
// an install that never mentions aggregate.min_series_saved at all must
// still get the 100 the default config comments.
func TestAggregateMinSeriesSavedDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jetsam.yaml")
	os.WriteFile(p, []byte("prometheus:\n  url: http://x:9090\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Aggregate.MinSeriesSaved != 100 {
		t.Errorf("Aggregate.MinSeriesSaved = %d, want 100", c.Aggregate.MinSeriesSaved)
	}
}

// TestAggregateMinSeriesSavedHonorsAnExplicitValue: a configured value must
// survive Load unchanged, not be silently forced back to the default.
func TestAggregateMinSeriesSavedHonorsAnExplicitValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jetsam.yaml")
	os.WriteFile(p, []byte("prometheus:\n  url: http://x:9090\naggregate:\n  min_series_saved: 5\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Aggregate.MinSeriesSaved != 5 {
		t.Errorf("Aggregate.MinSeriesSaved = %d, want 5", c.Aggregate.MinSeriesSaved)
	}
}

// TestAggregateMinSeriesSavedNegativeIsRefused mirrors
// TestNegativeMinWindowIsRefused: a negative threshold would make every
// collapse look worth naming, however little it saves, which is the
// opposite of what this field is for.
func TestAggregateMinSeriesSavedNegativeIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jetsam.yaml")
	os.WriteFile(p, []byte("prometheus:\n  url: http://x:9090\naggregate:\n  min_series_saved: -1\n"), 0o600)
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load accepted a negative aggregate.min_series_saved, want an error")
	}
	if !strings.Contains(err.Error(), "min_series_saved") {
		t.Errorf("error %q does not name the offending field", err)
	}
}
