package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/SaiPisey2/jetsam/internal/safe"
)

// ConsumerFinding is one consumer of a proposal's metric, together with
// what emit.RewriteConsumer did with it: Rewritten and Declined are exactly
// RewriteConsumer's own two return values, carried through unchanged.
//
// The two are not mutually exclusive. RewriteConsumer can rewrite one
// aggregate over the metric AND decline a second, different aggregate over
// the same metric elsewhere in the same expression -- see its own doc
// comment. So Rewritten differing from Consumer.Query is not "fully
// rewritten, ignore Declined": Declined is carried through and printed
// alongside a rewrite exactly when both are non-empty, because a reader who
// sees only the "after" text in that case would conclude this consumer no
// longer touches the raw series, and part of it still does.
//
// Declined non-empty on its own (Rewritten equal to Consumer.Query) means
// RewriteConsumer found an aggregate here and refused it outright -- a
// different fact from "no aggregate of this metric appears in this query at
// all", which is Declined empty and Rewritten unchanged. Aggregate reports
// every one of these states rather than ever leaving a consumer out: a
// consumer silently missing from this list reads, to anyone deciding
// whether the collapse is safe, exactly like one that was never a concern
// in the first place.
type ConsumerFinding struct {
	Consumer  aggregate.Consumer
	Rewritten string
	Declined  string
}

// AggregateFinding is one proposal jetsam has already measured against the
// real Prometheus and rendered, ready to print. This package never measures
// or renders anything itself -- see cmd/jetsam's aggregate command, which
// calls aggregate.Decide, issues the count query, and calls emit.RenderRule
// and emit.RewriteConsumer -- the same separation Scan already keeps from
// verdict.Compute.
type AggregateFinding struct {
	Proposal  aggregate.Proposal
	Rule      string // emit.RenderRule's output: the whole rule file, one rule added.
	Consumers []ConsumerFinding
}

// notAppliedNotice is printed by Aggregate on every call, whether or not
// findings or refusals has anything in it.
//
// A recording rule cannot read a metric that has been dropped at ingest:
// metric_relabel_configs runs before a sample ever reaches the TSDB, and a
// rule only ever queries the TSDB. So the rule this report renders and the
// drop `jetsam propose` knows how to write are mutually exclusive over the
// same metric -- measured, not assumed, against a real Prometheus carrying
// both at once, and the rule's output was empty. The saving this report
// describes is real only one layer upstream of Prometheus (a collector, or
// stream aggregation), never as a drop added to a scrape config here. This
// command never writes anything, and it says so on every run so that a
// reader who applies the rule and the rewrite by hand does not go on to add
// the drop themselves and lose data a rule still reads.
const notAppliedNotice = `Nothing here is applied. Prometheus drops a metric at ingest, before any
rule evaluates over it, so a recording rule built from a metric dropped to
save these series would have nothing left to read -- the saving this
reports is only real one layer upstream of Prometheus (a collector, or
stream aggregation), never as a drop added to a scrape config.`

// Aggregate writes the aggregate report: for each metric named, any caveat
// on it, its raw and aggregated series counts, the labels kept and the
// labels dropped, the operator and range function, the rendered recording
// rule, and each consumer with its query before and after -- then every
// withheld proposal and every refusal, the way Scan lists a blocked rule.
// See notAppliedNotice's own doc comment for why that line comes first and
// unconditionally, and writeFinding for why a caveat comes right after the
// metric's own headline numbers rather than at the end.
//
// sources is the same sentence corpus.Corpus.SourceList() builds for the
// drop path's "nothing reads it" claim, printed here for the same reason:
// this command's own claim is stronger still -- "every consumer of this
// needs only these labels" -- and a reader cannot judge it without knowing
// which sources actually contributed evidence. Naming them once, next to
// the notice that nothing is applied, is cheaper than making a reader
// re-derive it from which caveats happen to be present on which proposal.
//
// withheld is printed even when findings has nothing in it: a run that
// found several candidates and withheld every one of them (an
// unmeasurable proposal, one already recorded by an existing rule, a
// saving below the configured minimum) must not read, to something
// piping only stdout, as indistinguishably clean as a run with no
// candidates at all.
func Aggregate(w io.Writer, sources string, findings []AggregateFinding, withheld []string, refusals []aggregate.Refusal) {
	fmt.Fprintln(w, notAppliedNotice)
	fmt.Fprintf(w, "Evidence sources read: %s.\n", safe.Text(sources))
	fmt.Fprintln(w)

	if len(findings) == 0 {
		fmt.Fprintln(w, "nothing to collapse.")
	}
	for _, f := range findings {
		writeFinding(w, f)
	}

	if len(withheld) > 0 {
		fmt.Fprintf(w, "withheld %d proposal(s):\n", len(withheld))
		for _, wh := range withheld {
			// wh can embed remote-sourced text (a metric name, a rule
			// group or name) the same way a refusal's Reason can.
			fmt.Fprintf(w, "  - %s\n", safe.Text(wh))
		}
		fmt.Fprintln(w)
	}

	if len(refusals) > 0 {
		fmt.Fprintf(w, "refused %d metric(s):\n", len(refusals))
		for _, r := range refusals {
			// Metric and Reason can both embed remote-sourced text -- a
			// metric name, or a query quoted inside a Blocker -- the same
			// trust boundary report.Scan already treats as hostile.
			fmt.Fprintf(w, "  - %s: %s\n", safe.Text(r.Metric), safe.Text(r.Reason))
		}
	}
}

