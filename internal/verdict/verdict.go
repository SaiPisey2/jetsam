// Package verdict joins what a Prometheus stores against what anything
// reads, and says which metrics are safe to stop storing.
package verdict

import (
	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
)

// Grade is how strong the evidence behind a verdict is. It is an axis of
// its own, deliberately separate from whether a metric is read: the same
// finding carries different weight depending on what jetsam could see.
type Grade string

const (
	// GradeUsed: some known query reads this metric.
	GradeUsed Grade = "used"
	// GradeUnreferenced: no rule and no dashboard names it, but ad-hoc and
	// Explore queries are invisible. Never auto-proposed for dropping.
	GradeUnreferenced Grade = "unreferenced"
	// GradeUnqueried: a query log covering the window also shows nothing
	// read it. Full confidence.
	GradeUnqueried Grade = "unqueried"
)

// Verdict is one metric's answer.
type Verdict struct {
	Metric    string
	Series    int
	Grade     Grade
	Reason    string
	Droppable bool
}

// Result is every verdict plus the reasons jetsam declined to act.
type Result struct {
	Verdicts        []Verdict
	Blocked         []string
	DroppableSeries int
}

// Compute grades every metric in the inventory.
//
// haveQueryLog is the difference between "no rule mentions it" and "nobody
// read it". Only the second licenses a drop, which is why an install with
// no query log configured proposes nothing at all by default -- the honest
// outcome, not a broken one.
func Compute(inv inventory.Inventory, c corpus.Corpus, haveQueryLog bool) Result {
	res := Result{Blocked: c.Blocked}
	blocked := len(c.Blocked) > 0

	for _, m := range inv.Metrics {
		v := Verdict{Metric: m.Name, Series: m.Series}
		switch {
		case c.Used[m.Name]:
			v.Grade = GradeUsed
			v.Reason = "read by a rule"
		case c.Produced[m.Name]:
			v.Grade = GradeUsed
			v.Reason = "written by a recording rule"
		case !haveQueryLog:
			v.Grade = GradeUnreferenced
			v.Reason = "no rule reads it; no query log, so ad-hoc reads are invisible"
		default:
			v.Grade = GradeUnqueried
			v.Reason = "no rule reads it and no query read it in the window"
			v.Droppable = !blocked
		}
		if blocked && v.Droppable {
			v.Droppable = false
			v.Reason = "blocked: a query could not be read"
		}
		if v.Droppable {
			res.DroppableSeries += m.Series
		}
		res.Verdicts = append(res.Verdicts, v)
	}
	return res
}
