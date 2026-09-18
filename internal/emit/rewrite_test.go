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
	got, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = "path:m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
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
	got, err := RewriteConsumer("sum by (path) (rate(m_total[5m])) > 100", p)
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
// unrelated query; the parsed AST never confuses the two.
func TestRewriteConsumerLeavesALabelValueAlone(t *testing.T) {
	p := fixtureProposal()
	const query = `sum by (path) (rate(other_total{kind="m_total"}[5m]))`
	got, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
}

// TestRewriteConsumerIsIdempotent asks RewriteConsumer to rewrite its own
// prior output. The rewritten text is a bare selector, not an aggregate
// over m_total at all, so a second pass must find nothing left to match
// and return it byte-for-byte.
func TestRewriteConsumerIsIdempotent(t *testing.T) {
	p := fixtureProposal()
	once, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("first RewriteConsumer: %v", err)
	}
	twice, err := RewriteConsumer(once, p)
	if err != nil {
		t.Fatalf("second RewriteConsumer: %v", err)
	}
	if twice != once {
		t.Fatalf("not idempotent: first %q, second %q", once, twice)
	}
}

// TestRewriteConsumerRefusesAnUnparseableResult plants a RuleName that is a
// perfectly ordinary string -- nothing here checks RuleName's own validity,
// that is RenderRule's job -- but breaks the PromQL grammar the moment it
// lands inside the query text this substitution produces. RewriteConsumer
// must refuse rather than hand back text nothing downstream can parse.
func TestRewriteConsumerRefusesAnUnparseableResult(t *testing.T) {
	p := fixtureProposal()
	p.RuleName = `m"injected`
	if _, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p); err == nil {
		t.Fatal("RewriteConsumer accepted a rule name that breaks the rewritten query, want an error")
	}
}

// TestRewriteConsumerRefusesAResultThatDoesNotReferenceTheRule covers the
// other half of the same guard: a RuleName that happens to be valid PromQL
// syntax but does not parse back into a selector naming itself -- a bare
// number, here -- so the rewritten text parses fine yet no longer reads the
// rule at all.
func TestRewriteConsumerRefusesAResultThatDoesNotReferenceTheRule(t *testing.T) {
	p := fixtureProposal()
	p.RuleName = "123"
	if _, err := RewriteConsumer("sum by (path) (rate(m_total[5m]))", p); err == nil {
		t.Fatal("RewriteConsumer accepted a rule name that does not read back as itself, want an error")
	}
}

// TestRewriteConsumerLeavesDifferingGroupingAlone covers a consumer that
// aggregates the same metric under the same operator and range, but by a
// different label than the proposal keeps. Rewriting it anyway would point
// it at a rule computed over the WRONG grouping -- a number that looks
// plausible and is not the one this consumer asked for.
func TestRewriteConsumerLeavesDifferingGroupingAlone(t *testing.T) {
	p := fixtureProposal()
	const query = "sum by (status) (rate(m_total[5m]))"
	got, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
}

// TestRewriteConsumerLeavesWithoutAlone covers a `without` consumer: it
// keeps every label except the ones named, the complement of `by`, and is
// never the same set of series a `by (path)` rule recorded even when the
// named label happens to be "path" too.
func TestRewriteConsumerLeavesWithoutAlone(t *testing.T) {
	p := fixtureProposal()
	const query = "sum without (path) (rate(m_total[5m]))"
	got, err := RewriteConsumer(query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if got != query {
		t.Fatalf("got %q, want unchanged %q", got, query)
	}
}

// TestRewriteConsumerRewritesEveryMatchingOccurrence covers a query where
// the same aggregate appears twice -- both occurrences read the metric the
// rule now carries, so both must be rewritten, not just whichever one a
// naive first-match replace would find.
func TestRewriteConsumerRewritesEveryMatchingOccurrence(t *testing.T) {
	p := fixtureProposal()
	got, err := RewriteConsumer(
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
	got, err := RewriteConsumer("sum(rate(m_total[5m]))", p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	const want = ":m_total:sum_rate5m"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
