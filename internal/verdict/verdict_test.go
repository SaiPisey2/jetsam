package verdict

import (
	"math"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
)

func fixture() (inventory.Inventory, corpus.Corpus) {
	inv := inventory.Build(promapi.Status{
		Counts:     map[string]int{"used_metric": 100, "unread_metric": 400, "job:rec": 5},
		HeadSeries: 505,
	})
	c := corpus.Corpus{
		Queries:  3,
		Used:     map[string]bool{"used_metric": true},
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
