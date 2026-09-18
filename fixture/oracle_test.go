//go:build integration

package fixture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// TestMimirtoolAgreesDashboardsAreUsed is a differential oracle: an
// independent implementation of the dashboard half of jetsam's question,
// checked against jetsam's own answer on the same running stack.
//
// `mimirtool analyze grafana` plus `analyze prometheus` reads every
// dashboard in a live Grafana, substitutes and parses their panel
// queries, and cross-references the result against a live Prometheus to
// report which of ITS metrics are used by dashboards and actually exist.
// That is a second implementation, written by people with no knowledge of
// this project, of the same question corpus.Build answers for the
// Dashboards source. Agreement between the two is real evidence; this
// test is not a mock of jetsam checking itself.
//
// `mimirtool analyze ruler` is NOT part of this oracle. It calls the
// Grafana Mimir ruler API (GET /prometheus/config/v1/rules under Mimir's
// own path prefix), which a plain Prometheus does not serve: it fails
// with "GET /prometheus/config/v1/rules: requested resource not found".
// Verified by hand against this fixture; do not spend time wiring it in,
// there is no rules half of this oracle to have.
//
// mimirtool is not a build dependency -- go.mod is untouched by this
// task -- so this test skips cleanly when it is not on PATH rather than
// failing the suite for an operator who has not installed it.
func TestMimirtoolAgreesDashboardsAreUsed(t *testing.T) {
	if _, err := exec.LookPath("mimirtool"); err != nil {
		t.Skip("mimirtool not on PATH; skipping the differential oracle")
	}

	dir := t.TempDir()
	grafanaMetrics := filepath.Join(dir, "metrics-in-grafana.json")
	promMetrics := filepath.Join(dir, "prometheus-metrics.json")

	runMimirtool(t, "analyze", "grafana",
		"--address="+grafanaURL,
		"--key="+grafanaToken(t),
		"--output="+grafanaMetrics,
	)
	runMimirtool(t, "analyze", "prometheus",
		"--address="+promURL,
		"--grafana-metrics-file="+grafanaMetrics,
		"--output="+promMetrics,
	)

	// in_use_metric_counts is mimirtool's answer to "which metrics does a
	// dashboard read, restricted to metrics this Prometheus actually has
	// active series for" -- the same restriction corpus.Build applies via
	// refs.Resolve(allMetrics). That restriction is what makes the two
	// sets comparable at all: metrics-in-grafana.json's raw metricsUsed
	// includes names no target here exposes.
	var promReport struct {
		InUseMetricCounts []struct {
			Metric string `json:"metric"`
		} `json:"in_use_metric_counts"`
	}
	b, err := os.ReadFile(promMetrics)
	if err != nil {
		t.Fatalf("read %s: %v", promMetrics, err)
	}
	if err := json.Unmarshal(b, &promReport); err != nil {
		t.Fatalf("decode %s: %v", promMetrics, err)
	}
	mimirUsed := make(map[string]bool, len(promReport.InUseMetricCounts))
	for _, m := range promReport.InUseMetricCounts {
		mimirUsed[m.Metric] = true
	}
	if len(mimirUsed) == 0 {
		t.Fatal("mimirtool reported zero metrics used by dashboards -- is grafana/prometheus reachable?")
	}

	inv, src := liveSources(t)
	res := verdict.Compute(inv, corpus.Build(src, metricNames(inv)))
	jetsamUsed := make(map[string]bool, len(res.Verdicts))
	for _, v := range res.Verdicts {
		if v.Grade == verdict.GradeUsed {
			jetsamUsed[v.Metric] = true
		}
	}

	// The assertion is one-directional. jetsam closes transitively over
	// recording rules -- a metric read only by a recording rule, whose
	// output is read only by a dashboard, grades used -- and mimirtool's
	// dashboard analysis does not follow that chain at all. So jetsam
	// finding metrics mimirtool does not is expected, not a disagreement
	// worth failing on; the only failure that means something is
	// mimirtool finding a dashboard read that jetsam's wider corpus
	// somehow missed.
	var missing []string
	for m := range mimirUsed {
		if !jetsamUsed[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		t.Errorf("jetsam's used-set is missing %d metric(s) mimirtool found read by a dashboard: %v", len(missing), missing)
	}

	var jetsamOnly []string
	for m := range jetsamUsed {
		if !mimirUsed[m] {
			jetsamOnly = append(jetsamOnly, m)
		}
	}
	t.Logf("mimirtool: %d metrics used by dashboards; jetsam: %d metrics used overall; %d of jetsam's are not in mimirtool's dashboard-only set (rules, recording-rule closure, or the query log): %v",
		len(mimirUsed), len(jetsamUsed), len(jetsamOnly), jetsamOnly)
}

// runMimirtool runs one mimirtool subcommand, failing the test with its
// combined output on a non-zero exit -- mimirtool writes its errors to
// stderr as log lines, not as a Go error a caller can inspect otherwise.
func runMimirtool(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("mimirtool", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mimirtool %v: %v\n%s", args, err, out)
	}
}
