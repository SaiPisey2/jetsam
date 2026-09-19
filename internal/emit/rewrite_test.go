package emit

import (
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
)

// fixtureProposal is the same Metric/Keep/Op/Fn/Window/RuleName shape used
// throughout record_test.go for the canonical "collapse m_total to path"
// case, so a consumer written against it is exactly what the rendered rule
// (RenderRule, same p) actually computes -- see RewriteConsumer's own doc
// comment for why the two must agree.
func fixtureProposal() aggregate.Proposal {
	return aggregate.Proposal{
		Metric:   "m_total",
		Keep:     []string{"path"},
		Op:       "sum",
		Fn:       "rate",
		Window:   "5m",
		RuleName: "path:m_total:sum_rate5m",
	}
}

func TestRewriteConsumerRewritesTheFixtureConsumer(t *testing.T) {
	p := fixtureProposal()
	got, declined, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if declined != "" {
		t.Fatalf("declined = %q, want none", declined)
	}
}

// TestRewriteConsumerKeepsTheSurroundingComparison is the alerting-rule
// case: the aggregate is only part of the expression, and the ">100" that
// decides whether the rule fires must survive the rewrite exactly as it
// was. A rewrite that replaced the whole expression, rather than only the
// matched AggregateExpr subtree, would pass every other test here and
// still silently turn a real alert into one that always compares the raw
// rule value against nothing.
func TestRewriteConsumerKeepsTheSurroundingComparison(t *testing.T) {
	p := fixtureProposal()
	got, _, err := RewriteConsumer("sum by (path) (rate(m_total[5m])) > 100", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m > 100"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRewriteConsumerLeavesALabelValueAlone covers a metric name that
// merely appears as a label's VALUE, never as a selector naming that
// metric -- a string search for "m_total" would find it and corrupt an
// unrelated query; the parsed AST never confuses the two. The aggregate
// here reads a DIFFERENT metric (other_total) entirely, so it is not a
// near miss either: declined must stay empty, the same as if m_total
// never appeared in the query at all.
func TestRewriteConsumerLeavesALabelValueAlone(t *testing.T) {
	p := fixtureProposal()
	const query = `sum by (path) (rate(other_total{kind="m_total"}[5m]))`
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
	if declined != "" {
		t.Fatalf("declined = %q, want none: this aggregate never touches m_total", declined)
	}
}

// TestRewriteConsumerIsIdempotent asks RewriteConsumer to rewrite its own
// prior output. The rewritten text is a bare selector, not an aggregate
// over m_total at all, so a second pass must find nothing left to match
// and return it byte-for-byte.
func TestRewriteConsumerIsIdempotent(t *testing.T) {
	p := fixtureProposal()
	once, _, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("first RewriteConsumer: %v", err)
	}
	twice, _, err := RewriteConsumer(once, p)
	if err != nil {
		t.Fatalf("second RewriteConsumer: %v", err)
	}
	if twice != once {
		t.Fatalf("not idempotent: first %q, second %q", once, twice)
	}
}

// TestRewriteConsumerRejectsAHostileOrDisagreeingRuleName covers every way
// a RuleName can be wrong: plain garbage, and a value that is well-formed
// PromQL on its own but does not match what Metric/Keep/Op/Fn/Window would
// actually name (Minor 2 of this round's review -- record.go's RenderRule
// applies the identical aggregate.RuleName agreement check before it ever
// writes a rule, and RewriteConsumer must refuse the same Proposal rather
// than build a report pointing a consumer at a rule RenderRule would
// refuse). Both are caught by the same upfront guard, before query is
// even parsed -- see TestValidateRewriteRefuses* below for the deeper
// substitution-safety checks that guard against a hostile MECHANISM could
// still slip within a name that agreed with itself.
func TestRewriteConsumerRejectsAHostileOrDisagreeingRuleName(t *testing.T) {
	cases := []struct {
		name     string
		ruleName string
	}{
		{"garbage", `path:m_total:sum_rate5m"injected`},
		{"disagrees with Keep", "status:m_total:sum_rate5m"},
		{"unrelated string", "123"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fixtureProposal()
			p.RuleName = c.ruleName
			if _, _, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p); err == nil {
				t.Fatalf("RewriteConsumer accepted RuleName %q, want an error", c.ruleName)
			}
		})
	}
}

