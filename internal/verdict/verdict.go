// Package verdict joins what a Prometheus stores against what anything
// reads, and says which metrics are safe to stop storing.
package verdict

import (
	"fmt"
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
	// QueryLogMissing is true when a query log was configured and could not
	// be read. Like DashboardsMissing it withholds every drop: evidence
	// that was meant to exist and does not is not the same as evidence
	// nobody asked for, and only the second is safe to grade around.
	QueryLogMissing bool
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
	res := Result{Blocked: c.Blocked, DashboardsMissing: dashboardsMissing, QueryLogMissing: c.LogUnreadable}
	blocked := len(c.Blocked) > 0 || dashboardsMissing || c.LogUnreadable
	haveQueryLog := c.LogRead && c.LogQualifies

	for _, m := range inv.Metrics {
		v := Verdict{Metric: m.Name, Series: m.Series}
		switch {
		case c.Used[m.Name]:
			v.Grade = GradeUsed
			// Which source read it, named from the corpus rather than
			// assumed. The corpus was rules-only when this sentence was
			// written and has not been for some time: saying "read by a
			// rule" of a metric only a dashboard reads sends a reviewer to
			// a file that does not mention it, and the conclusion they
			// reasonably draw from not finding it is that the metric is
			// droppable after all.
			v.Reason = c.UsedBy[m.Name].Reason()
			if v.Reason == "" {
				// A corpus that records Used without UsedBy (a hand-built
				// one in a test, never Build's output). Say less rather
				// than name a source that may be wrong.
				v.Reason = "read by a known query"
			}
		case c.Produced[m.Name]:
			v.Grade = GradeUsed
			v.Reason = "written by a recording rule"
		case !haveQueryLog:
			v.Grade = GradeUnreferenced
			v.Reason = "nothing known reads it; " + c.LogShortfall() + ", so ad-hoc reads are invisible"
		default:
			v.Grade = GradeUnqueried
			// "or dashboard" only when a dashboard was actually
			// consulted. This is the row that proposes a deletion, and on
			// an install with no grafana.url the unconditional wording
			// asserted a search that never happened.
			v.Reason = fmt.Sprintf("no %s reads it and no logged query read it in the window", c.Readers())
			v.Droppable = true
		}

		// Withheld for a reason the operator can act on. This is the only
		// place Droppable is negated, so the reason and the decision cannot
		// drift apart -- an earlier shape set Droppable from !blocked in the
		// branch above, which made this block unreachable and left every
		// withheld metric showing the generic reason.
		if blocked && v.Droppable {
			v.Droppable = false
			switch {
			case dashboardsMissing:
				v.Reason = "blocked: dashboard evidence was configured but could not be fetched"
			case c.LogUnreadable:
				v.Reason = "blocked: the query log was configured but could not be read"
			default:
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
