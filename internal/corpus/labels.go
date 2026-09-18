package corpus

import (
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// LabelNeed is what one query needs from one metric.
//
// All is the refusal case and it is the default on any doubt: it means the
// query needs every label the metric carries, so no label can be aggregated
// away without changing what that query returns.
type LabelNeed struct {
	All      bool
	Required []string
	// Op is the aggregation applied directly over the metric, if any, and
	// OpSafe is whether that operator survives partial aggregation.
	Op     string
	OpSafe bool
	// Touches reports whether this query reads this metric at all. It is
	// the difference between "needs no label" and "never looked", which
	// the rest of this struct cannot express: both are the zero value.
	Touches bool
	// Fn is the range function applied to this metric BENEATH the
	// aggregation -- "rate", "irate" or "increase" -- and Window is its
	// range. Empty when the aggregation reads the metric's instant vector
	// directly.
	//
	// It is recorded because summing a counter and then rating the sum is
	// not the same as summing the rates: the sum drops when one series
	// resets or disappears, and rate reads that drop as a counter reset.
	// So the thing worth recording is the rate, not the metric.
	Fn     string
	Window time.Duration
	// Blocker is a human-readable reason for the refusals this file can
	// name specifically: an unsupported range function, a subquery, an
	// instant function applied before aggregation, a vector join below the
	// aggregate, a `without`/`ignoring` clause whose complement cannot be
	// computed here, or two selectors of the same metric in one query
	// making conflicting claims. Left empty for the refusals that genuinely
	// need every label (a bare unaggregated selector, or an aggregator that
	// preserves every series -- topk, bottomk, count_values).
	//
	// It exists because "some consumer needs every label" is FALSE for a
	// consumer whose real requirement this file worked out just fine before
	// an unrelated function, join or clause forced the refusal. A reader
	// who opens the rule and sees `sum by (path)` concludes jetsam is wrong
	// -- worse than no reason at all, and this package has shipped that
	// mistake before. Whether Blocker is set is exactly what lets a reader
	// downstream (decide.refuse) tell that FALSE case apart from the TRUE
	// one, so that each refusal states only what actually holds for it.
	Blocker string
}

// recordableRangeFns lists the range functions this package knows how to
// pre-aggregate safely. Anything else applied over a matrix selector under
// an aggregation refuses instead: max_over_time under sum does not commute
// with pre-aggregation at all, and avg_over_time only commutes while the
// series set stays fixed, which is exactly what a pod restart breaks.
var recordableRangeFns = map[string]bool{"rate": true, "irate": true, "increase": true}

// composes lists the aggregation operators that are idempotent under partial
// aggregation: sum by (X) over sum by (X+Y) equals sum by (X) over the raw
// series, and likewise for min and max.
//
// count is deliberately absent although it looks composable. count by (X)
// over series already collapsed to X+Y counts the COLLAPSED series, not the
// original ones -- a different number, silently. It is rescuable by recording
// count and rewriting the consumer to sum, which is a real but separate case
// and is not implemented here. avg, quantile, stddev and stdvar need more
// state than the aggregated series carries.
var composes = map[parser.ItemType]bool{
	parser.SUM: true,
	parser.MIN: true,
	parser.MAX: true,
}

// preservesEverySeries lists the aggregators that return their INPUT series
// rather than a collapsed one.
//
// This is the trap the whole file exists for. sum(m) and topk(5, m) both
// parse to an AggregateExpr with an empty Grouping and Without false -- they
// are indistinguishable by shape. But sum collapses to nothing while topk
// hands back the original series with every label intact. Reading the
// grouping alone would silently strip labels a dashboard displays.
// Op.IsAggregatorWithParam() does not help either: it is true for quantile,
// which collapses, as well as for topk, which does not.
//
// count_values belongs here for a different reason: it invents a label from
// each sample's VALUE, and that value is only stable over the raw series.
// Pre-collapsing any other label by summing at the source changes what gets
// merged, and so changes the count -- silently, which is exactly the failure
// this file exists to prevent. A `by` or `without` clause on count_values
// does not bound what it needs; it still needs every label of the source.
var preservesEverySeries = map[parser.ItemType]bool{
	parser.TOPK:         true,
	parser.BOTTOMK:      true,
	parser.COUNT_VALUES: true,
}

// LabelsNeeded reports what query needs from metric.
//
// An unparseable query is an error, never an empty need: "this query needs no
// labels" and "jetsam could not read this query" must not be the same answer,
// because the first licenses removing every label.
func LabelsNeeded(query, metric string) (LabelNeed, error) {
	expr, err := promParser.ParseExpr(query)
	if err != nil {
		return LabelNeed{}, fmt.Errorf("parse query: %w", err)
	}

	var need LabelNeed
	req := map[string]bool{}
	// opSeen and fnSeen track whether an EARLIER selector of this same
	// metric, in this same query, already claimed an Op or an Fn/Window --
	// see the comment where they are checked, below.
	var opSeen, fnSeen bool

	parser.Inspect(expr, func(n parser.Node, path []parser.Node) error {
		switch v := n.(type) {
		case *parser.VectorSelector:
			if !selectorTouches(v, metric) {
				return nil
			}
			need.Touches = true
			// Every matcher on the selector is a label the query depends on.
			for _, m := range v.LabelMatchers {
				if m.Name != "__name__" {
					req[m.Name] = true
				}
			}
			agg, aggIdx := enclosingAggregate(path)
			if agg == nil {
				// Not aggregated: the query returns these series as they
				// are, so every label is part of the answer.
				need.All = true
				return nil
			}

			op := agg.Op.String()
			opSafe := composes[agg.Op]
			if opSeen && op != need.Op {
				// parser.Inspect visits EVERY selector of this metric in the
				// query, and Op is a single field: if this metric appears
				// twice under different operators (`sum(...) / max(...)`),
				// the second assignment would silently speak for both,
				// which is the same "one claim wins" trap Fn/Window guard
				// against below. Refuse rather than let visit order decide.
				need.All = true
				need.Blocker = fmt.Sprintf("this query aggregates it under more than one operator: %s and %s", need.Op, op)
				return nil
			}
			opSeen, need.Op, need.OpSafe = true, op, opSafe

			if preservesEverySeries[agg.Op] {
				need.All = true
				return nil
			}
			if agg.Without {
				// `without (pod)` keeps every OTHER label, so everything but
				// the named ones is required -- which, without knowing the
				// full label set here, is every label. That is jetsam's
				// requirement, though, not the consumer's: the consumer
				// explicitly does NOT want pod, so "the consumer needs every
				// label" would be false of it -- record why every label
				// must survive instead of asserting that.
				need.All = true
				need.Blocker = "a `without` clause keeps every label except the ones it names, and the full label set of this metric is not known here to work out what that leaves, so every label must survive"
				return nil
			}
			if reason, unsafe := joinBelowAggregate(path, aggIdx); unsafe {
				need.All = true
				need.Blocker = reason
				return nil
			}
			fn, window, unsafe, reason := rangeFn(path, aggIdx)
			if unsafe {
				need.All = true
				need.Blocker = reason
				return nil
			}
			if fnSeen && (fn != need.Fn || window != need.Window) {
				// Same trap as the operator check above, one level down:
				// this metric appears twice under this aggregate's reach
				// with two different range claims (`rate(m[5m]) /
				// rate(m[10m])`, or one side rated and the other read
				// instantly) and Fn/Window can only carry one of them.
				need.All = true
				need.Blocker = fmt.Sprintf("this query applies more than one range claim to it: %s and %s",
					describeFn(need.Fn, need.Window), describeFn(fn, window))
				return nil
			}
			fnSeen, need.Fn, need.Window = true, fn, window
			for _, g := range agg.Grouping {
				req[g] = true
			}

		case *parser.BinaryExpr:
			if v.VectorMatching == nil {
				return nil
			}
			if v.VectorMatching.On {
				// on (x): matching happens ON x, so x must survive.
				for _, l := range v.VectorMatching.MatchingLabels {
					req[l] = true
				}
			} else if len(v.VectorMatching.MatchingLabels) > 0 {
				// ignoring (x): matching happens on every label EXCEPT x, so
				// x is the one label that need not agree -- a complement,
				// same as `without`, which LabelNeed cannot express as a
				// set. Refuse rather than under-report. As with `without`,
				// this is jetsam's requirement, not a true claim that the
				// consumer needs x too.
				need.All = true
				need.Blocker = "an `ignoring` clause matches on every label except the ones it names, and the full label set of this metric is not known here to work out what that leaves, so every label must survive"
			}
			// group_left/right(x) COPIES x from the many-side onto the
			// result, unlike MatchingLabels which only says what must
			// agree. Include is never a complement, on or ignoring alike,
			// so it is always required.
			for _, l := range v.VectorMatching.Include {
				req[l] = true
			}

		case *parser.Call:
			// label_replace and label_join name their source labels as
			// string ARGUMENTS. A walk that only inspects selectors and
			// groupings never sees them.
			switch v.Func.Name {
			case "label_replace":
				if len(v.Args) >= 4 {
					if s, ok := v.Args[3].(*parser.StringLiteral); ok {
						req[s.Val] = true
					}
				}
			case "label_join":
				for i := 3; i < len(v.Args); i++ {
					if s, ok := v.Args[i].(*parser.StringLiteral); ok {
						req[s.Val] = true
					}
				}
			}
		}
		return nil
	})

	if !need.Touches {
		return LabelNeed{}, nil
	}
	if need.All {
		return need, nil
	}
	var out []string
	for l := range req {
		out = append(out, l)
	}
	sort.Strings(out)
	need.Required = out
	return need, nil
}

// selectorTouches reports whether this selector can select metric.
func selectorTouches(v *parser.VectorSelector, metric string) bool {
	if v.Name == metric {
		return true
	}
	for _, m := range v.LabelMatchers {
		if m.Name == "__name__" && m.Matches(metric) {
			return true
		}
	}
	return false
}

// enclosingAggregate returns the nearest AggregateExpr above this node, and
// its index in path, or nil when the selector is not aggregated at all.
func enclosingAggregate(path []parser.Node) (*parser.AggregateExpr, int) {
	for i := len(path) - 1; i >= 0; i-- {
		if a, ok := path[i].(*parser.AggregateExpr); ok {
			return a, i
		}
	}
	return nil, -1
}

// rangeFn inspects the path between a selector and its enclosing aggregate
// (path[aggIdx]) and reports the range function applied over it, if any.
//
// For `sum by (path) (rate(m[5m]))` the path from the aggregate down to the
// selector is [*AggregateExpr, *Call, *MatrixSelector], so the shape to look
// for is: the selector's immediate parent is a *parser.MatrixSelector and
// its parent is a *parser.Call naming a recordable function.
//
// unsafe is true, with reason explaining why, whenever this cannot be
// answered safely:
//
//   - a *parser.SubqueryExpr anywhere between the selector and the
//     aggregate (as in `rate(m[5m:1m])`) evaluates the selector over a
//     sliding window before rate ever sees it, which this package does not
//     attempt to reason about;
//   - a matrix selector under any function outside recordableRangeFns --
//     max_over_time, avg_over_time, or any other -- does not commute with
//     pre-aggregation the way rate, irate and increase do;
//   - an INSTANT function applied directly to the selector -- abs(m),
//     clamp_min(m, 0) -- transforms the value before aggregation ever sees
//     it, and sum(abs(x)) is not abs(sum(x)) any more than sum(rate(x)) is
//     rate(sum(x)). This is checked by the selector's IMMEDIATE parent
//     alone, so a function nested under a range function is unaffected:
//     `label_replace(rate(m[5m]))` has a *MatrixSelector between the
//     selector and label_replace's Call, and that shape hits the case
//     above instead.
func rangeFn(path []parser.Node, aggIdx int) (fn string, window time.Duration, unsafe bool, reason string) {
	for i := aggIdx + 1; i < len(path); i++ {
		if _, ok := path[i].(*parser.SubqueryExpr); ok {
			return "", 0, true, "a subquery evaluates it over a sliding window before any range function sees it"
		}
	}
	if len(path) == 0 {
		return "", 0, false, ""
	}
	switch parent := path[len(path)-1].(type) {
	case *parser.MatrixSelector:
		if len(path) < 2 {
			return "", 0, true, "a matrix selector with nothing recognisable applying it"
		}
		call, ok := path[len(path)-2].(*parser.Call)
		if !ok {
			return "", 0, true, "a matrix selector with nothing recognisable applying it"
		}
		if !recordableRangeFns[call.Func.Name] {
			return "", 0, true, fmt.Sprintf("%s does not commute with pre-aggregation the way rate, irate and increase do", call.Func.Name)
		}
		return call.Func.Name, parent.Range, false, ""
	case *parser.Call:
		return "", 0, true, fmt.Sprintf("%s(...) is applied to it before aggregation, and does not commute with pre-aggregation", parent.Func.Name)
	default:
		// Nothing wraps the selector at all beyond the aggregate itself:
		// the aggregation reads its instant vector directly, which is Fn's
		// zero value.
		return "", 0, false, ""
	}
}

// joinBelowAggregate reports whether a vector-vector binary operation sits
// between the selector and its enclosing aggregate (path[aggIdx]).
//
// Position is what decides this. ABOVE the aggregate, a binary operation
// matches on labels the aggregation has already collapsed to, and the
// BinaryExpr case in LabelsNeeded's walk already accounts for what that
// requires. BELOW the aggregate the match happens against the RAW series --
// a bare `+` with no `on`/`ignoring` matches on every label by default, so
// both operands need every label just for the match to succeed, let alone
// for the result to be correct: collapsing one would not merely change the
// numbers, it would make every sample fail to match and the consumer return
// nothing at all. Reasoning about a specific on()/ignoring() clause at this
// position would have to account for how the aggregation groups relative to
// the join keys, which this package does not attempt -- refusing
// unconditionally is the safe, simple answer.
//
// A binary operation against a SCALAR (`m > 5`) has VectorMatching == nil:
// there is no matching, so it does not trigger this.
func joinBelowAggregate(path []parser.Node, aggIdx int) (reason string, unsafe bool) {
	for i := aggIdx + 1; i < len(path); i++ {
		if be, ok := path[i].(*parser.BinaryExpr); ok && be.VectorMatching != nil {
			return "it is matched against another vector before it is aggregated, so every label must survive for the match to succeed", true
		}
	}
	return "", false
}

// describeFn renders one selector's range-function claim for a refusal
// message naming a disagreement between two selectors of the same metric in
// one query -- see the fnSeen check in LabelsNeeded.
func describeFn(fn string, w time.Duration) string {
	if fn == "" {
		return "the metric itself"
	}
	return fn + " over " + window(w)
}
