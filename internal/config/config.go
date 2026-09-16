// Package config is jetsam's on-disk configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Prometheus struct {
		URL     string        `yaml:"url"`
		Timeout time.Duration `yaml:"timeout"`
		// MetricLimit caps /api/v1/status/tsdb?limit=. Without a limit the
		// endpoint returns only ten entries, which would read as "this
		// Prometheus has ten metrics" and mark nothing else as existing.
		MetricLimit int `yaml:"metric_limit"`
	} `yaml:"prometheus"`
	QueryLog struct {
		Path string `yaml:"path"`
	} `yaml:"query_log"`
}

const defaultYAML = `prometheus:
  url: http://localhost:9090
  timeout: 2m
  # Caps the metric inventory. Raise it above your instance's distinct
  # metric-name count; the TSDB status endpoint returns only the top ten
  # without it.
  metric_limit: 5000

# Optional. Without a query log jetsam can see what your RULES read but not
# what people read, so every unread metric is reported as "unreferenced"
# and nothing is proposed for dropping. Point this at the file Prometheus
# writes with --query.log-file to upgrade those to "unqueried" and make
# them eligible.
query_log:
  path: ""
`

// WriteDefault writes the commented default config, refusing to overwrite
// an existing file: a re-run of init must never discard edits.
//
// The file is created 0o600 (owner read/write only), not the more usual
// 0o644: this config grows a credential in a later version (Task 11-12
// add a GitHub token), and a config file is worth protecting from other
// local users from the moment it exists, not only once a secret lands in
// it.
func WriteDefault(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	_, err = f.WriteString(defaultYAML)
	return err
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Prometheus.URL == "" {
		return Config{}, errors.New("prometheus.url is required")
	}
	if c.Prometheus.Timeout == 0 {
		c.Prometheus.Timeout = 2 * time.Minute
	}
	if c.Prometheus.MetricLimit == 0 {
		c.Prometheus.MetricLimit = 5000
	}
	return c, nil
}

// HaveQueryLog reports whether real read traffic is available. It is the
// difference between "no rule mentions it" and "nobody read it".
func (c Config) HaveQueryLog() bool { return c.QueryLog.Path != "" }
