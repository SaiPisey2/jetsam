package corpus

import (
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
)

func testRules() []promapi.Rule {
	return []promapi.Rule{
		{Group: "api", Name: "ErrorRateHigh", Type: "alerting",
			Query: `sum(rate(http_requests_total{code=~"5.."}[5m])) > 1`},
		{Group: "api", Name: "job:latency:p99", Type: "recording",
			Query: `histogram_quantile(0.99, sum by (le, job) (rate(http_request_duration_seconds_bucket[5m])))`},
	}
}

func TestBuildMarksEveryRuleInputUsed(t *testing.T) {
	all := []string{"http_requests_total", "http_request_duration_seconds_bucket", "go_goroutines"}
	c := Build(Sources{Rules: testRules()}, all)

	for _, want := range []string{"http_requests_total", "http_request_duration_seconds_bucket"} {
		if !c.Used[want] {
			t.Errorf("%s: Used = false, want true", want)
		}
	}
	if c.Used["go_goroutines"] {
		t.Error("go_goroutines: Used = true, want false — no rule reads it")
	}
	if c.Queries != 2 {
		t.Errorf("Queries = %d, want 2", c.Queries)
	}
}

func TestBuildRecordsWhatRecordingRulesProduce(t *testing.T) {
	// A recording rule's OUTPUT is a real series in the inventory. v0.1
	// never proposes dropping one, because doing so means editing a rule
	// file rather than a scrape config.
	c := Build(Sources{Rules: testRules()}, []string{"http_requests_total"})
	if !c.Produced["job:latency:p99"] {
		t.Error("Produced[job:latency:p99] = false, want true")
	}
	if c.Produced["ErrorRateHigh"] {
		t.Error("Produced[ErrorRateHigh] = true — an alerting rule produces no series")
	}
}

func TestBuildBlocksOnAnUnparseableQuery(t *testing.T) {
	rules := []promapi.Rule{{Group: "g", Name: "Broken", Type: "alerting", Query: `rate(x[$interval])`}}
	c := Build(Sources{Rules: rules}, []string{"x", "y"})
	if len(c.Blocked) != 1 {
		t.Fatalf("Blocked = %v, want one entry naming the unreadable rule", c.Blocked)
	}
	if c.Used["y"] {
		t.Error("y: Used = true — a blocked corpus must not mark unrelated metrics used")
	}
}

// TestBuildProtectsABlockedRecordingRulesOutput pins an asymmetry: what a
// recording rule WRITES is reported by the rules API as its Name and Type,
// independently of whether jetsam can parse its query, so a parse failure
// must never strip it from Produced -- doing so would make that series
// eligible for deletion, when jetsam knows with certainty it is written.
// What the rule READS is a different story: an unparseable query might read
// anything, so Used stays gated on a successful Extract. Produced is the
// safe direction to protect because it is what excludes a metric from being
// a drop candidate downstream; Used is the safe direction to withhold
// because asserting a read jetsam did not actually verify is what would
// license an unsound drop of something else entirely.
func TestBuildProtectsABlockedRecordingRulesOutput(t *testing.T) {
	rules := []promapi.Rule{
		{Group: "g", Name: "Good", Type: "alerting", Query: `up == 0`},
		{Group: "g", Name: "Broken", Type: "recording", Query: `rate(x[$interval])`},
	}
	c := Build(Sources{Rules: rules}, []string{"up", "x", "unrelated"})

	if !c.Used["up"] {
		t.Error("up: Used = false, want true — the clean rule was processed normally")
	}
	if c.Used["x"] {
		t.Error("x: Used = true — the broken rule's query text names x, but an unreadable rule must contribute no reads")
	}
	if c.Used["unrelated"] {
		t.Error("unrelated: Used = true, want false")
	}
	if !c.Produced["Broken"] {
		t.Error("Produced[Broken] = false, want true — the rules API says this rule writes this series regardless of whether its query parses, so it must stay protected from deletion")
	}
	if len(c.Blocked) != 1 {
		t.Fatalf("Blocked = %v, want exactly one entry", c.Blocked)
	}
	if !strings.Contains(c.Blocked[0], "g") || !strings.Contains(c.Blocked[0], "Broken") {
		t.Errorf("Blocked[0] = %q, want it to name both the group %q and the rule %q", c.Blocked[0], "g", "Broken")
	}
}

// A metric only a dashboard reads is used. This is the case v0.1 could not
// see, and the whole reason this sub-project exists.
func TestDashboardsExtendUsed(t *testing.T) {
	c := Build(Sources{
		Rules: nil,
		Dashboards: []grafana.Dashboard{{
			UID: "d", Title: "D",
			Queries: []string{`sum by (path) (rate(dashboard_only_metric[$__rate_interval]))`},
		}},
	}, []string{"dashboard_only_metric", "nothing_reads_this"})

	if !c.Used["dashboard_only_metric"] {
		t.Error("a metric a dashboard reads is not marked used")
	}
	if c.Used["nothing_reads_this"] {
		t.Error("a metric nothing reads was marked used")
	}
}

