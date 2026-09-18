package emit

import (
	"fmt"
	"sort"
	"strings"
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
// over the same metric with no extra label matcher and no offset or @
// modifier of any kind on the selector -- because the rule's own
// expression never carries one either. Preserving a modifier instead of
// refusing over it would be just as correct (the aggregate of a shifted
// series is the shifted aggregate), but it widens this function from
// "replace a subtree" to "replace a subtree and faithfully reconstruct
// every modifier it carried", and a modifier missed after that is silent
// in exactly the way a wrong rewrite is: it parses, it reads fine in a
// report, and it points at a number that answers a different question.
// Refusing is deliberately the narrower, honest answer for now.
//
// Positions come off the parsed AST, not a text search: PositionRange
// bounds exactly the aggregate subtree being replaced, so a metric name
// spelled inside a label value, a string literal, or a comment elsewhere in
// the same query is never touched.
//
// The second return value is a human-readable reason, non-empty exactly
// when the query contains an aggregate of p.Metric that was deliberately
// left alone -- as opposed to a query with no such aggregate in it at all,
// which returns query unchanged with no reason. The two look identical if
// only the rewritten string is inspected, and this repo already knows what
// that costs: labels.go's Blocker exists for the same reason, because a
// consumer that was silently skipped reads, to anyone building a report
// from the result, exactly like a consumer that was never a concern in the
// first place. A caller building that report needs to say "not rewritten,
// because its selector also matched job" rather than nothing at all.
//
// The rewritten result is re-parsed and refused, rather than returned, if
// it does not parse as PromQL, or if it does not gain exactly as many
// references to p.RuleName as subtrees were replaced -- the second check
// exists because a byte-for-byte substitution can produce text that parses
// into something else entirely (a bare number, say, or a name the query
// already happened to mention before the rewrite) without erroring, and
// that is exactly as wrong as not parsing.
//
// RewriteConsumer also refuses outright, before looking at query at all,
// when p itself is not internally consistent: an Op outside {sum, min,
// max}, or a RuleName that does not match what aggregate.RuleName would
// build from Metric/Keep/Op/Fn/Window. Both mean p could not have come out
// of Decide, the same discipline RenderRule applies to the same fields
// before it ever touches a rule file -- so that the two never quietly
// disagree about what the rule they both describe actually computes.
func RewriteConsumer(query string, p aggregate.Proposal) (rewritten string, declined string, err error) {
	if p.Op != "sum" && p.Op != "min" && p.Op != "max" {
		return "", "", fmt.Errorf("operator %q is not sum, min or max", p.Op)
	}
	if want := aggregate.RuleName(p.Metric, p.Keep, p.Op, p.Fn, p.Window); want != p.RuleName {
		return "", "", fmt.Errorf("record name %q does not match what Metric, Keep, Op, Fn and Window would name (%q)", p.RuleName, want)
	}

	expr, err := rewriteParser.ParseExpr(query)
	if err != nil {
		return "", "", fmt.Errorf("parse query: %w", err)
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
			return "", "", fmt.Errorf("proposal window %q does not parse as a PromQL range", p.Window)
		}
	}

	var spans []span
	var declines []string
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		agg, ok := n.(*parser.AggregateExpr)
		if !ok {
			return nil
		}
		matched, reason := matchAggregate(agg, p, win)
		switch {
		case matched:
			pr := agg.PositionRange()
			spans = append(spans, span{int(pr.Start), int(pr.End)})
		case reason != "":
			declines = append(declines, reason)
		}
		return nil
	})
	declined = strings.Join(declines, "; ")
	if len(spans) == 0 {
		return query, declined, nil
	}

	before := countSelectors(expr, p.RuleName)

	// Furthest-right first: replacing a span never changes the byte
	// offsets of anything entirely to its left, so working back-to-front
	// lets every span still recorded against the ORIGINAL query stay
	// valid right up to the moment it is used.
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out := query
	for _, s := range spans {
		out = out[:s.start] + p.RuleName + out[s.end:]
	}

	if err := validateRewrite(out, p.RuleName, before, len(spans)); err != nil {
		return "", "", err
	}
	return out, declined, nil
}

