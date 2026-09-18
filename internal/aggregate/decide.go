// Package aggregate decides which metrics can be recorded in a smaller,
// pre-aggregated form and have their raw series dropped.
//
// The claim here is much stronger than a drop's. A drop says "nothing reads
// this". An aggregation says "every consumer of this needs only these
// labels, so the rest of the label space can stop being stored" -- and it
// is wrong the moment a single consumer was missed. Everything in this file
// therefore refuses on any doubt: a refusal costs an optimisation, a wrong
// proposal costs data something still reads.
package aggregate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
)

// Proposal is a metric Decide judged safe to collapse to its Keep labels
// under Op.
type Proposal struct {
	Metric    string
	Keep      []string // labels that survive, sorted
	Op        string   // "sum", "min" or "max"
	RawSeries int
	// KeptSeries is left at its zero value here: this package has no label
	// list to run a `count by (...)` query against, only a name and a
	// series count. The caller fills this in from a real count query
	// before comparing it against RawSeries -- see the task-3 brief's
	// correction on why that comparison does not belong in Decide.
	KeptSeries int
	RuleName   string
	Consumers  []Consumer
}

// Consumer names one thing that reads a proposed metric, for the pull
// request body that asks a human to approve the collapse.
type Consumer struct {
	Kind  string // "rule"
	Group string
	Name  string
	Query string
}

// Refusal is a metric Decide would not propose, and why -- the "why" is the
// only thing standing between a refusal and a reader assuming jetsam missed
// an easy win.
type Refusal struct {
	Metric string
	Reason string
}

// Decide walks every metric in inv that the corpus has a Needs entry for,
// and proposes collapsing it when every consumer agrees on both the label
// set and the operator, and nothing jetsam cannot rewrite reads it.
//
// A metric with no Needs entry is skipped, not refused: absence means
// nothing in the corpus touches it at all, which is the drop path's
// business, and reading the zero MetricNeed as "needs nothing" would treat
// the most permissive value the type holds as sufficient evidence that
// dropping every label is safe. See the comment on corpus.Corpus.Needs.
//
// dashboardConsumers names, for a metric a dashboard reads, the dashboards
// that read it, so the refusal can name them. It is not the only source of
// an unrewritable read, though: c.UsedBy also carries FromDashboard and
// FromLog bits, and a logged ad-hoc query is exactly as unrewritable as a
// dashboard panel -- it passes the label-union test today and breaks the
// moment anyone re-runs it once the raw series are gone. So both are
// checked independently, and dashboardConsumers is kept only to name the
// blocking dashboards when it can.
func Decide(inv inventory.Inventory, c corpus.Corpus, dashboardConsumers map[string][]string) ([]Proposal, []Refusal) {
	var proposals []Proposal
	var refusals []Refusal

	for _, m := range inv.Metrics {
		need, ok := c.Needs[m.Name]
		if !ok {
			continue
		}

		if reason, refused := refuse(m.Name, need, c, dashboardConsumers); refused {
			refusals = append(refusals, Refusal{Metric: m.Name, Reason: reason})
			continue
		}

		keep := append([]string(nil), need.Required...)
		sort.Strings(keep)
		op := need.Ops[0]

		proposals = append(proposals, Proposal{
			Metric:    m.Name,
			Keep:      keep,
			Op:        op,
			RawSeries: m.Series,
			RuleName:  strings.Join(keep, "_") + ":" + m.Name + ":" + op,
		})
	}

	return proposals, refusals
}

// refuse checks the causes in the brief's order and returns the first one
// that fires, so a refusal always names the single reason that decided it
// rather than whichever check happened to run last.
func refuse(metric string, need corpus.MetricNeed, c corpus.Corpus, dashboardConsumers map[string][]string) (string, bool) {
	if dashboards := dashboardConsumers[metric]; len(dashboards) > 0 {
		return fmt.Sprintf("read by dashboard(s) jetsam cannot rewrite: %s", strings.Join(dashboards, ", ")), true
	}
	if c.UsedBy[metric]&corpus.FromDashboard != 0 {
		return "read by a dashboard jetsam cannot rewrite", true
	}
	if c.UsedBy[metric]&corpus.FromLog != 0 {
		return "read by a query in the query log, which jetsam cannot rewrite", true
	}
	if need.All {
		reason := "some consumer needs every label"
		if len(need.Blockers) > 0 {
			reason += ": " + strings.Join(need.Blockers, "; ")
		}
		return reason, true
	}
	if len(need.Ops) != 1 {
		return fmt.Sprintf("consumers disagree on the aggregation operator: %s", strings.Join(need.Ops, ", ")), true
	}
	if !need.AllOpsSafe {
		return fmt.Sprintf("the %s operator does not survive partial aggregation", need.Ops[0]), true
	}
	return "", false
}