// A metric named only in a dashboard's own template-variable definition is
// used. The vendored "Node Exporter Full" dashboard's job/nodename/node
// dropdowns all query label_values(node_uname_info, ...), and no panel
// names that metric at all -- so a reader that only walks panel targets
// silently grades it unreferenced on an install with Grafana configured
// and no query log, which is the ordinary starting configuration. Grafana
// issues that query to Prometheus every time the dashboard loads, so this
// is a real read, not a technicality.
func TestDashboardVariableQueriesExtendUsed(t *testing.T) {
	c := Build(Sources{
		Dashboards: []grafana.Dashboard{{
			UID: "d", Title: "D",
			VariableQueries: []string{`label_values(variable_only_metric, job)`},
		}},
	}, []string{"variable_only_metric", "nothing_reads_this"})

	if !c.Used["variable_only_metric"] {
		t.Error("a metric named only in a dashboard's template-variable query is not marked used")
	}
	if c.Used["nothing_reads_this"] {
		t.Error("a metric nothing reads was marked used")
	}
	// label_values(...) is a Grafana template function, not PromQL, so this
	// always takes the same conservative fallback path an unparseable panel
	// does -- reported under its own label so an operator does not read
	// this as a broken panel.
	if len(c.DashboardVariablesUnparsed) != 1 {
		t.Errorf("DashboardVariablesUnparsed = %v, want the variable query named", c.DashboardVariablesUnparsed)
	}
}

// An unparseable RULE is fatal. Rules are Prometheus' own configuration, and
// one jetsam cannot read means it is misreading something fundamental.
func TestAnUnparseableRuleIsFatal(t *testing.T) {
	c := Build(Sources{
		Rules: []promapi.Rule{{Name: "bad", Group: "g", Type: "alerting", Query: "this is not promql {{{"}},
	}, nil)
	if len(c.Blocked) == 0 {
		t.Error("an unparseable rule did not block")
	}
}

// An unparseable DASHBOARD PANEL is contained, not fatal: it must not block
// every drop, because one bad panel in a large Grafana install would make the
// whole remediation half unreachable. Its metric-name-shaped tokens are
// conservatively marked used instead.
func TestAnUnparseableDashboardPanelIsContained(t *testing.T) {
	c := Build(Sources{
		Dashboards: []grafana.Dashboard{{
			UID: "d", Title: "D",
			Queries: []string{`!!! not promql at all but mentions suspicious_metric here`},
		}},
	}, []string{"suspicious_metric", "unrelated_metric"})

	if len(c.Blocked) != 0 {
		t.Errorf("an unparseable dashboard panel blocked everything: %v", c.Blocked)
	}
	if len(c.DashboardPanelsUnparsed) != 1 {
		t.Errorf("DashboardPanelsUnparsed = %v, want the panel named", c.DashboardPanelsUnparsed)
	}
	if !c.Used["suspicious_metric"] {
		t.Error("a metric named in an unparseable panel was not conservatively marked used")
	}
	if c.Used["unrelated_metric"] {
		t.Error("containment was too broad -- a metric the panel never mentions was marked used")
	}
}

func TestQueryLogReadsExtendUsed(t *testing.T) {
	c := Build(Sources{
		QueryLog:     &querylog.Reading{Queries: []string{"adhoc_metric"}, Span: 40 * 24 * time.Hour},
		LogQualifies: true,
	}, []string{"adhoc_metric", "never_read"})

	if !c.Used["adhoc_metric"] {
		t.Error("a metric read in the query log is not marked used")
	}
	if !c.LogRead || !c.LogQualifies {
		t.Errorf("LogRead=%v LogQualifies=%v, want both true", c.LogRead, c.LogQualifies)
	}
}

func TestAShortLogDoesNotQualify(t *testing.T) {
	c := Build(Sources{
		QueryLog:     &querylog.Reading{Queries: nil, Span: time.Hour},
		LogQualifies: false,
	}, []string{"m"})
	if !c.LogRead {
		t.Error("LogRead should be true -- a log WAS read")
	}
	if c.LogQualifies {
		t.Error("a one-hour log must not qualify")
	}
}

