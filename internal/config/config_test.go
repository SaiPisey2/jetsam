package config

import (
	"os"
	"path/filepath"
	"testing"
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

func TestHaveQueryLogIsFalseByDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jetsam.yaml")
	if err := WriteDefault(p); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The default install proposes nothing. That is the honest outcome, and
	// Task 6 depends on it.
	if c.HaveQueryLog() {
		t.Error("HaveQueryLog = true on a default config, want false")
	}
}
