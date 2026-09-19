package corpus

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLabelsNeeded(t *testing.T) {
	const m = "jetsam_demo_requests_total"
	cases := []struct {
		name  string
		query string
		want  LabelNeed
	}{
		{
			name:  "by grouping requires exactly those labels",
			query: `sum by (path) (rate(jetsam_demo_requests_total[5m]))`,
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true, Fn: "rate", Window: 5 * time.Minute},
		},
		{
			name:  "an empty grouping under sum requires nothing",
			query: `sum(rate(jetsam_demo_requests_total[5m]))`,
			want:  LabelNeed{Required: nil, Op: "sum", OpSafe: true, Fn: "rate", Window: 5 * time.Minute},
		},
		{
			// topk returns its input series with every label intact. Its AST
			// is identical in shape to sum's -- AggregateExpr, empty
			// grouping -- so only the operator distinguishes them. Reading
			// the grouping alone would strip labels a dashboard displays.
			name:  "topk requires every label despite an empty grouping",
			query: `topk(5, jetsam_demo_requests_total)`,
			want:  LabelNeed{All: true, Op: "topk", OpSafe: false},
		},
		{
			name:  "bottomk likewise",
			query: `bottomk(3, jetsam_demo_requests_total)`,
			want:  LabelNeed{All: true, Op: "bottomk", OpSafe: false},
		},
		{
			name:  "a bare selector requires every label",
			query: `jetsam_demo_requests_total`,
			want:  LabelNeed{All: true},
		},
		{
			name:  "a selector with a matcher still requires every label, matcher included",
			query: `jetsam_demo_requests_total{status="500"}`,
			want:  LabelNeed{All: true},
		},
		{
			name:  "a matcher under an aggregation is required",
			query: `sum by (path) (jetsam_demo_requests_total{status="500"})`,
			want:  LabelNeed{Required: []string{"path", "status"}, Op: "sum", OpSafe: true},
		},
		{
			name:  "without keeps every label but the named ones",
			query: `sum without (pod) (jetsam_demo_requests_total)`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			name:  "max composes",
			query: `max by (path) (jetsam_demo_requests_total)`,
			want:  LabelNeed{Required: []string{"path"}, Op: "max", OpSafe: true},
		},
		{
			// count by (X) over series already collapsed to X+Y counts the
			// collapsed series, not the original ones. A different number.
			name:  "count does not compose",
			query: `count by (path) (jetsam_demo_requests_total)`,
			want:  LabelNeed{Required: []string{"path"}, Op: "count", OpSafe: false},
		},
		{
			name:  "avg does not compose",
			query: `avg by (path) (jetsam_demo_requests_total)`,
			want:  LabelNeed{Required: []string{"path"}, Op: "avg", OpSafe: false},
		},
		{
			name:  "vector matching labels are required",
			query: `sum by (path) (jetsam_demo_requests_total) / on (path) sum by (path) (other_total)`,
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true},
		},
		{
			name:  "group_left include labels are required",
			query: `sum by (path) (jetsam_demo_requests_total) * on (path) group_left (pod) other_info`,
			want:  LabelNeed{Required: []string{"path", "pod"}, Op: "sum", OpSafe: true},
		},
		{
			// label_replace applied directly to the selector is an instant
			// function sitting immediately above it, exactly like abs(m) --
			// sum(label_replace(x, ...)) is not guaranteed safe to
			// pre-aggregate any more than sum(abs(x)) is, so this refuses
			// rather than record a function-free requirement that ignores
			// label_replace entirely.
			name:  "label_replace directly over the selector refuses like any other instant function",
			query: `sum by (path) (label_replace(jetsam_demo_requests_total, "x", "$1", "pod", "(.*)"))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			// label_replace nested under rate now refuses -- it is no longer
			// the only thing between the selector and the aggregate. The span
			// is [Call(label_replace), Call(rate), MatrixSelector], three
			// nodes, not the one recognised shape of exactly [Call,
			// MatrixSelector]. jetsam does not reason about whether
			// label_replace itself commutes with the aggregation -- it only
			// recognises a bare rate, irate or increase as the entire span --
			// so this refuses, over-conservatively but safely, rather than
			// assume a function it has not examined is harmless.
			name:  "label_replace nested under rate now refuses",
			query: `sum by (path) (label_replace(rate(jetsam_demo_requests_total[5m]), "x", "$1", "pod", "(.*)"))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			// Redundant parentheses around a recordable range function must
			// not force a refusal: *parser.ParenExpr is skipped when the span
			// between the aggregate and the selector is built, so this reads
			// identically to the version without them.
			name:  "redundant parentheses around rate do not force a refusal",
			query: `sum by (path) ((rate(jetsam_demo_requests_total[5m])))`,
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true, Fn: "rate", Window: 5 * time.Minute},
		},
		{
			// clamp_max wrapped around rate -- the shape this Critical fix
			// exists for. Before the fix, rangeFn looked only at the
			// selector's immediate parent (rate's own MatrixSelector) and
			// never saw clamp_max sitting above it, so this wrongly proposed
			// sum_rate5m. sum(clamp_max(rate(x), 1)) is not
			// clamp_max(sum(rate(x)), 1).
			name:  "clamp_max wrapped around rate refuses",
			query: `sum by (path) (clamp_max(rate(jetsam_demo_requests_total[5m]), 1))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			name:  "round wrapped around rate refuses",
			query: `sum by (path) (round(rate(jetsam_demo_requests_total[5m])))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			name:  "abs wrapped around rate refuses",
			query: `sum by (path) (abs(rate(jetsam_demo_requests_total[5m])))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			// A scalar comparison BELOW the aggregate but ABOVE a rate:
			// sum(rate(x) > 5) sums only the series whose rate cleared 5, a
			// different number from sum(rate(x)) > 5. joinBelowAggregate
			// correctly leaves this alone (no VectorMatching, so no join),
			// but rangeFn must still refuse it: the span between the
			// aggregate and the selector is [BinaryExpr, Call(rate),
			// MatrixSelector], not the recognised two-node shape.
			name:  "a scalar comparison above rate but below the aggregate refuses",
			query: `sum by (path) (rate(jetsam_demo_requests_total[5m]) > 5)`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			// The same trap with no range function at all: sum(m > 5) sums
			// the filtered series, sum(m) > 5 filters the sum. Before this
			// fix, a BinaryExpr as the selector's immediate parent hit
			// rangeFn's "nothing wraps the selector" default case and was
			// wrongly read as safe, proposing plain "sum".
			name:  "a scalar comparison directly over the selector refuses",
			query: `sum by (path) (jetsam_demo_requests_total > 5)`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			name:  "a query that does not touch the metric needs nothing from it",
			query: `sum by (path) (some_other_total)`,
			want:  LabelNeed{Required: nil},
		},
		{
			// count_values invents a label from each sample's VALUE, and
			// that value is only stable over the raw series. Pre-collapsing
			// any other label at the source changes what gets merged and so
			// changes the count -- silently. A bare call has no grouping at
			// all, so before the fix it looked identical to sum()'s
			// empty-grouping case and wrongly reported needing nothing.
			name:  "count_values requires every label even with no grouping",
			query: `count_values("v", jetsam_demo_requests_total)`,
			want:  LabelNeed{All: true, Op: "count_values", OpSafe: false},
		},
		{
			// Before the fix this fell into the generic `by` path and
			// reported Required: [pod], looking like a legitimate finding
			// instead of an unanalysable one.
			name:  "count_values requires every label despite a by clause",
			query: `count_values("v", jetsam_demo_requests_total) by (pod)`,
			want:  LabelNeed{All: true, Op: "count_values", OpSafe: false},
		},
		{
			name:  "count_values requires every label under without too",
			query: `count_values("v", jetsam_demo_requests_total) without (pod)`,
			want:  LabelNeed{All: true, Op: "count_values", OpSafe: false},
		},
		{
			name:  "ignoring excludes the named label rather than requiring it",
			query: `sum by (path) (jetsam_demo_requests_total) / ignoring (pod) other_total`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			name:  "a plain aggregation over the instant vector records no function",
			query: `sum by (path) (jetsam_demo_requests_total)`,
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true, Fn: ""},
		},
		{
			name:  "irate is recorded like rate",
			query: `sum by (path) (irate(jetsam_demo_requests_total[5m]))`,
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true, Fn: "irate", Window: 5 * time.Minute},
		},
		{
			name:  "increase is recorded like rate",
			query: `sum by (path) (increase(jetsam_demo_requests_total[5m]))`,
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true, Fn: "increase", Window: 5 * time.Minute},
		},
		{
			// max_over_time under sum does not commute with pre-aggregation
			// at all: summing the maxima across pods is not the maximum of
			// the summed series. Refusing costs an optimisation nobody has
			// asked for; recording it would cost correctness.
			name:  "an unsupported range function over the metric refuses",
			query: `sum by (path) (max_over_time(jetsam_demo_requests_total[5m]))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			// A subquery evaluates the selector over a sliding window before
			// rate ever sees it -- a different computation from a plain
			// rate(m[5m]) that this package does not attempt to reason
			// about, so it refuses rather than mis-record it.
			name:  "a subquery beneath rate refuses",
			query: `sum(rate(jetsam_demo_requests_total[5m:1m]))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
		{
			// sum(abs(x)) is not abs(sum(x)): an instant function directly
			// over the selector transforms the value before aggregation
			// ever sees it, the same trap max_over_time is refused for
			// above. Before this fix, abs(m) fell into the same code path
			// as a bare m and silently recorded Fn="" -- "no function
			// involved" -- which is false.
			name:  "an instant function directly over the selector refuses",
			query: `sum by (path) (abs(jetsam_demo_requests_total))`,
			want:  LabelNeed{All: true, Op: "sum", OpSafe: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LabelsNeeded(tc.query, m)
			if err != nil {
				t.Fatalf("LabelsNeeded: %v", err)
			}
			if got.All != tc.want.All {
				t.Errorf("All = %v, want %v", got.All, tc.want.All)
			}
			if !got.All && !reflect.DeepEqual(got.Required, tc.want.Required) {
				t.Errorf("Required = %v, want %v", got.Required, tc.want.Required)
			}
			if got.Op != tc.want.Op {
				t.Errorf("Op = %q, want %q", got.Op, tc.want.Op)
			}
			if got.OpSafe != tc.want.OpSafe {
				t.Errorf("OpSafe = %v, want %v", got.OpSafe, tc.want.OpSafe)
			}
			if !got.All {
				if got.Fn != tc.want.Fn {
					t.Errorf("Fn = %q, want %q", got.Fn, tc.want.Fn)
				}
				if got.Window != tc.want.Window {
					t.Errorf("Window = %v, want %v", got.Window, tc.want.Window)
				}
			}
		})
	}
}

func TestLabelsNeededRefusesWhatItCannotParse(t *testing.T) {
	if _, err := LabelsNeeded(`this is not promql {{{`, "m"); err == nil {
		t.Fatal("want an error on an unparseable query -- a query jetsam cannot read must not look like one needing no labels")
	}
}

// limitk is experimental and does not parse at this Prometheus version, so it
// reaches the refusal path by way of the parser rather than by an operator
// check. Pin that, because enabling the feature later would change it.
func TestExperimentalAggregatorsDoNotParse(t *testing.T) {
	if _, err := LabelsNeeded(`limitk(5, jetsam_demo_requests_total)`, "jetsam_demo_requests_total"); err == nil {
		t.Fatal("limitk parsed; if this Prometheus gained the experimental feature, LabelsNeeded must classify it as All")
	}
}

// TestOneQueryMakingTwoClaimsAboutOneMetricRefuses covers the fourth instance
// of this package's founding trap: parser.Inspect visits EVERY selector of
// this metric in the query, but Op/OpSafe/Fn/Window are plain fields, so a
// single query that reads the metric two different ways used to report only
// the LAST claim visited -- and worse, the answer flipped with source order,
// which a fixed corpus.Build could never explain. Each case here reads the
// metric twice within ONE query under a shape that disagrees with itself.
func TestOneQueryMakingTwoClaimsAboutOneMetricRefuses(t *testing.T) {
	const m = "jetsam_demo_requests_total"
	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "two different operators",
			query: `sum by (path) (jetsam_demo_requests_total) / max by (path) (jetsam_demo_requests_total)`,
		},
		{
			name:  "two different windows, order one way",
			query: `sum by (path) (rate(jetsam_demo_requests_total[5m])) / sum by (path) (rate(jetsam_demo_requests_total[10m]))`,
		},
		{
			name:  "two different windows, order reversed",
			query: `sum by (path) (rate(jetsam_demo_requests_total[10m])) / sum by (path) (rate(jetsam_demo_requests_total[5m]))`,
		},
		{
			name:  "one side rated, the other read instantly",
			query: `sum by (path) (rate(jetsam_demo_requests_total[5m])) / sum by (path) (jetsam_demo_requests_total)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LabelsNeeded(tc.query, m)
			if err != nil {
				t.Fatalf("LabelsNeeded: %v", err)
			}
			if !got.All {
				t.Fatalf("All = false, want true -- one LabelNeed cannot carry two disagreeing claims about the same metric (Fn=%q Window=%v Op=%q)",
					got.Fn, got.Window, got.Op)
			}
			if got.Blocker == "" {
				t.Error("Blocker is empty, want a reason naming the conflicting claims")
			}
		})
	}
}