// validateRewrite re-parses out and refuses it, rather than letting it
// through, unless it is valid PromQL AND gained exactly wantNewRefs more
// selectors named ruleName than the original query had (before). Factored
// out from RewriteConsumer so both checks can be pinned directly, in
// rewrite_test.go, against inputs no legitimate Proposal would ever
// produce -- RewriteConsumer's own entry guards (Op, and RuleName's
// agreement with aggregate.RuleName) make both failure modes very hard to
// reach end-to-end once p is already internally consistent, which is a
// property of THIS package's callers today, not a reason for the checks
// themselves to be weaker or untested.
//
// The reference count, not merely whether ruleName appears somewhere in
// out, is what is checked: a query that already mentioned p.RuleName
// before RewriteConsumer ever touched it (see
// TestRewriteConsumerToleratesRuleNameAlreadyPresentElsewhere) would
// satisfy "appears somewhere" without the substitution having added
// anything at all, which is exactly the gap Minor 1 of this round's
// review closed.
func validateRewrite(out, ruleName string, before, wantNewRefs int) error {
	rewritten, err := rewriteParser.ParseExpr(out)
	if err != nil {
		return fmt.Errorf("rewritten query %q does not parse as PromQL: %w", out, err)
	}
	if after := countSelectors(rewritten, ruleName); after-before != wantNewRefs {
		return fmt.Errorf("rewritten query %q does not gain exactly %d reference(s) to %q (had %d, has %d)", out, wantNewRefs, ruleName, before, after)
	}
	return nil
}

// matchAggregate reports whether agg is genuinely the subtree p's
// recording rule was built from. When agg aggregates p.Metric in some
// recognisable shape but fails one of the checks RewriteConsumer requires,
// it returns false with a reason describing which -- because a query that
// touches the metric under a NEAR MISS (an extra label matcher, a
// different window, an offset) is not the same claim as a query that
// never touched it at all, and a caller reporting on this proposal's
// consumers needs to be able to tell the two apart. See RewriteConsumer's
// own doc comment for why refusing rather than guessing matters here.
func matchAggregate(agg *parser.AggregateExpr, p aggregate.Proposal, win time.Duration) (matched bool, reason string) {
	vs, fn, aggWin, isFn := innerSelector(agg.Expr)
	if vs == nil || vs.Name != p.Metric {
		// Nothing here reads p.Metric at all -- not a near miss, just an
		// unrelated aggregate this proposal has no opinion about.
		return false, ""
	}

	var reasons []string
	if agg.Without {
		reasons = append(reasons, "excludes labels with `without` rather than keeping them with `by`, which reads a different, complementary set of series")
	}
	op, opOK := aggregateOp(p.Op)
	if !opOK || agg.Op != op {
		reasons = append(reasons, fmt.Sprintf("aggregates with %s, the rule aggregates with %s", agg.Op, p.Op))
	}
	if !sameLabels(agg.Grouping, p.Keep) {
		reasons = append(reasons, fmt.Sprintf("groups by (%s), the rule groups by (%s)", strings.Join(sortedCopy(agg.Grouping), ", "), strings.Join(p.Keep, ", ")))
	}
	switch {
	case p.Fn == "" && isFn:
		reasons = append(reasons, fmt.Sprintf("applies %s over a range, the rule reads the metric directly", fn))
	case p.Fn != "" && !isFn:
		reasons = append(reasons, fmt.Sprintf("reads the metric directly, the rule applies %s", p.Fn))
	case p.Fn != "" && isFn && fn != p.Fn:
		reasons = append(reasons, fmt.Sprintf("applies %s, the rule applies %s", fn, p.Fn))
	case p.Fn != "" && isFn && aggWin != win:
		reasons = append(reasons, fmt.Sprintf("uses a different window than the rule's %s", p.Window))
	}
	if label := extraMatcher(vs); label != "" {
		reasons = append(reasons, fmt.Sprintf("selects with an additional label matcher on %q, which the rule's own expression never carries", label))
	}
	if hasOffsetOrAt(vs) {
		reasons = append(reasons, "reads the metric with an offset or an @ modifier, which selects different samples than the rule computes")
	}

	if len(reasons) > 0 {
		return false, strings.Join(reasons, "; ")
	}
	return true, ""
}

