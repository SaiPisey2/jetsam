//go:build integration

package demo

import (
	"context"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// grade runs jetsam's real pipeline against the running stack and returns
// every metric's grade. Going through the actual packages rather than
// re-deriving the answer here is the point: a test that recomputes the
// verdict is the algorithm typed twice.
func grade(t *testing.T) map[string]verdict.Grade {
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
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	res := verdict.Compute(inv, corpus.Build(rules, names), false)

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
		// This is the intended canary for the arrival of dashboards in the
		// corpus. No vendored RULE names this metric, which is why it is
		// unreferenced today -- but dashboard 1860 does name it, in one of
		// the panel queries that parse without substitution. When
		// sub-project B adds dashboards, this case is meant to flip to
		// used, and that flip is the signal that the corpus widened, not a
		// stale expectation to be quietly corrected.
		{
			metric: "node_scrape_collector_duration_seconds",
			want:   verdict.GradeUnreferenced,
			why:    "no vendored rule names it, and v0.1's corpus is rules only -- dashboard 1860 does name it",
		},
		{
			metric: "jetsam_demo_requests_total",
			want:   verdict.GradeUnreferenced,
			why:    "only a dashboard reads it, and v0.1's corpus is rules only",
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
	c := corpus.Build(rules, nil)

	if len(c.Produced) != wantRecording {
		t.Errorf("corpus recorded %d produced metrics, want %d (see demo/VENDOR.md)", len(c.Produced), wantRecording)
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
	c := corpus.Build(rules, nil)
	if len(c.Blocked) != 0 {
		t.Errorf("corpus blocked %d queries, want 0: %v", len(c.Blocked), c.Blocked)
	}
	if c.Queries != wantRules {
		t.Errorf("corpus read %d queries, want %d (see demo/VENDOR.md)", c.Queries, wantRules)
	}
}
