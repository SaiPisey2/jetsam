package verdict

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
)

func fixture() (inventory.Inventory, corpus.Corpus) {
	inv := inventory.Build(promapi.Status{
		Counts:     map[string]int{"used_metric": 100, "unread_metric": 400, "job:rec": 5},
		HeadSeries: 505,
	})
	c := corpus.Corpus{
		Queries:  3,
		Used:     map[string]bool{"used_metric": true},
		UsedBy:   map[string]corpus.Source{"used_metric": corpus.FromRule},
		Produced: map[string]bool{"job:rec": true},
	}
	return inv, c
}

func TestWithoutAQueryLogNothingIsDroppable(t *testing.T) {
	// Rules and dashboards cannot see ad-hoc queries, so "no rule reads it"
	// is not "nobody reads it". unreferenced never auto-proposes a drop.
	inv, c := fixture()
	got := Compute(inv, c)

	for _, v := range got.Verdicts {
		if v.Metric == "unread_metric" {
			if v.Grade != GradeUnreferenced {
				t.Errorf("Grade = %q, want %q", v.Grade, GradeUnreferenced)
			}
			if v.Droppable {
				t.Error("Droppable = true without a query log, want false")
			}
		}
	}
	if got.DroppableSeries != 0 {
		t.Errorf("DroppableSeries = %d, want 0", got.DroppableSeries)
	}
}

func TestWithAQueryLogAnUnreadMetricBecomesDroppable(t *testing.T) {
	inv, c := fixture()
	c.LogRead = true
	c.LogQualifies = true
	got := Compute(inv, c)

	var found bool
	for _, v := range got.Verdicts {
		if v.Metric != "unread_metric" {
			continue
		}
		found = true
		if v.Grade != GradeUnqueried {
			t.Errorf("Grade = %q, want %q", v.Grade, GradeUnqueried)
		}
		if !v.Droppable {
			t.Error("Droppable = false, want true")
		}
	}
	if !found {
		t.Fatal("unread_metric missing from the verdicts")
	}
	if got.DroppableSeries != 400 {
		t.Errorf("DroppableSeries = %d, want 400", got.DroppableSeries)
	}
}

func TestARecordingRuleOutputIsNeverDroppable(t *testing.T) {
	inv, c := fixture()
	c.LogRead = true
	c.LogQualifies = true
	for _, v := range Compute(inv, c).Verdicts {
		if v.Metric == "job:rec" && v.Droppable {
			t.Error("job:rec: Droppable = true — dropping a recording rule's output needs a rule-file edit, not a scrape-config one")
		}
	}
}

// TestDroppableSeriesClampsRatherThanOverflowsNegative pins F3: two
// droppable metrics with math.MaxInt-1 series each sum to more than
// math.MaxInt can represent. Plain int addition wraps that into a
// negative DroppableSeries; saturating addition clamps to math.MaxInt
// instead -- still wrong, but visibly so rather than silently negative.
func TestDroppableSeriesClampsRatherThanOverflowsNegative(t *testing.T) {
	huge := math.MaxInt - 1
	inv := inventory.Build(promapi.Status{
		Counts:     map[string]int{"a": huge, "b": huge},
		HeadSeries: huge,
	})
	c := corpus.Corpus{Queries: 1, LogRead: true, LogQualifies: true}
	got := Compute(inv, c)

	if got.DroppableSeries < 0 {
		t.Fatalf("DroppableSeries went negative on overflow: %d", got.DroppableSeries)
	}
	if got.DroppableSeries != math.MaxInt {
		t.Errorf("DroppableSeries = %d, want it clamped to math.MaxInt (%d)", got.DroppableSeries, math.MaxInt)
	}
}

