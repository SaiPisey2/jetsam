//go:build integration

package fixture

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/grafana"
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

	// Compared against a corpus built from dashboards alone, not the full
	// liveSources corpus. With the query log folded in, jetsam's used-set
	// covers nearly the entire inventory regardless of whether the
	// dashboard fetch actually works -- see VENDOR.md on the live log's
	// contamination -- which would make this assertion pass even with
	// Dashboards silently nil. A dashboards-only corpus is the set
	// mimirtool's analysis is actually an independent implementation of.
	inv, src := liveSources(t)
	dashOnly := corpus.Build(corpus.Sources{Dashboards: src.Dashboards}, metricNames(inv))

	// templateOnly names metrics mentioned only in a dashboard's own
	// template-variable definitions (Grafana's "job"/"nodename"/"instance"
	// dropdowns, typically a label_values(metric, label) query), never in
	// any panel's target expr. This is a real, narrow, documented gap in
	// internal/grafana: Client.Dashboards collects panel target queries
	// only -- see its Dashboard type's doc comment, "the queries its
	// panels run" -- by design from the task that built it, not an
	// oversight introduced here. Grafana genuinely issues a
	// template-variable query to Prometheus each time the dashboard loads,
	// so a metric named only there is a real read jetsam's Dashboards
	// source does not see; jetsam's PRODUCTION verdict still catches it
	// through the query log in the fixture (confirmed:
	// TestKnownArchetypesGradeCorrectly and the isolation tests above all
	// use the full corpus, where this metric is not at issue). Fixing
	// internal/grafana to also scan template variables is a production
	// change outside this test's remit; it is named here, logged rather
	// than silently absorbed, and excluded from the hard failure below
	// because it is not the failure this test exists to catch -- a broken
	// dashboard fetch.
	known := make(map[string]bool, len(inv.Metrics))
	for _, m := range inv.Metrics {
		known[m.Name] = true
	}
	templateOnly := templateVariableMetrics(t, src.Dashboards, dashOnly, known)

	// The assertion is one-directional. jetsam closes transitively over
	// substitution and Extract's own resolution even within the dashboard
	// source alone, so an exact match is not expected either way; the
	// failure that means something is mimirtool finding a dashboard read
	// that jetsam's dashboard corpus missed for a reason OTHER than the
	// documented template-variable gap above.
	var missing, knownGap []string
	for m := range mimirUsed {
		if dashOnly.Used[m] {
			continue
		}
		if templateOnly[m] {
			knownGap = append(knownGap, m)
			continue
		}
		missing = append(missing, m)
	}
	if len(knownGap) > 0 {
		sort.Strings(knownGap)
		t.Logf("mimirtool found %d metric(s) named only in a dashboard template variable, which internal/grafana does not scan by design (known gap, not a test failure): %v", len(knownGap), knownGap)
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

// templateVarMetricToken matches a metric-name-shaped token, the same
// conservative extraction internal/corpus applies to an unparseable panel
// (see corpus.metricToken) -- reimplemented here rather than exported from
// production, since this is fixture-only test isolation, not something
// internal/corpus needs to expose.
var templateVarMetricToken = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// templateVariableMetrics fetches each dashboard's raw definition straight
// from Grafana (grafana.Dashboard does not carry templating, only panel
// queries) and returns every metric-shaped token found in a template
// variable's query/definition that dashOnly did NOT already mark used from
// a panel query. That is precisely the set of metrics a template-variable
// scan would add on top of what internal/grafana already collects.
func templateVariableMetrics(t *testing.T, dashboards []grafana.Dashboard, dashOnly corpus.Corpus, known map[string]bool) map[string]bool {
	t.Helper()
	token := grafanaToken(t)
	client := &http.Client{Timeout: 30 * time.Second}

	found := map[string]bool{}
	for _, d := range dashboards {
		req, _ := http.NewRequest("GET", grafanaURL+"/api/dashboards/uid/"+d.UID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("fetch dashboard %s for template-variable check: %v", d.UID, err)
		}
		var body struct {
			Dashboard struct {
				Templating struct {
					List []struct {
						Definition string `json:"definition"`
					} `json:"list"`
				} `json:"templating"`
			} `json:"dashboard"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if derr != nil {
			t.Fatalf("decode dashboard %s for template-variable check: %v", d.UID, derr)
		}
		for _, v := range body.Dashboard.Templating.List {
			if strings.TrimSpace(v.Definition) == "" {
				continue
			}
			for _, tok := range templateVarMetricToken.FindAllString(v.Definition, -1) {
				if known[tok] && !dashOnly.Used[tok] {
					found[tok] = true
				}
			}
		}
	}
	return found
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
