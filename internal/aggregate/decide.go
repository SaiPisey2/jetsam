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
	Metric string
	Keep   []string // labels that survive, sorted
	Op     string   // "sum", "min" or "max"
	// Fn is "rate", "irate", "increase", or "" for the metric itself, and
	// Window is its range (e.g. "5m"), empty when Fn is. When Fn is set,
	// the rule records the RATE, not the counter: summing a counter across
	// series and rating the sum afterward is not the same number as summing
	// the rates directly, because the sum drops whenever one series resets
	// or disappears, and rate reads that drop as a counter reset of its
	// own. The person approving this collapse is trusting that the rule
	// named here is the rate, so it stays correct across a pod restart.
	Fn        string
	Window    string
	RawSeries int
	// KeptSeries is left at its zero value here: this package has no label
	// list to run a `count by (...)` query against, only a name and a
	// series count. The caller fills this in from a real count query
	// before comparing it against RawSeries -- that comparison needs the
	// count query's actual result, which does not exist at this point in
	// the pipeline.
	KeptSeries int
	RuleName   string
	// Consumers is left empty here: this package has no rule list to
	// populate it from, only the corpus's summarised Needs. A later step
	// fills it in from the rules themselves when it builds the request
	// that asks a human to approve the collapse, the same way it fills in
	// KeptSeries from a count query.
	Consumers []Consumer
	// Caveats names something Decide judged safe to propose anyway, but
	// that the person approving it should see before acting on the
	// numbers. Today the only source is corpus.FromLog: a query the
	// query log recorded is a historical event, not a standing consumer
	// -- it ran once, possibly weeks ago, and its label requirements
	// already entered need's union in corpus.Build, so the safety proof
	// this package exists to make is untouched by it. What it costs is
	// only the guarantee that RE-RUNNING that exact query later returns
	// the same numbers, which is the operator's call to make once this
	// is advisory-only and nothing gets applied without them reading it
	// first -- unlike corpus.FromDashboard, which keeps reading for as
	// long as the dashboard exists and stays a hard refusal in refuse().
	Caveats []string
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
// and proposes collapsing it when the corpus's own evidence is complete, no
// dashboard jetsam cannot rewrite reads it, it is not itself a recording
// rule's output, and every consumer agrees on both the label set and the
// operator. A metric a logged query also read is still proposed -- see
// Proposal.Caveats -- because that query is a historical event, not a
// standing consumer the way a dashboard is.
//
// A metric with no Needs entry is skipped, not refused: absence means
// nothing in the corpus touches it at all, which is the drop path's
// business, and reading the zero MetricNeed as "needs nothing" would treat
// the most permissive value the type holds as sufficient evidence that
// dropping every label is safe. See the comment on corpus.Corpus.Needs.
//
// dashboardConsumers names, for a metric a dashboard reads, the dashboards
// that read it, so the refusal can name them. It is not the only source of
// a dashboard read, though: c.UsedBy also carries a FromDashboard bit for a
// dashboard read that never made it into dashboardConsumers (an unparseable
// panel, say), and that bit is checked independently so dashboardConsumers
// missing an entry never gets read as permission. Neither one, though, can
// see a read that never made it into the corpus at all -- see the
// evidence-completeness check inside refuse.
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

		// fn/win default to the metric-itself case. need.Fns is normalized
		// to that same case when empty -- see the comment in refuse -- so
		// this mirrors that check rather than re-deciding it.
		var fn, win string
		if len(need.Fns) == 1 && need.Fns[0] != "" {
			fn = need.Fns[0]
			win = need.Windows[0]
		}

		// keep can be empty here: All is false and Required is nil means the
		// one consumer that touches this metric aggregates it down to a
		// single series and needs no label at all -- collapsing 400 series
		// to 1 is the correct answer for that consumer, not an accident.
		// See RuleName's own doc comment for why it, not this loop, owns the
		// naming convention.
		ruleName := RuleName(m.Name, keep, op, fn, win)

		var caveats []string
		if c.UsedBy[m.Name]&corpus.FromLog != 0 {
			// See Proposal.Caveats' own doc comment for why this is a
			// caveat, not a refusal: the query log records what someone
			// ran once, not what keeps running, and its label
			// requirements are already folded into need above -- what is
			// NOT proven is that re-running that exact query later still
			// returns what it did.
			caveats = append(caveats, "queried ad hoc within the query log's window: jetsam cannot rewrite a "+
				"query it only saw in a log, and whoever ran it will find their saved query no longer returns "+
				"what it did once this collapses the raw series")
		}

		proposals = append(proposals, Proposal{
			Metric:    m.Name,
			Keep:      keep,
			Op:        op,
			Fn:        fn,
			Window:    win,
			RawSeries: m.Series,
			RuleName:  ruleName,
			Caveats:   caveats,
		})
	}

	return proposals, refusals
}

