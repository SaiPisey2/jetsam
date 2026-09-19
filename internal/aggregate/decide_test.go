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

// TestAMetricTheQueryLogReadIsProposedWithACaveat pins the ruling that
// replaced this test's old name (TestAMetricTheQueryLogReadIsRefused): a
// logged query is a historical event, not a standing consumer, so it must
// not cost the whole proposal the way a dashboard read does. Its label
// requirements are already in need's union (via corpus.Build), so Decide
// still proposes the metric -- it just carries a caveat naming the risk a
// human approving this needs to see: re-running that exact query later
// will not return what it did.
func TestAMetricTheQueryLogReadIsProposedWithACaveat(t *testing.T) {
	c := corpus.Corpus{
		Used:                 map[string]bool{"m_total": true},
		UsedBy:               map[string]corpus.Source{"m_total": corpus.FromRule | corpus.FromLog},
		Needs:                map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		DashboardsConfigured: true,
		DashboardsReachable:  true,
		LogRead:              true,
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 0 {
		t.Fatalf("refusals = %v, want none -- a logged read is a caveat, not a refusal", refs)
	}
	if len(props) != 1 {
		t.Fatalf("got %d proposals, want 1", len(props))
	}
	if len(props[0].Caveats) != 1 || !strings.Contains(props[0].Caveats[0], "log") {
		t.Fatalf("Caveats = %v, want one naming the query log", props[0].Caveats)
	}
}

// TestAMetricNeitherLoggedNorDashboardReadCarriesNoCaveat: the caveat must
// not appear on a proposal nothing logged ever touched -- with both
// evidence sources actually configured and read, so the two new
// evidence-never-consulted caveats (see the tests below) do not confound
// this one.
func TestAMetricNeitherLoggedNorDashboardReadCarriesNoCaveat(t *testing.T) {
	c := corpus.Corpus{
		Used:                 map[string]bool{"m_total": true},
		UsedBy:               map[string]corpus.Source{"m_total": corpus.FromRule},
		Needs:                map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		DashboardsConfigured: true,
		DashboardsReachable:  true,
		LogRead:              true,
	}
	props, _ := Decide(inv("m_total", 400), c, nil)
	if len(props) != 1 || len(props[0].Caveats) != 0 {
		t.Fatalf("props = %+v, want one proposal with no caveats", props)
	}
}

// TestAMetricIsCaveatedWhenGrafanaWasNeverConfigured is Important 1's first
// half: grafana.url unset and a metric a dashboard genuinely reads look
// IDENTICAL in the corpus -- neither carries a FromDashboard bit or a
// dashboardConsumers entry -- so refuse's own gate, which only catches
// Grafana configured-but-unreachable, cannot tell them apart. Decide must
// not stay silent about that gap: it proposes the metric (a hard refusal
// here would kill the only positive case an install with no dashboards
// configured has ever had) but says so.
func TestAMetricIsCaveatedWhenGrafanaWasNeverConfigured(t *testing.T) {
	c := corpus.Corpus{
		Used:    map[string]bool{"m_total": true},
		UsedBy:  map[string]corpus.Source{"m_total": corpus.FromRule},
		Needs:   map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		LogRead: true,
		// DashboardsConfigured left false: grafana.url was never set.
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 0 {
		t.Fatalf("refusals = %v, want none -- an unconfigured source caveats, it does not refuse", refs)
	}
	if len(props) != 1 {
		t.Fatalf("got %d proposals, want 1", len(props))
	}
	found := false
	for _, cv := range props[0].Caveats {
		if strings.Contains(cv, "grafana.url") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Caveats = %v, want one naming that grafana.url is not set", props[0].Caveats)
	}
}

// TestAMetricIsCaveatedWhenNoQueryLogIsConfigured is Important 1's second
// half, for the query log: no query_log.path configured leaves an ad-hoc
// read of this metric exactly as invisible as it is when the log is
// configured but exists to be checked -- LogRead is what says whether the
// log ran at all, and the caveat fires whenever it did not.
func TestAMetricIsCaveatedWhenNoQueryLogIsConfigured(t *testing.T) {
	c := corpus.Corpus{
		Used:                 map[string]bool{"m_total": true},
		UsedBy:               map[string]corpus.Source{"m_total": corpus.FromRule},
		Needs:                map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
		DashboardsConfigured: true,
		DashboardsReachable:  true,
		// LogRead left false: query_log.path was never set.
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 0 {
		t.Fatalf("refusals = %v, want none -- an unconfigured source caveats, it does not refuse", refs)
	}
	if len(props) != 1 {
		t.Fatalf("got %d proposals, want 1", len(props))
	}
	found := false
	for _, cv := range props[0].Caveats {
		if strings.Contains(cv, "query log") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Caveats = %v, want one naming that no query log is configured", props[0].Caveats)
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

// TestFromDashboardAloneStillRefusesAfterTheLogRulingChanged is a direct
// regression pin for the ruling in this round: removing the FromLog refusal
// must not have weakened the FromDashboard one it sits next to in refuse().
func TestFromDashboardAloneStillRefusesAfterTheLogRulingChanged(t *testing.T) {
	c := corpus.Corpus{
		Used:   map[string]bool{"m_total": true},
		UsedBy: map[string]corpus.Source{"m_total": corpus.FromRule | corpus.FromDashboard},
		Needs:  map[string]corpus.MetricNeed{"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}},
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(props) != 0 || len(refs) != 1 || !strings.Contains(refs[0].Reason, "dashboard") {
		t.Fatalf("got %d proposals and refusals %v, want exactly one refusal naming the dashboard", len(props), refs)
	}
}

// TestBothDashboardAndLogBitsRefuseForTheDashboardInTheSameRun exercises
// both paths together in one Decide call: a metric read by both a
// dashboard and the query log must still refuse -- for the dashboard,
// which keeps reading for as long as it exists -- while a second, log-only
// metric in the SAME run still gets proposed with its caveat. The two
// behaviours must not interfere with each other.
func TestBothDashboardAndLogBitsRefuseForTheDashboardInTheSameRun(t *testing.T) {
	inv2 := inventory.Inventory{
		Metrics: []inventory.Metric{
			{Name: "dash_and_log", Series: 400},
			{Name: "log_only", Series: 400},
		},
		TotalSeries: 800,
	}
	need := corpus.MetricNeed{Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true}
	c := corpus.Corpus{
		Used: map[string]bool{"dash_and_log": true, "log_only": true},
		UsedBy: map[string]corpus.Source{
			"dash_and_log": corpus.FromRule | corpus.FromDashboard | corpus.FromLog,
			"log_only":     corpus.FromRule | corpus.FromLog,
		},
		Needs:                map[string]corpus.MetricNeed{"dash_and_log": need, "log_only": need},
		DashboardsConfigured: true,
		DashboardsReachable:  true,
		LogRead:              true,
	}

	props, refs := Decide(inv2, c, nil)

	if len(refs) != 1 || refs[0].Metric != "dash_and_log" || !strings.Contains(refs[0].Reason, "dashboard") {
		t.Fatalf("refusals = %v, want exactly one refusing dash_and_log for the dashboard", refs)
	}
	if len(props) != 1 || props[0].Metric != "log_only" {
		t.Fatalf("proposals = %+v, want exactly one, for log_only", props)
	}
	if len(props[0].Caveats) != 1 {
		t.Fatalf("log_only's Caveats = %v, want one naming the query log", props[0].Caveats)
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

// The counter-reset defect this task exists for: summing a counter and
// rating the sum is not the same as summing the rates, so what gets
// recorded and named must be the rate, not the metric.
func TestARateConsumerProposesTheRateNotTheCounter(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{
			"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true,
				Fns: []string{"rate"}, Windows: []string{"5m"}},
		},
	}
	props, refs := Decide(inv("m_total", 400), c, nil)
	if len(props) != 1 {
		t.Fatalf("got %d proposals and %d refusals, want 1 proposal: %v", len(props), len(refs), refs)
	}
	p := props[0]
	if p.Fn != "rate" {
		t.Errorf("Fn = %q, want rate", p.Fn)
	}
	if p.Window != "5m" {
		t.Errorf("Window = %q, want 5m", p.Window)
	}
	if p.RuleName != "path:m_total:sum_rate5m" {
		t.Errorf("RuleName = %q, want path:m_total:sum_rate5m", p.RuleName)
	}
}

// Whichever function a recording rule picked, at least one consumer's
// answer would change -- irate and rate are not interchangeable.
func TestDisagreeingRangeFunctionsAreRefused(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{
			"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true,
				Fns: []string{"irate", "rate"}},
		},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "function") {
		t.Fatalf("refusals = %v, want one naming the function disagreement", refs)
	}
}

// Same function, different windows: exactly as unrewritable as different
// functions, because whichever window the rule picks, one consumer's rate
// changes underneath it.
func TestDisagreeingWindowsAreRefused(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{
			"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true,
				Fns: []string{"rate"}, Windows: []string{"10m", "5m"}},
		},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "window") {
		t.Fatalf("refusals = %v, want one naming the window disagreement", refs)
	}
}

// A consumer reading the counter directly and one rating it are answering
// different questions -- sum(m) is not sum(rate(m[5m])) -- so this must
// refuse exactly like two disagreeing functions do, and the empty string
// must read as "the metric itself" rather than an unlabelled blank.
func TestInstantAndRateMixIsRefused(t *testing.T) {
	c := corpus.Corpus{
		Used: map[string]bool{"m_total": true},
		Needs: map[string]corpus.MetricNeed{
			"m_total": {Required: []string{"path"}, Ops: []string{"sum"}, AllOpsSafe: true,
				Fns: []string{"", "rate"}},
		},
	}
	_, refs := Decide(inv("m_total", 400), c, nil)
	if len(refs) != 1 || !strings.Contains(refs[0].Reason, "the metric itself") {
		t.Fatalf("refusals = %v, want one naming the mix, with the empty string rendered as \"the metric itself\"", refs)
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

// TestTheEveryLabelReasonSaysOnlyWhatHoldsForItsOwnCase is the fix for a
// refusal that stated something false about the consumer: "some consumer
// needs every label" is true of a bare selector, topk or count_values,
// which pass every label of the raw series straight through -- but false of
// a consumer Blockers explains, which asked for a specific requirement
// (Required: [path]) that some OTHER cause (a function, a join, a clause
// jetsam cannot resolve, conflicting claims) then forced All on. A reader
// who opens `sum by (path)` after being told the consumer needed every
// label concludes jetsam is wrong. Blockers is exactly what tells the two
// cases apart, and each must get the sentence that is actually true of it.
func TestTheEveryLabelReasonSaysOnlyWhatHoldsForItsOwnCase(t *testing.T) {
	t.Run("no blocker: the consumer genuinely needs every label", func(t *testing.T) {
		c := corpus.Corpus{
			Used:  map[string]bool{"m_total": true},
			Needs: map[string]corpus.MetricNeed{"m_total": {All: true, Ops: []string{"topk"}}},
		}
		_, refs := Decide(inv("m_total", 400), c, nil)
		if len(refs) != 1 || !strings.Contains(refs[0].Reason, "some consumer needs every label") {
			t.Fatalf("refusals = %v, want the generic reason -- it is true here", refs)
		}
	})

	t.Run("a blocker: jetsam's requirement, not a claim about the consumer", func(t *testing.T) {
		c := corpus.Corpus{
			Used: map[string]bool{"m_total": true},
			Needs: map[string]corpus.MetricNeed{"m_total": {
				All: true, Required: []string{"path"},
				Blockers: []string{"abs(...) is applied to it before aggregation, and does not commute with pre-aggregation"},
			}},
		}
		_, refs := Decide(inv("m_total", 400), c, nil)
		if len(refs) != 1 {
			t.Fatalf("want a refusal, got %v", refs)
		}
		if strings.Contains(refs[0].Reason, "some consumer needs every label") {
			t.Errorf("Reason = %q -- this consumer asked for exactly path, not every label; the generic sentence is false here", refs[0].Reason)
		}
		if !strings.Contains(refs[0].Reason, "every label of this metric must survive") {
			t.Errorf("Reason = %q, want jetsam's own requirement stated instead", refs[0].Reason)
		}
		if !strings.Contains(refs[0].Reason, "abs(...) is applied to it before aggregation") {
			t.Errorf("Reason = %q, want the blocker text carried through", refs[0].Reason)
		}
	})
}