// TestValidateRewriteRefusesUnparseableOutput pins validateRewrite's first
// check directly, with an input no Proposal that passed RewriteConsumer's
// own entry guards could realistically produce -- see validateRewrite's
// doc comment for why reaching this specific failure end-to-end is hard
// once RuleName must already agree with aggregate.RuleName.
func TestValidateRewriteRefusesUnparseableOutput(t *testing.T) {
	if err := validateRewrite(`m"injected`, `m"injected`, 0, 1); err == nil {
		t.Fatal("validateRewrite accepted output that does not parse, want an error")
	}
}

// TestValidateRewriteRefusesTheWrongReferenceCount pins the second check:
// "123" parses (as a NumberLiteral), so the first check alone would let it
// through, but it gains ZERO selectors named "123" even though one
// substitution was claimed -- the exact gap Minor 1 of this round's review
// closed, where the old check only asked whether the name appeared
// anywhere in the result.
func TestValidateRewriteRefusesTheWrongReferenceCount(t *testing.T) {
	if err := validateRewrite("123", "123", 0, 1); err == nil {
		t.Fatal("validateRewrite accepted a result with the wrong reference count, want an error")
	}
}

// TestValidateRewriteAcceptsAPreexistingReference is the positive half of
// the same check: a query that already mentioned ruleName once before the
// rewrite, and gains exactly one more, must be accepted -- the count is
// what matters, not merely whether ruleName appears "somewhere".
func TestValidateRewriteAcceptsAPreexistingReference(t *testing.T) {
	if err := validateRewrite("m_total + m_total", "m_total", 1, 1); err != nil {
		t.Fatalf("validateRewrite: %v", err)
	}
}

// TestRewriteConsumerLeavesDifferingGroupingAlone covers a consumer that
// aggregates the same metric under the same operator and range, but by a
// different label than the proposal keeps. Rewriting it anyway would point
// it at a rule computed over the WRONG grouping -- a number that looks
// plausible and is not the one this consumer asked for. It IS a near miss
// -- the aggregate genuinely reads m_total -- so it must come back with a
// reason, not silently alongside a query that never touched m_total at all.
func TestRewriteConsumerLeavesDifferingGroupingAlone(t *testing.T) {
	p := fixtureProposal()
	const query = "sum by (status) (rate(m_total[5m]))"
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
	if declined == "" {
		t.Fatal("declined is empty, want a reason: this aggregate does touch m_total")
	}
}

// TestRewriteConsumerLeavesWithoutAlone covers a `without` consumer: it
// keeps every label except the ones named, the complement of `by`, and is
// never the same set of series a `by (path)` rule recorded even when the
// named label happens to be "path" too. Also a near miss, so it must carry
// a reason.
func TestRewriteConsumerLeavesWithoutAlone(t *testing.T) {
	p := fixtureProposal()
	const query = "sum without (path) (rate(m_total[5m]))"
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
	if declined == "" {
		t.Fatal("declined is empty, want a reason: this aggregate does touch m_total")
	}
}