// RuleName returns the conventional level:metric:operation name a rule
// collapsing metric to keep under op -- further wrapped in fn over window,
// when fn is set -- records under. Decide builds every Proposal's RuleName
// by calling this, rather than by inlining the same string-building logic,
// so that a consumer with only the finished Proposal in hand -- emit's
// RenderRule, which never sees the Needs that produced it -- can recompute
// the identical name from Proposal's own Metric/Keep/Op/Fn/Window and
// refuse to write a rule whose declared name has drifted from what those
// fields would actually record. Two implementations of this string that
// happen to agree today are worse than one: the day they diverge, the
// wrong one is silent, and a rule that looks entirely ordinary in a diff
// records under a name nothing reads, or overwrites a name something else
// does.
//
// keep is trusted to already be sorted, exactly as Proposal.Keep documents
// it -- this function does not sort it again.
func RuleName(metric string, keep []string, op, fn, window string) string {
	// The leading colon (":m_total:sum") is the conventional rendering of
	// an empty level in level:metric:operation, so the name stays
	// idiomatic even when keep is empty -- a consumer that aggregates the
	// metric down to a single series and needs no label at all is a real
	// case, not an oversight.
	name := strings.Join(keep, "_") + ":" + metric + ":" + op
	if fn != "" {
		// sum_rate5m, not sum_rate_5m: the function and its window read as
		// one token, the same way Prometheus's own rules do it.
		name += "_" + fn + window
	}
	return name
}

