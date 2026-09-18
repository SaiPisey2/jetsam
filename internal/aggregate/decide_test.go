package aggregate

import (
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
)

func inv(metric string, series int) inventory.Inventory {
	return inventory.Inventory{
		Metrics:     []inventory.Metric{{Name: metric, Series: series}},
		TotalSeries: series,
	}
}

func TestAMetricEveryConsumerAggregatesIsProposed(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{
			"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true},
		},
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(props) != 1 {
		t.Fatalf("got %d proposals and %d refusals, want 1 proposal: %v", len(props), len(refs), refs)
	}
	p := props[0]
	if p.Op != "sum" || len(p.Keep) != 1 || p.Keep[0] != "path" {
		t.Errorf("proposal = %+v, want sum over [path]", p)
	}
	if p.RuleName != "path:m_total:sum" {
		t.Errorf("RuleName = %q, want path:m_total:sum", p.RuleName)
	}
	if p.RawSeries != 400 {
		t.Errorf("RawSeries = %d, want 400 -- a later step quotes this to the person approving the collapse", p.RawSeries)
	}
}

func TestAMetricADashboardReadsIsRefusedByName(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, map[string][]string{"m_total": {"Node Exporter Full"}})
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "Node Exporter Full") {
		t.Fatalf("refusals = %v, want one naming the dashboard", refs)
	}
	if refs[0].Metric != "m_total" {
		t.Errorf("Metric = %q, want m_total", refs[0].Metric)
	}
}

// The fixture must be shaped so that only the All check can catch it: an
// Ops/AllOpsSafe pair that would ALSO refuse on its own (Ops: nil, or a
// disagreeing pair) lets this test pass even if the All check is deleted,
// because a later check catches the same fixture for a different reason.
func TestAMetricNeedingEveryLabelIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {All: true, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "every label") {
		t.Fatalf("refusals = %v, want one naming the every-label requirement", refs)
	}
}

func TestDisagreeingOperatorsAreRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"max", "sum"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "operator") {
		t.Fatalf("refusals = %v, want one naming the operator disagreement", refs)
	}
}

// {Ops: nil} means no consumer that aggregates this metric was ever seen --
// a different failure from disagreement, and the reason must not say
// "disagree" about a disagreement that does not exist: a reader who goes
// looking for it will conclude jetsam is wrong rather than that this is
// simply the input's most permissive shape.
func TestNoOperatorAtAllIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 {
		t.Fatalf("want a refusal, got %v", refs)
	}
	if strings.Contains(refs[0].Reason, "disagree") {
		t.Errorf("Reason = %q, must not claim a disagreement that does not exist", refs[0].Reason)
	}
	if !strings.Contains(refs[0].Reason, "operator") {
		t.Errorf("Reason = %q, want it to say there is no operator", refs[0].Reason)
	}
}

func TestANonComposingOperatorIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"avg"}, AllOpsSafe: false}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 {
		t.Fatalf("want a refusal, got %v", refs)
	}
}

// Blockers is documented on MetricNeed as "human-readable reasons
// aggregation is impossible" -- that contract holds regardless of All, so a
// non-empty Blockers must refuse even when All is false.
func TestBlockersRefuseEvenWhenAllIsFalse(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {
			Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true,
			Blockers: []string{"could not confirm which labels this query needs: up"},
		}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "could not confirm which labels this query needs: up") {
		t.Fatalf("refusals = %v, want one carrying the blocker text", refs)
	}
}

// The All branch folds Blockers into its own reason -- pinned separately
// from the All-is-false case above because deleting that fold leaves the
// suite green otherwise: TestAMetricNeedingEveryLabelIsRefused only checks
// for "every label", which the fold does not change.
func TestBlockersAreCarriedIntoTheEveryLabelRefusal(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {
			All: true, Blockers: []string{"could not parse a query that may touch it: {job=\"api\"} > 5"},
		}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "could not parse a query that may touch it") {
		t.Fatalf("refusals = %v, want one carrying the blocker text", refs)
	}
}