func TestABlockedCorpusForbidsEveryDrop(t *testing.T) {
	inv, c := fixture()
	c.Blocked = []string{"rule g/Broken: parse query: unexpected character"}
	c.LogRead = true
	c.LogQualifies = true
	got := Compute(inv, c)

	if got.DroppableSeries != 0 {
		t.Errorf("DroppableSeries = %d, want 0: an unreadable query may reference anything", got.DroppableSeries)
	}
	if len(got.Blocked) != 1 {
		t.Errorf("Blocked = %v, want it carried through to the report", got.Blocked)
	}
}

// Compute no longer takes a haveQueryLog bool. The corpus carries that
// evidence, so a caller cannot assert it without it being true -- which is
// what the old hardcoded false was protecting against.
func TestUnqueriedRequiresAQualifyingLog(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "m", Series: 10}}, TotalSeries: 10}

	withLog := Compute(inv, corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		LogRead: true, LogQualifies: true,
	})
	if withLog.Verdicts[0].Grade != GradeUnqueried {
		t.Errorf("grade = %q, want unqueried when a qualifying log saw no read", withLog.Verdicts[0].Grade)
	}
	if !withLog.Verdicts[0].Droppable {
		t.Error("a metric graded unqueried should be droppable")
	}

	shortLog := Compute(inv, corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		LogRead: true, LogQualifies: false,
	})
	if shortLog.Verdicts[0].Grade != GradeUnreferenced {
		t.Errorf("grade = %q, want unreferenced when the log is too short", shortLog.Verdicts[0].Grade)
	}
	if shortLog.Verdicts[0].Droppable {
		t.Error("a too-short log must not license a drop")
	}
}

// Grafana configured but unreachable withholds every drop. "No dashboard
// reads it" and "nobody looked" produce identical numbers and opposite
// meanings.
func TestUnreachableGrafanaWithholdsEveryDrop(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "m", Series: 10}}, TotalSeries: 10}
	res := Compute(inv, corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		LogRead: true, LogQualifies: true,
		DashboardsConfigured: true, DashboardsReachable: false,
	})
	if res.Verdicts[0].Droppable {
		t.Error("a drop was licensed while dashboard evidence was configured and unavailable")
	}
}

// TestABlockedRuleExplainsItself pins the fix for the dead-code bug: a
// metric withheld because a rule could not be read must SAY SO. Before the
// fix, Droppable was only ever set via `!blocked` in the default branch, so
// the `if blocked && v.Droppable` block that assigns the "blocked: ..."
// reason could never run, and every withheld metric showed the generic
// unqueried reason instead.
func TestABlockedRuleExplainsItself(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "m", Series: 10}}, TotalSeries: 10}
	res := Compute(inv, corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		LogRead: true, LogQualifies: true,
		Blocked: []string{"rule g/Broken: parse query: unexpected character"},
	})
	v := res.Verdicts[0]
	if v.Droppable {
		t.Error("Droppable = true while a rule is blocked, want false")
	}
	if !strings.Contains(v.Reason, "blocked: a query could not be read") {
		t.Errorf("Reason = %q, want it to name the blocked-rule cause", v.Reason)
	}
}

// TestUnreachableDashboardsExplainThemselves is the dashboard-cause sibling:
// a metric withheld because Grafana was configured but unreachable must
// name THAT cause, not the generic blocked-rule one, and Result must expose
// DashboardsMissing so the report layer (Task 6) does not have to
// re-derive it.
func TestUnreachableDashboardsExplainThemselves(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "m", Series: 10}}, TotalSeries: 10}
	res := Compute(inv, corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		LogRead: true, LogQualifies: true,
		DashboardsConfigured: true, DashboardsReachable: false,
	})
	v := res.Verdicts[0]
	if v.Droppable {
		t.Error("Droppable = true while dashboard evidence is missing, want false")
	}
	if !strings.Contains(v.Reason, "dashboard evidence was configured but could not be fetched") {
		t.Errorf("Reason = %q, want it to name the dashboard cause, not the query cause", v.Reason)
	}
	if strings.Contains(v.Reason, "blocked: a query could not be read") {
		t.Errorf("Reason = %q, wrongly names the query cause instead of the dashboard cause", v.Reason)
	}
	if !res.DashboardsMissing {
		t.Error("Result.DashboardsMissing = false, want true")
	}
}