// innerSelector extracts the VectorSelector at the heart of e, regardless
// of whether e is a bare selector or a range function wrapped around a
// matrix selector, and reports the function name and window it found (fn
// is "" and isFn is false for the bare case). It exists so matchAggregate
// can recognise "this aggregate touches p.Metric" for the purpose of
// naming a near miss, independent of which shape p itself uses -- a
// consumer applying rate when the proposal has no Fn at all is exactly as
// much a near miss as one applying the wrong window.
func innerSelector(e parser.Expr) (vs *parser.VectorSelector, fn string, win time.Duration, isFn bool) {
	switch v := e.(type) {
	case *parser.VectorSelector:
		return v, "", 0, false
	case *parser.Call:
		if len(v.Args) != 1 {
			return nil, "", 0, false
		}
		ms, ok := v.Args[0].(*parser.MatrixSelector)
		if !ok {
			return nil, "", 0, false
		}
		inner, ok := ms.VectorSelector.(*parser.VectorSelector)
		if !ok {
			return nil, "", 0, false
		}
		name := ""
		if v.Func != nil {
			name = v.Func.Name
		}
		return inner, name, ms.Range, true
	}
	return nil, "", 0, false
}

// extraMatcher returns the name of the first label matcher on vs beyond
// the implicit __name__ one, or "" when there is none. The rule's own
// expression (renderExpr in record.go) is always the bare metric name with
// no matcher at all, so any matcher here means the consumer is asking a
// narrower question than the rule answers -- aggregating one job's series
// is not the same number as aggregating every job's, even under an
// otherwise identical operator, grouping, function and window.
func extraMatcher(vs *parser.VectorSelector) string {
	for _, m := range vs.LabelMatchers {
		if m.Name != "__name__" {
			return m.Name
		}
	}
	return ""
}

// hasOffsetOrAt reports whether vs reads the metric shifted in time by an
// offset or an @ modifier, in any of the forms the parser can produce.
//
// Offset is deliberately NOT checked here: it is the value the query
// ENGINE computes at evaluation time from OriginalOffset, an @ modifier,
// and any enclosing subquery, and after a bare parser.ParseExpr -- which
// is all RewriteConsumer ever does -- it is always zero regardless of what
// the query text says. Checking it instead of OriginalOffset makes this
// guard pass on every offset consumer there is: `rate(m[5m] offset 1h)`
// parses with Offset == 0 and OriginalOffset == 1h, and a check against
// Offset alone would rewrite it into `path:m_total:sum_rate5m`, silently
// moving a live alert's window from an hour ago to now. OriginalOffsetExpr
// covers the same field's experimental duration-expression form
// (`offset (1h)`) the same way DurationExpr covers Duration elsewhere in
// this package; Timestamp and StartOrEnd are already parser-set and cover
// `@ 1700000000` and `@ start()/end()`.
func hasOffsetOrAt(vs *parser.VectorSelector) bool {
	return vs.OriginalOffset != 0 || vs.OriginalOffsetExpr != nil || vs.Timestamp != nil || vs.StartOrEnd != 0
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
	g := sortedCopy(grouping)
	k := sortedCopy(keep)
	for i := range g {
		if g[i] != k[i] {
			return false
		}
	}
	return true
}

// sortedCopy returns a sorted copy of ss, leaving the original untouched --
// both sameLabels and matchAggregate's own mismatch message need a sorted
// view of an AggregateExpr's Grouping without disturbing the AST node it
// came from.
func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// aggregateOp maps a Proposal.Op string to the parser token it must equal
// on the AST. RewriteConsumer has already refused any p.Op outside this
// set before matchAggregate ever runs, so the ok result here can never
// actually be false in that path; it is still returned, rather than
// assumed, so this function stays correct on its own if that changes.
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

// countSelectors returns the number of VectorSelector nodes in expr named
// exactly name. RewriteConsumer uses it on the query both before and after
// substitution, rather than asking only "does the result mention name
// anywhere" (Minor 1 of this round's review): a query that already
// mentioned p.RuleName before the rewrite -- for whatever reason -- would
// pass the weaker check without the rewrite having added anything at all.
func countSelectors(expr parser.Expr, name string) int {
	n := 0
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if vs, ok := node.(*parser.VectorSelector); ok && vs.Name == name {
			n++
		}
		return nil
	})
	return n
}