func TestRuleNameSortsItsLabels(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {Required: []string{"status", "path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	props, _ := Decide(inv("m_total", 400), c, nil)
	if len(props) != 1 || props[0].RuleName != "path_status:m_total:sum" {
		t.Fatalf("RuleName = %v, want path_status:m_total:sum -- stable between runs", props)
	}
}

// Checking dashboardConsumers alone would miss this: a logged ad-hoc query
// is exactly as unrewritable as a dashboard panel, so Decide must also
// refuse on corpus.FromLog in UsedBy, even when no dashboard names the
// metric.
func TestAMetricTheQueryLogReadIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:   map[string]bool{"m_total": true},
		UsedBy: map[string]corpus.Source{"m_total": corpus.FromRule | corpus.FromLog},
		Needs:  map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "query log") {
		t.Fatalf("refusals = %v, want one naming the query log", refs)
	}
}

// dashboardConsumers can be built independently of UsedBy (or omitted
// entirely by a caller that has not populated it yet), so the FromDashboard
// bit alone -- with no name to report -- must still refuse.
func TestAMetricUsedByDashboardWithNoNameIsStillRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:   map[string]bool{"m_total": true},
		UsedBy: map[string]corpus.Source{"m_total": corpus.FromDashboard},
		Needs:  map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, map[string][]string{})
	if len(refs) != 1 {
		t.Fatalf("want a refusal, got %v", refs)
	}
}

// A rule that would not parse forbids every drop in the verdict path
// (internal/verdict's "blocked"); this path claims more than a drop does,
// so it needs the same withholding, or worse. c.Blocked being non-empty at
// all -- regardless of which metric the unparseable rule named -- must
// refuse every metric, because an unparseable rule might reference any of
// them.
func TestABlockedCorpusWithholdsEveryProposal(t *testing.T) {
	c := corpus.Corpus{
		Used:    map[string]bool{"m_total": true},
		Needs:   map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		Blocked: []string{"rule g/Broken: parse query: unexpected character"},
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(props) != 0 || len(refs) != 1 {
		t.Fatalf("got %d proposals and %d refusals, want 0 and 1", len(props), len(refs))
	}
}

// Grafana configured and unreachable is the sharpest case: a metric a
// dashboard actually reads carries no FromDashboard bit in UsedBy and
// appears in no dashboardConsumers entry, because the fetch that would have
// populated both never completed. That state is indistinguishable, to every
// other check in this file, from "no dashboard reads it" -- so this gate
// must catch it before either silence gets read as permission.
func TestUnreachableGrafanaWithholdsEveryProposal(t *testing.T) {
	c := corpus.Corpus{
		Used:                 map[string]bool{"m_total": true},
		Needs:                map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		DashboardsConfigured: true,
		DashboardsReachable:  false,
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "Grafana") {
		t.Fatalf("refusals = %v, want one naming Grafana as unreachable", refs)
	}
}

// A query log that was configured but could not be read is negative
// evidence that never actually arrived -- exactly the case LogUnreadable
// exists to distinguish from "no log configured". A metric this path would
// otherwise propose must be withheld, because the log might have shown an
// ad-hoc read of it.
func TestAnUnreadableQueryLogWithholdsEveryProposal(t *testing.T) {
	c := corpus.Corpus{
		Used:          map[string]bool{"m_total": true},
		Needs:         map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		LogUnreadable: true,
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "query log") {
		t.Fatalf("refusals = %v, want one naming the unreadable query log", refs)
	}
}

// A recording rule's output has no raw series behind it to stop scraping --
// it is computed, not scraped. Proposing to collapse one would ask for a
// rule-file edit this package cannot make, over a drop with nothing to
// remove.
func TestARecordingRuleOutputIsNeverProposedForAggregation(t *testing.T) {
	c := corpus.Corpus{
		Used:     map[string]bool{"job:rec": true},
		Produced: map[string]bool{"job:rec": true},
		Needs:    map[string]corpus.MetricNeed{"job:rec": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	props, refs := Decide(inv("job:rec", 400), c, nil)
	if len(props) != 0 || len(refs) != 1 {
		t.Fatalf("got %d proposals and %d refusals, want 0 and 1", len(props), len(refs))
	}
}

// AllOpsSafe already rules this out for anything corpus.Build produces
// today, but this package does not get to assume that invariant holds
// forever in a package it does not own -- so a MetricNeed whose one
// operator is not sum, min or max must refuse even when AllOpsSafe claims
// otherwise.
func TestAnOperatorOutsideTheKnownSetIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"stddev"}, AllOpsSafe: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 {
		t.Fatalf("want a refusal, got %v", refs)
	}
}