// writeFinding prints one AggregateFinding: the series counts, what
// survives and what does not, the operator, the rendered rule, and every
// consumer.
func writeFinding(w io.Writer, f AggregateFinding) {
	p := f.Proposal
	saved := p.RawSeries - p.KeptSeries
	fmt.Fprintf(w, "%s: %d series -> %d kept (saves %d)\n", safe.Text(p.Metric), p.RawSeries, p.KeptSeries, saved)

	// Caveats come immediately after the headline numbers, before keeps/
	// drops/op/rule -- deliberately the first thing printed about this
	// metric, not appended at the end where a reader who acts on the
	// numbers above could miss it. See aggregate.Proposal.Caveats' own doc
	// comment for what can land here today (a logged, not standing, read).
	for _, c := range p.Caveats {
		fmt.Fprintf(w, "  CAVEAT: %s\n", safe.Text(c))
	}

	keep := "(none -- every consumer collapses it to a single series)"
	if len(p.Keep) > 0 {
		keep = strings.Join(p.Keep, ", ")
	}
	// keep and f.Rule are not reachable with hostile content through this
	// command today (label names come off an already-parsed PromQL query,
	// and f.Rule is jetsam's own rendered YAML), but Aggregate is exported
	// and every other remote-sourced field in this function already goes
	// through safe.Text -- a caller two packages away should not have to
	// know which fields are the exception.
	fmt.Fprintf(w, "  keeps: %s\n", safe.Text(keep))
	// jetsam has no way to name which OTHER labels the raw series carry --
	// only that they do not survive the collapse -- so this states the
	// fact it actually has rather than inventing a label list it does not.
	fmt.Fprintln(w, "  drops: every other label")

	op := p.Op
	if p.Fn != "" {
		op = fmt.Sprintf("%s over %s(%s)", p.Op, p.Fn, p.Window)
	}
	fmt.Fprintf(w, "  op:    %s\n", op)

	fmt.Fprintln(w, "  rule:")
	for _, line := range strings.Split(strings.TrimRight(f.Rule, "\n"), "\n") {
		fmt.Fprintf(w, "    %s\n", safe.Text(line))
	}

	if len(f.Consumers) == 0 {
		fmt.Fprintln(w, "  consumers: none of the rules jetsam read")
	}
	for _, cf := range f.Consumers {
		writeConsumer(w, cf)
	}
	fmt.Fprintln(w)
}

// writeConsumer prints one consumer's before/after, distinguishing the
// three shapes emit.RewriteConsumer's own contract allows:
//
//   - changed, not declined: fully rewritten -- the whole aggregate over
//     this metric was this consumer's only claim on it.
//   - changed AND declined: PARTIALLY rewritten. A consumer's expression
//     can contain one aggregate that matches the proposal (replaced) and
//     another, over the same metric, that does not (left alone) -- see
//     ConsumerFinding's doc comment. Showing only "after" here is exactly
//     how a reader concludes this consumer no longer needs the raw series
//     when part of it still does, so both are printed together.
//   - unchanged: either declined (RewriteConsumer found an aggregate here
//     and refused it outright) or not (no aggregate of this metric appears
//     in this consumer's query at all).
func writeConsumer(w io.Writer, cf ConsumerFinding) {
	c := cf.Consumer
	fmt.Fprintf(w, "  consumer %s %s/%s:\n", safe.Text(c.Kind), safe.Text(c.Group), safe.Text(c.Name))
	fmt.Fprintf(w, "    before: %s\n", safe.Text(c.Query))
	changed := cf.Rewritten != "" && cf.Rewritten != c.Query
	switch {
	case changed && cf.Declined != "":
		fmt.Fprintf(w, "    after:  %s\n", safe.Text(cf.Rewritten))
		fmt.Fprintf(w, "    partially rewritten -- part of this query still reads the raw metric: %s\n", safe.Text(cf.Declined))
	case changed:
		fmt.Fprintf(w, "    after:  %s\n", safe.Text(cf.Rewritten))
	case cf.Declined != "":
		// The point of this branch: a consumer RewriteConsumer declined to
		// touch at all is reported here, not left out. See
		// ConsumerFinding's doc comment.
		fmt.Fprintf(w, "    not rewritable: %s\n", safe.Text(cf.Declined))
	default:
		fmt.Fprintln(w, "    after:  (unchanged -- no aggregate of this metric found here)")
	}
}