// TestAJoinBelowTheAggregateRefusesButAboveItDoesNot is Critical B: a bare
// vector-vector join BELOW the aggregate matches on every label of the raw
// series by default, so collapsing this metric would not just change the
// numbers, it would make the join match nothing at all. The exact same
// binary-operator shape ABOVE the aggregate is a different, safe thing --
// it matches on labels the aggregation already collapsed to -- and must not
// be caught by this rule; it still refuses here, but for the window
// disagreement (TestOneQueryMakingTwoClaimsAboutOneMetricRefuses' own
// concern), not for a join. The Blocker text is what tells the two apart.
func TestAJoinBelowTheAggregateRefusesButAboveItDoesNot(t *testing.T) {
	const m = "jetsam_demo_requests_total"

	t.Run("a bare join below the aggregate refuses, naming the join", func(t *testing.T) {
		query := `sum by (path) (rate(jetsam_demo_requests_total[5m]) + rate(other_total[5m]))`
		got, err := LabelsNeeded(query, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if !got.All {
			t.Fatal("All = false, want true -- a bare `+` below the aggregate matches on every label of the raw series")
		}
		if !strings.Contains(got.Blocker, "matched against another vector") {
			t.Errorf("Blocker = %q, want it to name the join", got.Blocker)
		}
	})

	t.Run("the same binary operator above the aggregate is not a join-below refusal", func(t *testing.T) {
		query := `sum by (path) (rate(jetsam_demo_requests_total[5m])) / sum by (path) (rate(jetsam_demo_requests_total[10m]))`
		got, err := LabelsNeeded(query, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if !got.All {
			t.Fatal("All = false, want true -- the two sides disagree on the window")
		}
		if strings.Contains(got.Blocker, "matched against another vector") {
			t.Errorf("Blocker = %q -- the join-below-the-aggregate check fired for a binary operator ABOVE the aggregate, which is over-broad", got.Blocker)
		}
	})
}

// TestBlockerDistinguishesJetsamsRequirementFromTheConsumersOwn is the other
// half of what "some consumer needs every label" got wrong: `without` and
// `ignoring` both refuse because their COMPLEMENT can't be computed here --
// the consumer explicitly does NOT want the named label -- which is a
// different, false claim from "the consumer needs every label" (genuinely
// true of a bare selector, topk or count_values, which never set Blocker).
// Blocker is what a reader downstream needs to tell the two apart, so it
// must be set here exactly as it is for the refusals added earlier.
func TestBlockerDistinguishesJetsamsRequirementFromTheConsumersOwn(t *testing.T) {
	const m = "jetsam_demo_requests_total"

	t.Run("without sets a blocker naming the complement problem", func(t *testing.T) {
		got, err := LabelsNeeded(`sum without (pod) (jetsam_demo_requests_total)`, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if got.Blocker == "" {
			t.Error("Blocker is empty, want a reason -- `without` does not mean the consumer needs every label, it means jetsam cannot compute the complement")
		}
	})

	t.Run("ignoring sets a blocker naming the complement problem", func(t *testing.T) {
		got, err := LabelsNeeded(`sum by (path) (jetsam_demo_requests_total) / ignoring (pod) other_total`, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if got.Blocker == "" {
			t.Error("Blocker is empty, want a reason -- `ignoring` does not mean the consumer needs every label either")
		}
	})

	t.Run("a bare selector sets no blocker -- it genuinely needs every label", func(t *testing.T) {
		got, err := LabelsNeeded(`jetsam_demo_requests_total`, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if got.Blocker != "" {
			t.Errorf("Blocker = %q, want empty -- an unaggregated selector genuinely needs every label, so the generic reason is already true", got.Blocker)
		}
	})

	t.Run("topk sets no blocker -- it genuinely needs every label", func(t *testing.T) {
		got, err := LabelsNeeded(`topk(5, jetsam_demo_requests_total)`, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if got.Blocker != "" {
			t.Errorf("Blocker = %q, want empty -- topk hands back every series with every label, so the generic reason is already true", got.Blocker)
		}
	})

	t.Run("count_values sets no blocker -- it genuinely needs every label", func(t *testing.T) {
		got, err := LabelsNeeded(`count_values("v", jetsam_demo_requests_total)`, m)
		if err != nil {
			t.Fatalf("LabelsNeeded: %v", err)
		}
		if got.Blocker != "" {
			t.Errorf("Blocker = %q, want empty -- count_values needs the raw series' every label, so the generic reason is already true", got.Blocker)
		}
	})
}
