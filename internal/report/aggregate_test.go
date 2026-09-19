package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
)

// TestAggregateAlwaysSaysNothingIsApplied is the test the brief calls out
// by name: this is the one sentence standing between a useful report and a
// footgun, so it must appear on every run -- including the emptiest one,
// with nothing proposed and nothing refused. A caller who reads this and
// then hand-applies the rule, the rewrite, and a drop of their own has lost
// data a rule still reads; the report must not let that look like a clean
// bill of health.
func TestAggregateAlwaysSaysNothingIsApplied(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", nil, nil, nil)
	out := b.String()
	if !strings.Contains(out, "Nothing here is applied") {
		t.Fatalf("report does not say nothing is applied when there is nothing to report:\n%s", out)
	}
	if !strings.Contains(out, "drops") || !strings.Contains(out, "ingest") {
		t.Errorf("report does not explain that Prometheus drops at ingest, before rules run:\n%s", out)
	}
	if !strings.Contains(out, "upstream") {
		t.Errorf("report does not say the saving is only real upstream of Prometheus:\n%s", out)
	}
}

// TestAggregateNamesTheSourcesItRead is Important 1's second half: this
// command makes the strongest claim in the project -- "every consumer of
// this needs only these labels" -- and a reader cannot judge it without
// knowing which evidence sources actually contributed. The sentence sits
// right next to the not-applied notice, on every call, the same way that
// notice does.
func TestAggregateNamesTheSourcesItRead(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules, dashboards and the query log", nil, nil, nil)
	out := b.String()
	if !strings.Contains(out, "rules, dashboards and the query log") {
		t.Fatalf("report does not name which sources were read:\n%s", out)
	}
	notice := strings.Index(out, "Nothing here is applied")
	sources := strings.Index(out, "rules, dashboards and the query log")
	if notice < 0 || sources < 0 || sources < notice {
		t.Errorf("the sources line does not appear alongside the not-applied notice:\n%s", out)
	}
}

// TestAggregateSanitizesTheSourcesLine: sources is built from corpus
// state today, not remote input, but Aggregate is exported and every other
// field this function prints already goes through safe.Text -- a caller
// two packages away should not have to know this one is an exception.
func TestAggregateSanitizesTheSourcesLine(t *testing.T) {
	const hostile = "rules\x1b[2KFAKE"
	var b bytes.Buffer
	Aggregate(&b, hostile, nil, nil, nil)
	if strings.ContainsRune(b.String(), 0x1b) {
		t.Errorf("report contains a raw ESC byte:\n%q", b.String())
	}
}

// TestAggregateSaysNothingIsAppliedWhenEveryMetricIsRefused pins the exact
// failure mode the brief names: a run where every metric was refused must
// carry the same warning as one with findings, not silently drop it because
// there was "nothing" to report alongside it.
func TestAggregateSaysNothingIsAppliedWhenEveryMetricIsRefused(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", nil, nil, []aggregate.Refusal{{Metric: "m_total", Reason: "some consumer needs every label"}})
	out := b.String()
	if !strings.Contains(out, "Nothing here is applied") {
		t.Fatalf("report drops the not-applied notice when every metric was refused:\n%s", out)
	}
}

// TestAggregateListsRefusalsWithReasons mirrors how propose lists blocked
// inputs: a refusal is worthless to a reader unless its reason is right
// there next to the metric it names.
func TestAggregateListsRefusalsWithReasons(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", nil, nil, []aggregate.Refusal{
		{Metric: "http_requests_total", Reason: "read by a dashboard jetsam cannot rewrite"},
	})
	out := b.String()
	if !strings.Contains(out, "http_requests_total") || !strings.Contains(out, "read by a dashboard jetsam cannot rewrite") {
		t.Errorf("report does not name the refused metric with its reason:\n%s", out)
	}
}

func sampleFinding() AggregateFinding {
	return AggregateFinding{
		Proposal: aggregate.Proposal{
			Metric:     "http_requests_total",
			Keep:       []string{"path"},
			Op:         "sum",
			RawSeries:  400,
			KeptSeries: 12,
			RuleName:   "path:http_requests_total:sum",
		},
		Rule: "groups:\n    - name: jetsam\n      rules:\n        - record: path:http_requests_total:sum\n          expr: sum by (path) (http_requests_total)\n",
		Consumers: []ConsumerFinding{
			{
				Consumer:  aggregate.Consumer{Kind: "rule", Group: "g", Name: "r1", Query: "sum by (path) (http_requests_total) > 100"},
				Rewritten: "path:http_requests_total:sum > 100",
			},
			{
				Consumer:  aggregate.Consumer{Kind: "rule", Group: "g", Name: "r2", Query: `sum by (path) (http_requests_total{job="api"})`},
				Rewritten: `sum by (path) (http_requests_total{job="api"})`, // unchanged: RewriteConsumer left it as-is
				Declined:  `selects with an additional label matcher on "job", which the rule's own expression never carries`,
			},
		},
	}
}

// TestAggregateStatesTheSeriesCountsLabelsAndOperator pins the required
// content the brief lists for each metric: raw and kept series, the labels
// kept, the operator, and the rendered recording rule.
func TestAggregateStatesTheSeriesCountsLabelsAndOperator(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{sampleFinding()}, nil, nil)
	out := b.String()

	for _, want := range []string{"http_requests_total", "400", "12", "path", "sum", "path:http_requests_total:sum"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}
}

