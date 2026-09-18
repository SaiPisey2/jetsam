package emit

import (
	"fmt"
	"sort"
	"time"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/prometheus/prometheus/promql/parser"
)

// rewriteParser is RewriteConsumer's own parser instance, kept as one shared
// value the same way ruleParser is in record.go -- nothing here is per-call
// either.
var rewriteParser = parser.NewParser(parser.Options{})

// span is a byte range in the original query, carried as plain ints rather
// than posrange.PositionRange so the replace loop below can do ordinary
// string slicing without importing that package for a type it only ever
// unwraps.
type span struct{ start, end int }

// RewriteConsumer returns query with every aggregate subtree that is
// genuinely what p's recording rule computes replaced by a bare reference
// to p.RuleName, so the consumer reads the rule instead of recomputing it
// from the raw metric on every eval.
//
// The rule records the WHOLE aggregate expression, not the bare metric --
// see record.go's renderExpr, which builds "sum by (path) (rate(m[5m]))" as
// one unit -- so what gets replaced here is that same whole subtree, never
// just the metric name inside it. A consumer's surrounding expression (a
// comparison, an arithmetic operation, another aggregation wrapping this
// one) is left exactly as it was; only the matched AggregateExpr's own span
// is swapped out. Replacing the metric name alone would leave a
// "sum by (path) (rate(path:m_total:sum_rate5m[5m]))" behind -- a rate of a
// rate, over a series the rule never produced at that resolution -- and
// replacing the whole expression whenever it merely CONTAINS a match would
// silently drop a surrounding "> 100" on a real alerting rule.
//
// Matching is conservative on purpose: the same operator, the same
// grouping labels (by, not without -- a without's complement is a
// different set of series entirely), the same range function and window,
// over the same metric with no extra label matcher, offset, or @ modifier
// on the selector -- because the rule's own expression never carries one
// either. Any consumer whose aggregate differs from the proposal in any of
// these ways is left untouched rather than guessed at: a wrong rewrite
// points a live alert at a series that does not mean what it did.
//
// Positions come off the parsed AST, not a text search: PositionRange
// bounds exactly the aggregate subtree being replaced, so a metric name
// spelled inside a label value, a string literal, or a comment elsewhere in
// the same query is never touched.
//
// The result is re-parsed and refused, rather than returned, if it does
// not parse as PromQL or no longer references p.RuleName at all -- the
// second check exists because a byte-for-byte substitution can produce
// text that happens to parse into something else entirely (a bare number,
// say) without erroring, and that is exactly as wrong as not parsing.
func RewriteConsumer(query string, p aggregate.Proposal) (string, error) {
	expr, err := rewriteParser.ParseExpr(query)
	if err != nil {
		return "", fmt.Errorf("parse query: %w", err)
	}

	// p.Window is only meaningful, and only guaranteed to parse as a
	// PromQL range, when p.Fn is set -- see Proposal's own doc comment.
	// Parsing it once here, through the same parser that will later read
	// every candidate's own window, means the two are compared as the
	// durations they mean rather than as strings that must happen to be
	// spelled identically ("1h" vs "60m").
	var win time.Duration
	if p.Fn != "" {
		var ok bool
		win, ok = windowDuration(p.Window)
		if !ok {
			return "", fmt.Errorf("proposal window %q does not parse as a PromQL range", p.Window)
		}
	}

	var spans []span
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		agg, ok := n.(*parser.AggregateExpr)
		if !ok {
			return nil
		}
		if aggregateMatches(agg, p, win) {
			pr := agg.PositionRange()
			spans = append(spans, span{int(pr.Start), int(pr.End)})
		}
		return nil
	})
	if len(spans) == 0 {
		return query, nil
	}

	// Furthest-right first: replacing a span never changes the byte
	// offsets of anything entirely to its left, so working back-to-front
	// lets every span still recorded against the ORIGINAL query stay
	// valid right up to the moment it is used.
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out := query
	for _, s := range spans {
		out = out[:s.start] + p.RuleName + out[s.end:]
	}

	rewritten, err := rewriteParser.ParseExpr(out)
	if err != nil {
		return "", fmt.Errorf("rewritten query %q does not parse as PromQL: %w", out, err)
	}
	if !referencesMetric(rewritten, p.RuleName) {
		return "", fmt.Errorf("rewritten query %q no longer references %q", out, p.RuleName)
	}
	return out, nil
}