// TestUsedReasonIsUnchangedByTheBlockedFix confirms the restructuring in
// Compute's default branch did not touch the used/produced branches: their
// Reason strings and Droppable=false must be exactly as before.
func TestUsedReasonIsUnchangedByTheBlockedFix(t *testing.T) {
	inv, c := fixture()
	got := Compute(inv, c)

	for _, v := range got.Verdicts {
		switch v.Metric {
		case "used_metric":
			if v.Reason != "read by a rule" {
				t.Errorf("used_metric: Reason = %q, want %q", v.Reason, "read by a rule")
			}
			if v.Droppable {
				t.Error("used_metric: Droppable = true, want false")
			}
		case "job:rec":
			if v.Reason != "written by a recording rule" {
				t.Errorf("job:rec: Reason = %q, want %q", v.Reason, "written by a recording rule")
			}
			if v.Droppable {
				t.Error("job:rec: Droppable = true, want false")
			}
		}
	}
}

// TestUsedReasonNamesTheActualSource is the acceptance test for the
// report that attributed dashboard and query-log evidence to rules. On the
// live fixture this printed "jetsam_demo_requests_total used read by a
// rule" and "node_uname_info used read by a rule" -- of two metrics no
// rule reads, and the two this sub-project exists to protect. A reviewer
// who searches the rules for either name finds nothing, and the reasonable
// conclusion from finding nothing is that jetsam is wrong.
func TestUsedReasonNamesTheActualSource(t *testing.T) {
	inv := inventory.Build(promapi.Status{
		Counts:     map[string]int{"rule_metric": 1, "dash_metric": 1, "logged_metric": 1, "shared_metric": 1},
		HeadSeries: 4,
	})
	c := corpus.Build(corpus.Sources{
		Rules: []promapi.Rule{
			{Group: "g", Name: "r", Type: "alerting", Query: `rule_metric > 0`},
			{Group: "g", Name: "s", Type: "alerting", Query: `shared_metric > 0`},
		},
		Dashboards: []grafana.Dashboard{{UID: "d", Title: "D",
			Queries: []string{`rate(dash_metric[5m])`, `shared_metric`}}},
		QueryLog:             &querylog.Reading{Queries: []string{"logged_metric"}, Span: 800 * time.Hour},
		LogQualifies:         true,
		DashboardsConfigured: true, DashboardsReachable: true,
	}, []string{"rule_metric", "dash_metric", "logged_metric", "shared_metric"})

	want := map[string]string{
		"rule_metric":   "read by a rule",
		"dash_metric":   "read by a dashboard",
		"logged_metric": "read by a logged query",
		"shared_metric": "read by a rule and a dashboard",
	}
	for _, v := range Compute(inv, c).Verdicts {
		if v.Grade != GradeUsed {
			t.Errorf("%s: Grade = %q, want %q", v.Metric, v.Grade, GradeUsed)
		}
		if v.Reason != want[v.Metric] {
			t.Errorf("%s: Reason = %q, want %q", v.Metric, v.Reason, want[v.Metric])
		}
	}
}

// TestAnUnreadableQueryLogWithholdsEveryDrop: an unusable log is the
// absence of negative evidence, not evidence of absence. It must withhold
// exactly as an unreachable Grafana does.
func TestAnUnreadableQueryLogWithholdsEveryDrop(t *testing.T) {
	inv, c := fixture()
	c.LogUnreadable = true
	got := Compute(inv, c)

	if !got.QueryLogMissing {
		t.Error("Result.QueryLogMissing = false, want true")
	}
	for _, v := range got.Verdicts {
		if v.Droppable {
			t.Errorf("%s: Droppable = true although the query log could not be read", v.Metric)
		}
	}
	if got.DroppableSeries != 0 {
		t.Errorf("DroppableSeries = %d, want 0", got.DroppableSeries)
	}
	for _, v := range got.Verdicts {
		if v.Metric != "unread_metric" {
			continue
		}
		if !strings.Contains(v.Reason, "could not be read") {
			t.Errorf("unread_metric: Reason = %q, does not say the log could not be read", v.Reason)
		}
	}
}

