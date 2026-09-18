//go:build integration

package fixture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/corpus"
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
// The comparison is against a corpus built from corpus.Sources{Dashboards:
// ...} alone, not jetsam's full liveSources corpus. Folding the query log
// in would make the comparison non-discriminating: the fixture's live log
// is contaminated by Grafana's own dashboard auto-refresh and by this
// suite's own scans (see VENDOR.md), so jetsam's used-set covers nearly
// the whole inventory whether or not the Dashboards source is even
// populated. Isolating it here is what lets a broken dashboard fetch
// actually fail this test.
//
// `mimirtool analyze ruler` is NOT part of this oracle. It calls the
// Grafana Mimir ruler API (GET /prometheus/config/v1/rules under Mimir's
// own path prefix), which a plain Prometheus does not serve: it fails
// with "GET /prometheus/config/v1/rules: requested resource not found".
// Verified by hand against this fixture; do not spend time wiring it in,
// there is no rules half of this oracle to have.
//
// mimirtool is not a build dependency -- go.mod is untouched -- so this
// test skips cleanly when it is not on PATH rather than failing the suite
// for an operator who has not installed it. CI does not install it, so
// this oracle NEVER runs there: it is a local check somebody has to run
// deliberately, not a gate anything is held to. See fixture/VENDOR.md.
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

	// Compared against a corpus built from dashboards alone, not the full
	// liveSources corpus. With the query log folded in, jetsam's used-set
	// covers nearly the entire inventory regardless of whether the
	// dashboard fetch actually works -- see VENDOR.md on the live log's
	// contamination -- which would make this assertion pass even with
	// Dashboards silently nil. A dashboards-only corpus is the set
	// mimirtool's analysis is actually an independent implementation of.
	inv, src := liveSources(t)
	dashOnly := corpus.Build(corpus.Sources{Dashboards: src.Dashboards}, metricNames(inv))

	// The assertion is one-directional. jetsam closes transitively over
	// substitution and Extract's own resolution even within the dashboard
	// source alone, so an exact match is not expected either way; the
	// failure that means something is mimirtool finding a dashboard read
	// that jetsam's dashboard corpus missed entirely.
	//
	// This used to carry a named exception here for metrics referenced
	// only by a dashboard's template-variable definitions (label_values(),
	// backing a "job"/"nodename"/"instance" dropdown) -- internal/grafana
	// only scanned panel queries, and node_uname_info, read solely by the
	// vendored dashboard's own variables, was missing from jetsam's
	// dashboard-only set as a result. That gap is now closed:
	// grafana.Dashboard carries VariableQueries alongside Queries, and
	// corpus.Build resolves them the same way it resolves an unparseable
	// panel. mimirtool and jetsam now agree on the dashboard-only set with
	// no exception needed; if a real disagreement reappears, it belongs in
	// this failure, not behind a new allowance.
	var missing []string
	for m := range mimirUsed {
		if !dashOnly.Used[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("jetsam's dashboard-only used-set is missing %d metric(s) mimirtool found read by a dashboard: %v", len(missing), missing)
	}

	var jetsamOnly []string
	for m := range dashOnly.Used {
		if !mimirUsed[m] {
			jetsamOnly = append(jetsamOnly, m)
		}
	}
	sort.Strings(jetsamOnly)
	shown := jetsamOnly
	if len(shown) > 10 {
		shown = shown[:10]
	}
	t.Logf("mimirtool: %d metrics used by dashboards; jetsam (dashboards only): %d metrics used; %d of jetsam's are not in mimirtool's set (first %d shown): %v",
		len(mimirUsed), len(dashOnly.Used), len(jetsamOnly), len(shown), shown)
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