// TestRewriteConsumerRewritesEveryMatchingOccurrence covers a query where
// the same aggregate appears twice -- both occurrences read the metric the
// rule now carries, so both must be rewritten, not just whichever one a
// naive first-match replace would find.
func TestRewriteConsumerRewritesEveryMatchingOccurrence(t *testing.T) {
	p := fixtureProposal()
	got, _, err := RewriteConsumer(
		"sum by (path) (rate(m_total[5m])) + sum by (path) (rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m + path:m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Count(got, "rate(") != 0 {
		t.Fatalf("got %q, a raw aggregate survived the rewrite", got)
	}
}

// TestRewriteConsumerKeepEmptyRewritesCorrectly covers the case where
// every consumer collapses the metric to a single series: Keep is empty,
// and the rule name's leading colon (":m_total:sum_rate5m") is the
// conventional rendering of that empty level -- see decide.go's own
// comment on RuleName. The consumer side must match a bare `sum(...)` with
// no `by` clause at all, not a `by ()` that nothing ever writes.
func TestRewriteConsumerKeepEmptyRewritesCorrectly(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Op:       "sum",
		Fn:       "rate",
		Window:   "5m",
		RuleName: ":m_total:sum_rate5m",
	}
	got, _, err := RewriteConsumer("sum(rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = ":m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRewriteConsumerLeavesAnOffsetConsumerAloneUnderFn is the Critical
// fix from this round's review: parser.VectorSelector.Offset is what the
// query ENGINE computes at eval time and is always zero straight out of
// ParseExpr, regardless of what the query text says -- only
// OriginalOffset (and OriginalOffsetExpr, its experimental
// duration-expression form) records what the text actually asked for. A
// guard that checked Offset instead rejected nothing, and rewrote a
// consumer reading "an hour ago" into one reading "now": both queries
// parse, and the wrong one reads perfectly plausible in a report.
func TestRewriteConsumerLeavesAnOffsetConsumerAloneUnderFn(t *testing.T) {
	p := fixtureProposal()
	const query = "sum by (path) (rate(m_total[5m] offset 1h)) > 100"
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q -- an offset consumer must not be rewritten to read \"now\"", got, query)
	}
	if declined == "" {
		t.Fatal("declined is empty, want a reason naming the offset")
	}
}

// TestRewriteConsumerLeavesAnOffsetConsumerAloneWithoutFn covers the same
// Critical fix on the p.Fn == "" path, where the offset sits directly on
// the bare metric selector rather than inside a range function's matrix
// selector.
func TestRewriteConsumerLeavesAnOffsetConsumerAloneWithoutFn(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Keep:     []string{"path"},
		Op:       "sum",
		RuleName: "path:m_total:sum",
	}
	const query = "sum by (path) (m_total offset 1h)"
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
	if declined == "" {
		t.Fatal("declined is empty, want a reason naming the offset")
	}
}

// TestRewriteConsumerRewritesAnExplicitZeroOffset confirms the Critical
// fix checks OriginalOffset's VALUE, not merely whether an offset clause
// is present in the text: "offset 0s" carries a zero shift, exactly like
// no offset at all, and must still be rewritten.
func TestRewriteConsumerRewritesAnExplicitZeroOffset(t *testing.T) {
	p := fixtureProposal()
	got, _, err := RewriteConsumer("sum by (path) (rate(m_total[5m] offset 0s))", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRewriteConsumerLeavesNearMissesAlone pins the five match axes that,
// before this round, were each held down by no test at all: a single-line
// deletion of any one check in matchAggregate survived the whole suite.
// The first case is not actually a near miss (a different metric
// entirely) and must come back with no decline reason; the rest genuinely
// touch m_total and must each be declined with one.
func TestRewriteConsumerLeavesNearMissesAlone(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		wantDeclined bool
	}{
		{"different metric entirely", "sum by (path) (rate(other_metric[5m]))", false},
		{"extra label matcher", `sum by (path) (rate(m_total{job="x"}[5m]))`, true},
		{"different operator", "min by (path) (rate(m_total[5m]))", true},
		{"different window", "sum by (path) (rate(m_total[10m]))", true},
		{"different range function", "sum by (path) (irate(m_total[5m]))", true},
	}
	p := fixtureProposal()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, declined, err := RewriteConsumer(c.query, p)
			if err != nil {
				t.Fatalf("RewriteConsumer: %v", err)
			}
			if got != c.query {
				t.Fatalf("got %q, want unchanged %q", got, c.query)
			}
			if (declined != "") != c.wantDeclined {
				t.Fatalf("declined = %q, want empty=%v", declined, !c.wantDeclined)
			}
		})
	}
}

