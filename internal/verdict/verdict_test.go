package verdict

import (
	"testing"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
)

func fixture() (inventory.Inventory, corpus.Corpus) {
	inv := inventory.Build(map[string]int{"used_metric": 100, "unread_metric": 400, "job:rec": 5})
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
	got := Compute(inv, c, false)

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
	got := Compute(inv, c, true)

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
	for _, v := range Compute(inv, c, true).Verdicts {
		if v.Metric == "job:rec" && v.Droppable {
			t.Error("job:rec: Droppable = true — dropping a recording rule's output needs a rule-file edit, not a scrape-config one")
		}
	}
}

func TestABlockedCorpusForbidsEveryDrop(t *testing.T) {
	inv, c := fixture()
	c.Blocked = []string{"rule g/Broken: parse query: unexpected character"}
	got := Compute(inv, c, true)

	if got.DroppableSeries != 0 {
		t.Errorf("DroppableSeries = %d, want 0: an unreadable query may reference anything", got.DroppableSeries)
	}
	if len(got.Blocked) != 1 {
		t.Errorf("Blocked = %v, want it carried through to the report", got.Blocked)
	}
}
