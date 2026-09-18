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
	Grafana struct {
		// URL is optional. The credential comes from $GRAFANA_TOKEN and is
		// deliberately not a field here: a config file gets committed.
		URL string `yaml:"url"`
	} `yaml:"grafana"`
	QueryLog struct {
		// Path is a glob. Nobody retains a 30-day query log as one file; it
		// is rotated, and often gzipped.
		Path string `yaml:"path"`
		// MinWindow is how long the log must cover before its silence is
		// evidence. A Go duration string -- "720h", never "30d", which is
		// not a Go duration unit and fails to unmarshal.
		MinWindow time.Duration `yaml:"min_window"`
	} `yaml:"query_log"`
	// PrometheusFile is the path, inside a git checkout, of the scrape
	// config to edit. propose refuses to run without it.
	PrometheusFile string `yaml:"prometheus_file"`
	Pricing        struct {
		PerSeriesMonth float64 `yaml:"per_series_month"`
	} `yaml:"pricing"`
}

const defaultYAML = `prometheus:
  url: http://localhost:9090
  timeout: 2m
  # Caps the metric inventory. Raise it above your instance's distinct
  # metric-name count; the TSDB status endpoint returns only the top ten
  # without it.
  metric_limit: 5000

# Optional. Reading dashboards widens the corpus, so fewer metrics look
# unread. The credential comes from $GRAFANA_TOKEN, never from this file.
grafana:
  url: ""

# Optional. Without a query log jetsam can see what your RULES and DASHBOARDS
# read but not what people read ad hoc, so every unread metric is reported as
# "unreferenced" and nothing is proposed for dropping. Point path at the file
# Prometheus writes with global.query_log_file -- a glob, since the log is
# rotated -- to upgrade those to "unqueried" and make them eligible.
query_log:
  path: ""
  # How long the log must cover before its silence counts as evidence. A Go
  # duration string: "720h" is 30 days. "30d" is NOT valid and will fail to
  # parse -- d is not a Go duration unit, and neither is a negative value,
  # which is refused rather than defaulted.
  #
  # Note that Prometheus' query log records PromQL ENGINE queries only. A
  # metric read through the label-values or series endpoints -- how Grafana
  # resolves a dashboard's variables -- never appears in it, so set
  # grafana.url alongside this rather than instead of it.
  min_window: 720h

# Required by "jetsam propose". Path, inside a git checkout, of the scrape
# config propose edits to add the drop rules it proposes.
prometheus_file: ""

# Optional. Without a configured rate, the proposed pull request states no
# cost -- there is no honest universal price for a series across Grafana
# Cloud, self-hosted Mimir, or VictoriaMetrics.
pricing:
  per_series_month: 0
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
	// Only zero means "unset"; a negative value is a mistake that must be
	// refused rather than defaulted away. "min_window: -1h" survived the
	// zero check, and then "Span 0 >= -1h" qualified an empty log, which
	// grades every unread metric unqueried and licenses dropping all of
	// them -- the worst outcome this program has, reached by a typo.
	if c.QueryLog.MinWindow < 0 {
		return Config{}, fmt.Errorf("query_log.min_window must not be negative, got %s", c.QueryLog.MinWindow)
	}
	if c.QueryLog.MinWindow == 0 {
		c.QueryLog.MinWindow = 720 * time.Hour
	}
	return c, nil
}