// aggregateMatches reports whether agg is genuinely the subtree p's
// recording rule was built from -- see RewriteConsumer's doc comment for
// why every one of these has to hold rather than most of them.
func aggregateMatches(agg *parser.AggregateExpr, p aggregate.Proposal, win time.Duration) bool {
	if agg.Without {
		return false
	}
	op, ok := aggregateOp(p.Op)
	if !ok || agg.Op != op {
		return false
	}
	if !sameLabels(agg.Grouping, p.Keep) {
		return false
	}
	if p.Fn == "" {
		vs, ok := agg.Expr.(*parser.VectorSelector)
		return ok && isBareMetric(vs, p.Metric)
	}
	call, ok := agg.Expr.(*parser.Call)
	if !ok || call.Func == nil || call.Func.Name != p.Fn || len(call.Args) != 1 {
		return false
	}
	ms, ok := call.Args[0].(*parser.MatrixSelector)
	if !ok || ms.Range != win {
		return false
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	return ok && isBareMetric(vs, p.Metric)
}

// isBareMetric reports whether vs selects metric and nothing more: no
// matcher beyond the implicit __name__ one, no offset, no @ modifier. The
// rule's own expression (renderExpr in record.go) is always the bare
// metric name with none of these, so a selector carrying any of them is
// asking a narrower or different question than the rule answers --
// aggregating one job's series is not the same number as aggregating every
// job's, even under the identical operator and grouping.
func isBareMetric(vs *parser.VectorSelector, metric string) bool {
	if vs == nil || vs.Name != metric {
		return false
	}
	for _, m := range vs.LabelMatchers {
		if m.Name != "__name__" {
			return false
		}
	}
	return vs.Offset == 0 && vs.Timestamp == nil && vs.StartOrEnd == 0
}

// sameLabels reports whether grouping and keep name the same set of
// labels, order aside -- an AggregateExpr's Grouping is written in
// whatever order the query author typed it, and Proposal.Keep is sorted,
// so comparing them positionally without sorting a copy of grouping first
// would reject a consumer that matches in every way that matters.
func sameLabels(grouping, keep []string) bool {
	if len(grouping) != len(keep) {
		return false
	}
	g := append([]string(nil), grouping...)
	sort.Strings(g)
	k := append([]string(nil), keep...)
	sort.Strings(k)
	for i := range g {
		if g[i] != k[i] {
			return false
		}
	}
	return true
}

// aggregateOp maps a Proposal.Op string to the parser token it must equal
// on the AST -- the same three operators decide.go ever proposes (see
// decide.refuse, which rejects anything else before a Proposal exists at
// all).
func aggregateOp(op string) (parser.ItemType, bool) {
	switch op {
	case "sum":
		return parser.SUM, true
	case "min":
		return parser.MIN, true
	case "max":
		return parser.MAX, true
	}
	return 0, false
}

// windowDuration parses window (e.g. "5m") as a PromQL range the same way
// the parser itself would read it inside a matrix selector, rather than
// re-implementing Prometheus's duration grammar here just to compare two
// durations for equality.
func windowDuration(window string) (time.Duration, bool) {
	expr, err := rewriteParser.ParseExpr("x[" + window + "]")
	if err != nil {
		return 0, false
	}
	ms, ok := expr.(*parser.MatrixSelector)
	if !ok {
		return 0, false
	}
	return ms.Range, true
}

// referencesMetric reports whether expr contains a selector named exactly
// name -- the last check RewriteConsumer runs, because "parses" and
// "reads the rule it was supposed to" are two different claims and a
// substitution passing the first has not necessarily earned the second.
func referencesMetric(expr parser.Expr, name string) bool {
	found := false
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		if found {
			return nil
		}
		if vs, ok := n.(*parser.VectorSelector); ok && vs.Name == name {
			found = true
		}
		return nil
	})
	return found
}