// TestAggregateShowsACaveatProminently pins the ruling that replaced the
// query-log refusal: a metric a logged query also read is still proposed,
// but the caveat naming that must appear right alongside the numbers --
// before the keep/drop/op detail -- not merely somewhere in the report,
// since a reader must not be able to act on the series counts without
// seeing it first.
func TestAggregateShowsACaveatProminently(t *testing.T) {
	f := sampleFinding()
	f.Proposal.Caveats = []string{"queried ad hoc within the query log's window: jetsam cannot rewrite a query it only saw in a log"}

	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{f}, nil, nil)
	out := b.String()

	if !strings.Contains(out, "query log's window") {
		t.Fatalf("report does not carry the caveat text:\n%s", out)
	}

	headline := strings.Index(out, "http_requests_total: 400 series")
	caveat := strings.Index(out, "query log's window")
	keeps := strings.Index(out, "keeps:")
	if headline < 0 || caveat < 0 || keeps < 0 || !(headline < caveat && caveat < keeps) {
		t.Errorf("caveat is not positioned between the headline numbers and the keep/drop detail:\n%s", out)
	}
}

// TestAggregateWithoutACaveatPrintsNone: a proposal with no caveats must
// not gain a spurious one.
func TestAggregateWithoutACaveatPrintsNone(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{sampleFinding()}, nil, nil)
	if strings.Contains(b.String(), "CAVEAT") {
		t.Errorf("report shows a caveat for a proposal that carries none:\n%s", b.String())
	}
}

// TestAggregateNamesWhatIsRemoved: a reader must not have to infer from the
// kept labels alone that everything else about the metric is gone.
func TestAggregateNamesWhatIsRemoved(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{sampleFinding()}, nil, nil)
	out := b.String()
	if !strings.Contains(out, "drop") {
		t.Errorf("report does not say which labels are removed:\n%s", out)
	}
}

// TestAggregateShowsARewrittenConsumerBeforeAndAfter.
func TestAggregateShowsARewrittenConsumerBeforeAndAfter(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{sampleFinding()}, nil, nil)
	out := b.String()
	if !strings.Contains(out, "sum by (path) (http_requests_total) > 100") {
		t.Errorf("report does not show the consumer's original query:\n%s", out)
	}
	if !strings.Contains(out, "path:http_requests_total:sum > 100") {
		t.Errorf("report does not show the consumer's rewritten query:\n%s", out)
	}
}

// TestAggregateReportsADeclinedConsumerRatherThanOmittingIt is the test
// pinning the brief's sharpest warning: a consumer RewriteConsumer declines
// to rewrite must be reported as not rewritable, never silently dropped
// from the list -- that silence is exactly how someone concludes an
// aggregation is safe when it is not.
func TestAggregateReportsADeclinedConsumerRatherThanOmittingIt(t *testing.T) {
	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{sampleFinding()}, nil, nil)
	out := b.String()
	if !strings.Contains(out, "r2") {
		t.Fatalf("report omits the declined consumer entirely:\n%s", out)
	}
	if !strings.Contains(out, "not rewritable") {
		t.Errorf("report does not say the declined consumer is not rewritable:\n%s", out)
	}
	if !strings.Contains(out, `additional label matcher on "job"`) {
		t.Errorf("report does not carry the decline reason:\n%s", out)
	}
}

// TestAggregatePartiallyRewrittenConsumerShowsBothTheRewriteAndTheDecline
// pins the shape most likely to mislead a reader of this report:
// RewriteConsumer can rewrite one aggregate over a metric and decline a
// different aggregate over the SAME metric in the same expression --
// `sum by (path) (rate(m_total[5m])) + min by (path) (rate(m_total[5m]))`
// rewrites the sum and declines the min. Showing only "after" would tell a
// reader this consumer no longer needs the raw series, when the min half of
// it still does; the report must show the rewrite AND the decline together.
func TestAggregatePartiallyRewrittenConsumerShowsBothTheRewriteAndTheDecline(t *testing.T) {
	finding := AggregateFinding{
		Proposal: aggregate.Proposal{
			Metric: "m_total", Keep: []string{"path"}, Op: "sum",
			Fn: "rate", Window: "5m", RawSeries: 400, KeptSeries: 12,
			RuleName: "path:m_total:sum_rate5m",
		},
		Rule: "groups: []\n",
		Consumers: []ConsumerFinding{{
			Consumer: aggregate.Consumer{
				Kind: "rule", Group: "g", Name: "Mixed",
				Query: "sum by (path) (rate(m_total[5m])) + min by (path) (rate(m_total[5m]))",
			},
			Rewritten: "path:m_total:sum_rate5m + min by (path) (rate(m_total[5m]))",
			Declined:  "aggregates with min, the rule aggregates with sum",
		}},
	}
	var b bytes.Buffer
	Aggregate(&b, "rules", []AggregateFinding{finding}, nil, nil)
	out := b.String()

	if !strings.Contains(out, "path:m_total:sum_rate5m + min by (path) (rate(m_total[5m]))") {
		t.Errorf("report does not show the partial rewrite:\n%s", out)
	}
	if !strings.Contains(out, "aggregates with min, the rule aggregates with sum") {
		t.Errorf("report does not carry the decline reason alongside the rewrite:\n%s", out)
	}
	if !strings.Contains(out, "partially") {
		t.Errorf("report does not flag this as a partial rewrite, which could mislead a reader into thinking the raw series is no longer needed:\n%s", out)
	}
}

// TestAggregateSanitizesRemoteSourcedText: metric and rule names come from
// a remote Prometheus, the same trust boundary report.Scan already treats
// as hostile.
func TestAggregateSanitizesRemoteSourcedText(t *testing.T) {
	const hostile = "m\x1b[2KFAKE"
	var b bytes.Buffer
	Aggregate(&b, "rules", nil, nil, []aggregate.Refusal{{Metric: hostile, Reason: "some consumer needs every label"}})
	out := b.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("report contains a raw ESC byte:\n%q", out)
	}
}