// TestUnreferencedReasonNamesWhyTheLogDoesNotHelp: "no query log" said of
// a log that is configured and merely short sends an operator to configure
// something they already configured.
func TestUnreferencedReasonNamesWhyTheLogDoesNotHelp(t *testing.T) {
	inv, c := fixture()
	c.LogRead = true
	c.LogSpan = 3 * time.Hour
	c.LogQualifies = false

	for _, v := range Compute(inv, c).Verdicts {
		if v.Metric != "unread_metric" {
			continue
		}
		if strings.Contains(v.Reason, "no query log") {
			t.Errorf("Reason = %q claims no query log although one is configured and short", v.Reason)
		}
		if !strings.Contains(v.Reason, "short of the configured minimum") {
			t.Errorf("Reason = %q does not say the configured log is too short", v.Reason)
		}
	}
}

// TestUnqueriedReasonDoesNotClaimADashboardSearchThatDidNotHappen: this is
// the row that proposes a deletion, and it asserted "no rule or dashboard
// reads it" on installs where grafana.url is unset and no dashboard was
// ever consulted -- while the header two lines above correctly said the
// queries came from rules and the query log alone. Exactly the cross-layer
// contradiction the used-reason fix was raised for, in the configuration
// the query log's blind spot already makes dangerous.
func TestUnqueriedReasonDoesNotClaimADashboardSearchThatDidNotHappen(t *testing.T) {
	inv := inventory.Build(promapi.Status{Counts: map[string]int{"unread_metric": 400}, HeadSeries: 400})
	log := func() *querylog.Reading {
		return &querylog.Reading{Queries: []string{"other_metric"}, Span: 800 * time.Hour}
	}

	withDashboards := corpus.Build(corpus.Sources{
		QueryLog: log(), LogQualifies: true,
		DashboardsConfigured: true, DashboardsReachable: true,
	}, []string{"unread_metric", "other_metric"})
	noDashboards := corpus.Build(corpus.Sources{
		QueryLog: log(), LogQualifies: true,
	}, []string{"unread_metric", "other_metric"})

	reasonFor := func(c corpus.Corpus) string {
		for _, v := range Compute(inv, c).Verdicts {
			if v.Metric == "unread_metric" {
				if v.Grade != GradeUnqueried {
					t.Fatalf("test setup: Grade = %q, want %q", v.Grade, GradeUnqueried)
				}
				return v.Reason
			}
		}
		t.Fatal("test setup: unread_metric has no verdict")
		return ""
	}

	const want = "no rule or dashboard reads it and no logged query read it in the window"
	if got := reasonFor(withDashboards); got != want {
		t.Errorf("dashboards consulted: Reason = %q, want %q", got, want)
	}
	const wantNoDash = "no rule reads it and no logged query read it in the window"
	if got := reasonFor(noDashboards); got != wantNoDash {
		t.Errorf("no dashboards consulted: Reason = %q, want %q", got, wantNoDash)
	}
}

// TestUnqueriedReasonDoesNotCreditAnUnreachableGrafana: configured but
// unfetchable dashboards contributed nothing, so the sentence must not
// claim they were searched either. (Nothing is droppable in this state, so
// the reason reaching a reader is the blocked one -- this asserts the
// phrase the corpus would supply, via the same predicate.)
func TestUnqueriedReasonDoesNotCreditAnUnreachableGrafana(t *testing.T) {
	c := corpus.Build(corpus.Sources{DashboardsConfigured: true}, nil)
	if c.DashboardsRead() {
		t.Error("DashboardsRead = true although Grafana was configured and unreachable")
	}
}