// TestAnUnparseableLoggedQueryIsContainedAndReported: the query log is the
// one source whose SILENCE licenses a deletion, and it had the weakest
// handling of the three -- a query that would not parse was silently
// continued past, recorded nowhere. Rules are fatal and dashboard panels
// get a lexical fallback; this gives logged queries the same fallback and
// makes the failure visible.
func TestAnUnparseableLoggedQueryIsContainedAndReported(t *testing.T) {
	all := []string{"logged_metric", "other_metric"}
	c := Build(Sources{
		QueryLog: &querylog.Reading{
			Queries: []string{`rate(logged_metric[`, "other_metric"},
			Span:    800 * time.Hour,
		},
		LogQualifies: true,
	}, all)

	if len(c.Blocked) != 0 {
		t.Errorf("an unparseable logged query was fatal: %v", c.Blocked)
	}
	if len(c.QueryLogUnparsed) != 1 {
		t.Fatalf("QueryLogUnparsed = %v, want exactly one entry", c.QueryLogUnparsed)
	}
	if !strings.Contains(c.QueryLogUnparsed[0], "query log") {
		t.Errorf("QueryLogUnparsed entry %q does not say which source it came from", c.QueryLogUnparsed[0])
	}
	// The conservative fallback: the metric named in the query it could
	// not parse is still protected, exactly as a malformed panel's is.
	if !c.Used["logged_metric"] {
		t.Error("logged_metric: Used = false -- an unparseable logged query did not protect the metric it names")
	}
	if !c.Used["other_metric"] {
		t.Error("other_metric: Used = false -- the parseable query beside it was not read")
	}
}

// TestUsedByNamesTheSourceThatReadEachMetric is what makes a verdict's
// reason true. The corpus was rules-only when that sentence was written
// and has not been since; a metric only a dashboard reads must not be
// reported as read by a rule.
func TestUsedByNamesTheSourceThatReadEachMetric(t *testing.T) {
	all := []string{"rule_metric", "dash_metric", "logged_metric", "shared_metric"}
	c := Build(Sources{
		Rules: []promapi.Rule{
			{Group: "g", Name: "r", Type: "alerting", Query: `rule_metric > 0`},
			{Group: "g", Name: "s", Type: "alerting", Query: `shared_metric > 0`},
		},
		Dashboards: []grafana.Dashboard{{UID: "d", Title: "D",
			Queries: []string{`rate(dash_metric[5m])`, `shared_metric`}}},
		QueryLog:             &querylog.Reading{Queries: []string{"logged_metric"}, Span: 800 * time.Hour},
		LogQualifies:         true,
		DashboardsConfigured: true, DashboardsReachable: true,
	}, all)

	for metric, want := range map[string]string{
		"rule_metric":   "read by a rule",
		"dash_metric":   "read by a dashboard",
		"logged_metric": "read by a logged query",
		"shared_metric": "read by a rule and a dashboard",
	} {
		if got := c.UsedBy[metric].Reason(); got != want {
			t.Errorf("%s: reason = %q, want %q", metric, got, want)
		}
	}
}

func TestSourceListNamesOnlyTheSourcesActuallyUsed(t *testing.T) {
	rules := []promapi.Rule{{Group: "g", Name: "r", Type: "alerting", Query: `up > 0`}}
	for _, tc := range []struct {
		name string
		src  Sources
		want string
	}{
		{"rules alone", Sources{Rules: rules}, "rules"},
		{"rules and dashboards", Sources{Rules: rules,
			DashboardsConfigured: true, DashboardsReachable: true}, "rules and dashboards"},
		{"rules and log", Sources{Rules: rules,
			QueryLog: &querylog.Reading{}}, "rules and the query log"},
		{"all three", Sources{Rules: rules, QueryLog: &querylog.Reading{},
			DashboardsConfigured: true, DashboardsReachable: true}, "rules, dashboards and the query log"},
		// Configured and unreachable contributed nothing, so naming it
		// would overstate the corpus to whoever approves the deletion.
		{"dashboards unreachable", Sources{Rules: rules, DashboardsConfigured: true}, "rules"},
	} {
		if got := Build(tc.src, nil).SourceList(); got != tc.want {
			t.Errorf("%s: SourceList = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLogShortfallTellsTheThreeStatesApart(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Corpus
		want string
	}{
		{"none configured", Corpus{}, "no query log is configured"},
		{"configured but short", Corpus{LogRead: true, LogSpan: 3 * time.Hour},
			"the query log covers only 3h0m0s, short of the configured minimum"},
		{"unreadable", Corpus{LogUnreadable: true}, "the query log is configured but could not be read"},
		{"qualifies", Corpus{LogRead: true, LogQualifies: true, LogSpan: 800 * time.Hour}, ""},
	} {
		if got := tc.c.LogShortfall(); got != tc.want {
			t.Errorf("%s: LogShortfall = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAnUnreadableLogIsNotAnEmptyOne: an empty log says nobody queried
// anything, which licenses dropping everything. An unreadable one must say
// the opposite.
func TestAnUnreadableLogIsNotAnEmptyOne(t *testing.T) {
	c := Build(Sources{QueryLogUnreadable: true}, nil)
	if !c.LogUnreadable {
		t.Error("LogUnreadable = false, want true")
	}
	if c.LogRead || c.LogQualifies {
		t.Errorf("LogRead = %v, LogQualifies = %v -- an unreadable log must not look like evidence", c.LogRead, c.LogQualifies)
	}
}