// refuse checks the causes in order and returns the first one that fires,
// so a refusal always names the single reason that decided it rather than
// whichever check happened to run last.
func refuse(metric string, need corpus.MetricNeed, c corpus.Corpus, dashboardConsumers map[string][]string) (string, bool) {
	// The corpus itself can be incomplete: a rule that would not parse, a
	// Grafana that was configured but never answered, a query log that was
	// configured but could not be read. The drop path withholds every drop
	// over exactly these three (internal/verdict's "blocked"), and this
	// path needs the gate MORE, not less: a drop only claims silence, an
	// aggregation claims every consumer was seen and agreed. When Grafana
	// is unreachable, a metric a dashboard actually reads carries no
	// FromDashboard bit and appears in no dashboardConsumers entry -- that
	// state is indistinguishable from "no dashboard reads it" everywhere
	// else in this file, so it must be caught here, once, before either of
	// those silences gets read as permission.
	switch {
	case len(c.Blocked) > 0:
		return "a rule did not parse, so the corpus cannot vouch for every consumer", true
	case c.DashboardsConfigured && !c.DashboardsReachable:
		return "Grafana is configured but was not reachable, so dashboard reads of this metric cannot be ruled out", true
	case c.LogUnreadable:
		return "the query log is configured but could not be read, so logged reads of this metric cannot be ruled out", true
	}

	// A recording rule's output is not scraped -- it is computed from other
	// series -- so there are no raw series here for a scrape config to stop
	// collecting. Proposing one anyway would ask for a rule-file edit that
	// this package cannot make and a drop that has nothing to remove: a
	// no-op dressed as an optimisation. See corpus.Corpus.Produced.
	if c.Produced[metric] {
		return "this metric is a recording rule's output, not a scraped series, so there is nothing here to collapse", true
	}

	if dashboards := dashboardConsumers[metric]; len(dashboards) > 0 {
		return fmt.Sprintf("read by dashboard(s) jetsam cannot rewrite: %s", strings.Join(dashboards, ", ")), true
	}
	if c.UsedBy[metric]&corpus.FromDashboard != 0 {
		return "read by a dashboard jetsam cannot rewrite", true
	}
	// corpus.FromLog is deliberately NOT checked here. A dashboard keeps
	// reading for as long as it exists and jetsam cannot edit Grafana, so
	// it stays a hard refusal above -- but a query the query log recorded
	// is a historical event, not a standing consumer: it ran once,
	// possibly weeks ago, and its label requirements already entered
	// need's union in corpus.Build, so the safety proof this function
	// exists to make does not depend on it. What a logged reader costs is
	// only the guarantee that re-running that exact query later returns
	// the same numbers -- under this package's advisory-only contract
	// (nothing is ever applied without a human reading it first), that is
	// the operator's call to make with the fact in front of them, so
	// Decide records it on Proposal.Caveats instead of refusing here.
	if need.All {
		// need.All means jetsam's requirement is "every label must survive",
		// which is NOT the same claim as "some consumer needs every label":
		// that is true for a consumer that genuinely passes every label
		// through unchanged (a bare selector, topk, count_values), but false
		// for one that only reads a few labels and got All for some other
		// reason -- an unsupported function, a join below the aggregate,
		// two disagreeing claims in one query, a `without`/`ignoring` clause
		// whose complement jetsam cannot compute, or evidence it could not
		// read at all. Blockers is set in exactly those cases (see
		// LabelNeed.Blocker), so its presence is what tells the two apart:
		// a reader who opens `sum by (path)` must not be told the consumer
		// wanted every label when it only ever asked for path.
		if len(need.Blockers) > 0 {
			return "every label of this metric must survive: " + strings.Join(need.Blockers, "; "), true
		}
		return "some consumer needs every label", true
	}
	// Blockers is documented as "human-readable reasons aggregation is
	// impossible" -- corpus only ever sets one alongside All today, which
	// would make this branch unreachable through corpus.Build, but this
	// package does not get to assume that invariant holds forever in a
	// package it does not own. A non-empty Blockers on its own is reason
	// enough to refuse, All or not.
	if len(need.Blockers) > 0 {
		return "aggregation is impossible: " + strings.Join(need.Blockers, "; "), true
	}
	if len(need.Ops) == 0 {
		// Not the same failure as disagreement below: no consumer that
		// aggregates this metric was ever seen, so there is no operator to
		// name it as safe or unsafe, and saying "disagree" here would point
		// a reader at a disagreement that does not exist -- exactly the
		// mistake Source.Reason's doc comment warns against for the same
		// reason: it reads as jetsam being wrong rather than the input
		// being what it is.
		return "no consumer aggregates it, so there is no operator to record it under", true
	}
	if len(need.Ops) > 1 {
		return fmt.Sprintf("consumers disagree on the aggregation operator: %s", strings.Join(need.Ops, ", ")), true
	}
	if !need.AllOpsSafe {
		return fmt.Sprintf("the %s operator does not survive partial aggregation", need.Ops[0]), true
	}
	// AllOpsSafe already rules this out for anything corpus.Build produces
	// today -- composes only ever sets it true for sum, min or max. Checked
	// anyway rather than trusted from a distance: a MetricNeed built by
	// something other than corpus.Build, or by a future corpus.Build that
	// grows a new safe operator without updating this list, must not slip
	// through on AllOpsSafe alone.
	if op := need.Ops[0]; op != "sum" && op != "min" && op != "max" {
		return fmt.Sprintf("%s is not sum, min or max, the only operators this package knows how to record", op), true
	}
	// Fns is empty for a MetricNeed built directly by a test written before
	// Fn existed (this package's own tests among them) -- and, per
	// corpus.foldNeeds' own invariant, is never empty for a metric a real
	// corpus.Build actually has a Needs entry for. Both cases mean the same
	// thing here: no function was ever claimed, so there is nothing to
	// disagree about. Only an actual pair of DIFFERENT claims -- rate and
	// irate, or the metric itself and rate -- reaches the len(need.Fns) > 1
	// branch below.
	if len(need.Fns) > 1 {
		return fmt.Sprintf("consumers disagree on the range function: %s", strings.Join(renderFns(need.Fns), ", ")), true
	}
	if len(need.Fns) == 1 && need.Fns[0] != "" && len(need.Windows) != 1 {
		return fmt.Sprintf("consumers disagree on the range window: %s", strings.Join(need.Windows, ", ")), true
	}
	return "", false
}

// renderFns renders Fns for a human reading a refusal reason: the empty
// string is a real member of the set (see corpus.MetricNeed.Fns), and
// printing it unlabelled would read as a blank rather than as "one consumer
// reads the metric itself."
func renderFns(fns []string) []string {
	out := make([]string, len(fns))
	for i, fn := range fns {
		if fn == "" {
			out[i] = "the metric itself"
		} else {
			out[i] = fn
		}
	}
	return out
}
