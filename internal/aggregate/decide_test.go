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
}

func TestAMetricNeedingEveryLabelIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used:  map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{"m_total": {All: true}},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 {
		t.Fatalf("want a refusal, got %v", refs)
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

// The brief only checks dashboardConsumers, but a logged ad-hoc query is
// exactly as unrewritable as a dashboard panel: Decide must also refuse on
// corpus.FromLog in UsedBy, even when no dashboard names the metric.
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
