package corpus

import (
	"reflect"
	"testing"
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
			want:  LabelNeed{Required: []string{"path"}, Op: "sum", OpSafe: true},
		},
		{
			name:  "an empty grouping under sum requires nothing",
			query: `sum(rate(jetsam_demo_requests_total[5m]))`,
			want:  LabelNeed{Required: nil, Op: "sum", OpSafe: true},
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
			// The source label is a string argument, not a matcher. An AST
			// walk that only inspects selectors misses it entirely.
			name:  "label_replace's source label is required",
			query: `sum by (path) (label_replace(jetsam_demo_requests_total, "x", "$1", "pod", "(.*)"))`,
			want:  LabelNeed{Required: []string{"path", "pod"}, Op: "sum", OpSafe: true},
		},
		{
			name:  "a query that does not touch the metric needs nothing from it",
			query: `sum by (path) (some_other_total)`,
			want:  LabelNeed{Required: nil},
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