// TestRewriteConsumerDeclinesAnAggregateItCannotUnwrap is Important 2: an
// aggregate that reads p.Metric through a shape innerSelector cannot
// unwrap -- here, rate wrapped in label_replace -- must be declined with a
// reason, not silently reported as "no aggregate of this metric found
// here." Before this fix, vs == nil on its own was read as "unrelated",
// which is false: this query plainly reads m_total, jetsam just does not
// recognise the shape well enough to compare it against the rule.
func TestRewriteConsumerDeclinesAnAggregateItCannotUnwrap(t *testing.T) {
	p := fixtureProposal()
	const query = `sum by (path) (label_replace(rate(m_total[5m]), "x", "$1", "pod", "(.*)")) > 1`
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
	if declined == "" {
		t.Fatal("declined is empty, want a reason -- this query plainly aggregates m_total, jetsam just cannot unwrap the shape")
	}
	if !strings.Contains(declined, "shape jetsam does not recognise") {
		t.Errorf("declined = %q, want it to say the shape is unrecognised", declined)
	}
}

// TestRewriteConsumerToleratesRuleNameAlreadyPresentElsewhere covers Minor
// 1 from this round's review: the OLD check only asked whether the
// rewritten text mentioned p.RuleName ANYWHERE, which a query that already
// mentioned it before the rewrite would satisfy regardless of whether the
// substitution actually did anything. The count-based check must still
// accept this query -- one genuine reference before, two after, exactly
// one span replaced -- rather than treating the pre-existing mention as
// suspicious.
func TestRewriteConsumerToleratesRuleNameAlreadyPresentElsewhere(t *testing.T) {
	p := fixtureProposal()
	const query = "path:m_total:sum_rate5m + sum by (path) (rate(m_total[5m]))"
	got, _, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m + path:m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRewriteConsumerRejectsABadOp covers Minor 3: an unparseable Window
// already errors, but an Op outside {sum, min, max} used to fail open,
// returning the query unchanged with no error at all -- indistinguishable
// from "nothing here matched". Both malformations mean the same thing,
// that p could not have come out of Decide, and RenderRule already treats
// them the same way; RewriteConsumer must too.
func TestRewriteConsumerRejectsABadOp(t *testing.T) {
	p := fixtureProposal()
	p.Op = "avg"
	if _, _, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p); err == nil {
		t.Fatal("RewriteConsumer accepted an Op outside sum/min/max, want an error")
	}
}

// TestRewriteParserRejectsExperimentalModifiers pins what rewriteParser's
// zero-value Options{} buys this package: `anchored`, `smoothed`, and a
// duration-expression window are three more parser-set VectorSelector /
// MatrixSelector fields -- Anchored, Smoothed, and RangeExpr -- that
// hasOffsetOrAt does not check, exactly the shape of the offset defect
// this package already shipped once. They are safe today only because
// they fail to parse at all under the default Options, so matchAggregate
// never sees a selector carrying one. If rewriteParser is ever built with
// EnableExtendedRangeSelectors or ExperimentalDurationExpr, or a future
// Prometheus stabilises either without a flag, this test starts failing
// where the silent version of the bug would otherwise start -- matching
// TestExperimentalAggregatorsDoNotParse in
// internal/corpus/labels_test.go, which pins the same kind of gate for a
// different experimental feature.
func TestRewriteParserRejectsExperimentalModifiers(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"anchored", "rate(m_total[5m] anchored)"},
		{"smoothed", "rate(m_total[5m] smoothed)"},
		{"duration-expression window", "rate(m_total[(5+0)m])"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := rewriteParser.ParseExpr(c.query); err == nil {
				t.Fatalf("%q parsed; if this Prometheus enabled the feature, hasOffsetOrAt/MatrixSelector matching must account for it", c.query)
			}
		})
	}
}

// TestRewriteConsumerReportsBothARewriteAndADecline covers the one shape
// where a caller receives a successful rewrite AND a decline together: two
// aggregates of the same metric in one query, one matching the proposal
// and one that does not. A caller treating declined != "" as "nothing
// happened" would show the original query while jetsam actually rewrote
// part of it, so both return values must be pinned together rather than
// left to follow from RewriteConsumer's doc comment alone.
func TestRewriteConsumerReportsBothARewriteAndADecline(t *testing.T) {
	p := fixtureProposal()
	const query = "sum by (path) (rate(m_total[5m])) + min by (path) (rate(m_total[5m]))"
	got, declined, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m + min by (path) (rate(m_total[5m]))"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if declined == "" {
		t.Fatal("declined is empty, want a reason for the min aggregate that was left alone")
	}
}
