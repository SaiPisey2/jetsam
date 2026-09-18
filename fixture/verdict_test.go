//go:build integration

package fixture

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// grafanaURL is the fixture's Grafana, reachable the same way stack_test.go
// reaches it.
const grafanaURL = "http://localhost:3000"

// demoLogMinWindow is this fixture's stand-in for the configured
// query_log.min_window a real install would set (720h by default -- see
// internal/config). The fixture's log has only been running since `make
// demo-up`, not 30 days, so pinning that default here would make
// LogQualifies false forever and the query log would never be exercised.
// This value is not asserted anywhere; it only has to be short enough that
// the fixture's own log -- which fixture/ready.sh already waited to become
// measurable before this test runs -- qualifies.
const demoLogMinWindow = 1 * time.Minute

// grafanaToken reads the service-account token fixture/grafana/token.sh
// wrote during `make demo-up`. Production reads the same credential from
// $GRAFANA_TOKEN (see cmd/jetsam/main.go's gather); nothing sets that
// variable for `go test`, so the fixture reads the file directly.
func grafanaToken(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("grafana/.token")
	if err != nil {
		t.Fatalf("no grafana token, run `make demo-up`: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// liveSources gathers every evidence source jetsam's production code
// gathers -- rules, dashboards, and the query log -- against the running
// stack, mirroring cmd/jetsam/main.go's gather rather than re-deriving a
// second, divergent wiring of the same three sources.
func liveSources(t *testing.T) (inventory.Inventory, corpus.Sources) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cl := promapi.New(promURL, 2*time.Minute)
	status, err := cl.TSDBStatus(ctx, 5000)
	if err != nil {
		t.Fatalf("tsdb status, run `make demo-up`: %v", err)
	}
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	inv := inventory.Build(status)

	src := corpus.Sources{Rules: rules, DashboardsConfigured: true}
	g := grafana.New(grafanaURL, grafanaToken(t), 2*time.Minute)
	dash, err := g.Dashboards(ctx)
	if err != nil {
		t.Fatalf("grafana dashboards, run `make demo-up`: %v", err)
	}
	src.Dashboards = dash
	src.DashboardsReachable = true

	reading, err := querylog.Read("querylog/queries.log")
	if err != nil {
		t.Fatalf("query log, run `make demo-up`: %v", err)
	}
	src.QueryLog = reading
	if reading != nil {
		src.LogQualifies = reading.Span >= demoLogMinWindow
	}

	return inv, src
}

// metricNames extracts every metric name in the shape corpus.Build wants.
func metricNames(inv inventory.Inventory) []string {
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	return names
}

// grade runs jetsam's real pipeline against the running stack and returns
// every metric's grade. Going through the actual packages rather than
// re-deriving the answer here is the point: a test that recomputes the
// verdict is the algorithm typed twice.
func grade(t *testing.T) map[string]verdict.Grade {
	t.Helper()
	inv, src := liveSources(t)
	res := verdict.Compute(inv, corpus.Build(src, metricNames(inv)))

	out := make(map[string]verdict.Grade, len(res.Verdicts))
	for _, v := range res.Verdicts {
		out[v.Metric] = v.Grade
	}
	return out
}

// TestKnownArchetypesGradeCorrectly is the assertion that catches an
// upstream bump moving a metric across a grading boundary -- the exact
// failure that went unnoticed twice on the sibling project, because the
// suite only ever checked that the stack was running.
func TestKnownArchetypesGradeCorrectly(t *testing.T) {
	grades := grade(t)

	cases := []struct {
		metric string
		want   verdict.Grade
		why    string
	}{
		{
			metric: "node_cpu_seconds_total",
			want:   verdict.GradeUsed,
			why:    "the vendored node-exporter rules read it",
		},
		// This was the canary for the arrival of dashboards in the corpus.
		// No vendored RULE names this metric -- it graded unreferenced
		// through v0.1, when the corpus was rules only -- but dashboard
		// 1860 does name it, in one of the panel queries that parse
		// without substitution. Now that the corpus reads dashboards, this
		// is the flip that proves it: the acceptance test for this whole
		// sub-project, not a stale expectation quietly corrected.
		{
			metric: "node_scrape_collector_duration_seconds",
			want:   verdict.GradeUsed,
			why:    "dashboard 1860 reads it, in a panel query that parses without substitution",
		},
		{
			metric: "jetsam_demo_requests_total",
			want:   verdict.GradeUsed,
			why:    "a dashboard reads it -- the corpus is no longer rules only",
		},
		// This case exists to exercise verdict.Compute's Produced branch --
		// the rule that stops jetsam proposing a drop for a metric one of
		// its own recording rules writes. Delete that branch and this
		// assertion is the one that fails.
		//
		// The metric is chosen, not arbitrary. instance:node_num_cpu:sum
		// would NOT work: instance:node_load1_per_cpu:ratio reads it, so it
		// reaches GradeUsed through the Used branch, which comes first, and
		// the assertion would pass with the Produced branch deleted.
		// instance:node_cpu_utilisation:rate5m is written by a recording
		// rule and read by nothing, so Produced is the only branch that can
		// grade it used.
		//
		// It also depends on the recording rule actually producing series:
		// if the scrape job names stop matching the vendored rules'
		// selectors, this metric vanishes from the inventory and the case
		// fails with "absent from the inventory entirely".
		{
			metric: "instance:node_cpu_utilisation:rate5m",
			want:   verdict.GradeUsed,
			why:    "a recording rule writes it, and nothing reads it -- only Produced can protect it",
		},
	}
	for _, tc := range cases {
		got, ok := grades[tc.metric]
		if !ok {
			t.Errorf("%s is absent from the inventory entirely -- is its target up?", tc.metric)
			continue
		}
		if got != tc.want {
			t.Errorf("%s grades %q, want %q (%s)", tc.metric, got, tc.want, tc.why)
		}
	}
}

// TestRecordingRuleOutputsAreProtected checks the rule that stops jetsam
// proposing a drop for a metric one of its own recording rules writes.
// The vendored corpus supplies all of them, so this is checked against
// real rule outputs rather than an invented one.
func TestRecordingRuleOutputsAreProtected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cl := promapi.New(promURL, 2*time.Minute)
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	c := corpus.Build(corpus.Sources{Rules: rules}, nil)

	if len(c.Produced) != wantRecording {
		t.Errorf("corpus recorded %d produced metrics, want %d (see fixture/VENDOR.md)", len(c.Produced), wantRecording)
	}
	for _, want := range []string{
		"instance:node_num_cpu:sum",
		"instance:node_cpu_utilisation:rate5m",
		"instance:node_load1_per_cpu:ratio",
	} {
		if !c.Produced[want] {
			t.Errorf("%s is written by a recording rule but is not protected", want)
		}
	}
}

// TestTheCorpusBlocksNothing records a property worth knowing before
// sub-project B changes it: with rules as the only corpus, every query
// parses and nothing is blocked. B adds dashboards, whose queries do NOT
// parse, and this test is where that change becomes visible.
func TestTheCorpusBlocksNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cl := promapi.New(promURL, 2*time.Minute)
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	c := corpus.Build(corpus.Sources{Rules: rules}, nil)
	if len(c.Blocked) != 0 {
		t.Errorf("corpus blocked %d queries, want 0: %v", len(c.Blocked), c.Blocked)
	}
	if c.Queries != wantRules {
		t.Errorf("corpus read %d queries, want %d (see fixture/VENDOR.md)", c.Queries, wantRules)
	}
}

// A log shorter than the configured minimum must not license a drop, however
// complete it looks.
func TestAShortQueryLogLicensesNothing(t *testing.T) {
	inv, src := liveSources(t)
	src.LogQualifies = false
	res := verdict.Compute(inv, corpus.Build(src, metricNames(inv)))
	for _, v := range res.Verdicts {
		if v.Droppable {
			t.Fatalf("%s was droppable with a non-qualifying log", v.Metric)
		}
	}
}

// Grafana configured but unreachable withholds every drop while scan still
// works.
func TestUnreachableGrafanaWithholdsEveryDrop(t *testing.T) {
	inv, src := liveSources(t)
	src.DashboardsConfigured = true
	src.DashboardsReachable = false
	src.Dashboards = nil
	res := verdict.Compute(inv, corpus.Build(src, metricNames(inv)))
	for _, v := range res.Verdicts {
		if v.Droppable {
			t.Fatalf("%s was droppable while dashboard evidence was unavailable", v.Metric)
		}
	}
}
