// Package verdict joins what a Prometheus stores against what anything
// reads, and says which metrics are safe to stop storing.
package verdict

import (
	"math"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
)

// addSeriesSaturating adds b to a, clamping to math.MaxInt instead of
// wrapping into a negative number when the true sum overflows. Series
// counts are validated non-negative where they enter jetsam (see
// promapi.Client.TSDBStatus), so the only way this overflows is a hostile
// or compromised Prometheus reporting enough individually-plausible counts
// that their sum is not representable. A clamped total is still wrong --
// it understates the true sum -- but it is visibly, absurdly wrong (an
// implausible number of series) rather than silently wrong (a negative
// count of series a human is being asked to approve deleting).
func addSeriesSaturating(a, b int) int {
	sum := a + b
	if b > 0 && sum < a {
		return math.MaxInt
	}
	return sum
}

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
	// DashboardsMissing is true when dashboard evidence was configured but
	// could not be fetched, so the report layer can say so without
	// re-deriving it from the corpus itself.
	DashboardsMissing bool
}

// Compute grades every metric in the inventory.
//
// The corpus itself carries the difference between "no rule mentions it"
// and "nobody read it": LogRead and LogQualifies together are what used to
// be a caller-supplied haveQueryLog bool. Only a log that was both read and
// long enough licenses a drop, which is why an install with no qualifying
// log proposes nothing at all by default -- the honest outcome, not a
// broken one.
func Compute(inv inventory.Inventory, c corpus.Corpus) Result {
	// Dashboard evidence configured but unavailable withholds everything.
	// "No dashboard reads it" and "nobody looked" produce identical numbers
	// and opposite meanings.
	dashboardsMissing := c.DashboardsConfigured && !c.DashboardsReachable
	res := Result{Blocked: c.Blocked, DashboardsMissing: dashboardsMissing}
	blocked := len(c.Blocked) > 0 || dashboardsMissing
	haveQueryLog := c.LogRead && c.LogQualifies

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
			v.Droppable = true
		}

		// Withheld for a reason the operator can act on. This is the only
		// place Droppable is negated, so the reason and the decision cannot
		// drift apart -- an earlier shape set Droppable from !blocked in the
		// branch above, which made this block unreachable and left every
		// withheld metric showing the generic reason.
		if blocked && v.Droppable {
			v.Droppable = false
			if dashboardsMissing {
				v.Reason = "blocked: dashboard evidence was configured but could not be fetched"
			} else {
				v.Reason = "blocked: a query could not be read"
			}
		}
		if v.Droppable {
			res.DroppableSeries = addSeriesSaturating(res.DroppableSeries, m.Series)
		}
		res.Verdicts = append(res.Verdicts, v)
	}
	return res
}
