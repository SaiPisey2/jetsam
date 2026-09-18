package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/SaiPisey2/jetsam/internal/emit"
	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/promapi"
)

// aggregateFixture stands up a fake Prometheus exposing one metric,
// http_requests_total, with a recording rule that aggregates it down to
// (path) with sum -- the shape aggregate.Decide proposes -- and answers
// the measurement query with keptSeries. It writes a jetsam.yaml that never
// sets prometheus_file: aggregate does not edit a scrape config and must
// not require one.
func aggregateFixture(t *testing.T, rawSeries, keptSeries int, extraYAML string) (cfgPath string) {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"http_requests_total","value":` + itoa(rawSeries) + `}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[
				{"name":"path:http_requests_total:sum","type":"recording","query":"sum by (path) (http_requests_total)"}
			]}]}}`))
		case "/api/v1/query":
			q := r.URL.Query().Get("query")
			if !strings.Contains(q, `count(count by (path) ({__name__="http_requests_total", __ignore_usage__=""}))`) {
				t.Errorf("unexpected measurement query: %s", q)
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + itoa(keptSeries) + `"]}]}}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)

	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\n"+extraYAML), 0o644)
	return cfgPath
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestAggregateDoesNotRequirePrometheusFile: aggregate is a report, not an
// editor of a scrape config, and must run without prometheus_file set at
// all.
func TestAggregateDoesNotRequirePrometheusFile(t *testing.T) {
	cfgPath := aggregateFixture(t, 400, 12, "")
	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
}

// TestAggregateHasNoApplyFlag pins the brief's hardest constraint: this
// command opens no pull request and writes nothing, so -apply must not
// exist as a flag at all.
func TestAggregateHasNoApplyFlag(t *testing.T) {
	cfgPath := aggregateFixture(t, 400, 12, "")
	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath, "-apply"}, &stdout, &stderr, noEnv)
	if rc == 0 {
		t.Fatalf("aggregateCmd accepted -apply, want it undefined: stdout=%s", stdout.String())
	}
}

// TestAggregateAlwaysPrintsTheNotAppliedNotice runs the full command
// end-to-end and checks the safety sentence survives the whole pipeline,
// not just report.Aggregate in isolation.
func TestAggregateAlwaysPrintsTheNotAppliedNotice(t *testing.T) {
	cfgPath := aggregateFixture(t, 400, 12, "")
	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d: stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nothing here is applied") {
		t.Errorf("output does not say nothing is applied:\n%s", stdout.String())
	}
}

// TestAggregateProposesACollapsibleMetric is the positive end-to-end case:
// a metric with a real reduction should be named, with its rendered rule.
func TestAggregateProposesACollapsibleMetric(t *testing.T) {
	cfgPath := aggregateFixture(t, 400, 12, "")
	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "http_requests_total") {
		t.Fatalf("output does not name the collapsible metric:\n%s", out)
	}
	if !strings.Contains(out, "path:http_requests_total:sum") {
		t.Errorf("output does not show the rendered recording rule's name:\n%s", out)
	}
}

// TestAggregateWithholdsAMetricThatSavesNoSeries is the mutation-pinning
// test for the KeptSeries >= RawSeries withholding: the measured aggregate
// has exactly as many series as the raw metric, so collapsing it saves
// nothing and it must not be named.
func TestAggregateWithholdsAMetricThatSavesNoSeries(t *testing.T) {
	cfgPath := aggregateFixture(t, 400, 400, "")
	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "http_requests_total") {
		t.Errorf("metric that saves no series was named:\n%s", out)
	}
	if !strings.Contains(out, "nothing to collapse") {
		t.Errorf("output does not say there was nothing to collapse:\n%s", out)
	}
}

// TestAggregateWithholdsBelowTheConfiguredMinimum: a real but small saving,
// below aggregate.min_series_saved, must not be named either.
func TestAggregateWithholdsBelowTheConfiguredMinimum(t *testing.T) {
	cfgPath := aggregateFixture(t, 400, 350, "aggregate:\n  min_series_saved: 100\n")
	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "http_requests_total") {
		t.Errorf("metric saving only 50 series was named against a minimum of 100:\n%s", out)
	}
}

// TestAggregateWithholdsAnUnmeasurableProposal: the count query jetsam needs
// to trust KeptSeries fails, so the proposal must be withheld rather than
// printed with a missing or zero number.
func TestAggregateWithholdsAnUnmeasurableProposal(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[{"name":"http_requests_total","value":400}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[
				{"name":"path:http_requests_total:sum","type":"recording","query":"sum by (path) (http_requests_total)"}
			]}]}}`))
		case "/api/v1/query":
			w.Write([]byte(`{"status":"error","errorType":"timeout","error":"query timed out"}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer prom.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\n"), 0o644)

	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "path:http_requests_total:sum") || strings.Contains(out, "400 series") {
		t.Errorf("an unmeasured proposal was printed with a number jetsam never obtained:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "http_requests_total") {
		t.Errorf("stderr does not say which metric could not be measured:\n%s", stderr.String())
	}
}

// TestAggregateReportsADeclinedConsumerEndToEnd exercises the real wiring
// from a rule through emit.RewriteConsumer: a second rule reads the metric
// with an extra label matcher the recording rule's own expression never
// carries, so RewriteConsumer must decline it, and the report must name it
// as not rewritable rather than leave it out.
func TestAggregateReportsADeclinedConsumerEndToEnd(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[{"name":"http_requests_total","value":400}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[
				{"name":"path:http_requests_total:sum","type":"recording","query":"sum by (path) (http_requests_total)"},
				{"name":"NarrowAlert","type":"alerting","query":"sum by (path) (http_requests_total{job=\"api\"}) > 100"}
			]}]}}`))
		case "/api/v1/query":
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"12"]}]}}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer prom.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\n"), 0o644)

	var stdout, stderr bytes.Buffer
	rc := aggregateCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv)
	if rc != 0 {
		t.Fatalf("aggregateCmd = %d: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "NarrowAlert") {
		t.Fatalf("declined consumer NarrowAlert was omitted from the report:\n%s", out)
	}
	if !strings.Contains(out, "not rewritable") {
		t.Errorf("report does not say the consumer is not rewritable:\n%s", out)
	}
}

// TestAggregateRunTwiceAgainstTheSameLogProducesTheSameResult is the
// regression test for the class of defect this round fixes: Prometheus logs
// every /api/v1/query call it answers, including jetsam's own measurement
// query, so a metric aggregate measures on one run would -- without a
// marker naming that query as tooling -- look, on the NEXT run, like it is
// read ad hoc by a `count` consumer. Running the tool would be what stops
// the tool working. This reproduces exactly that sequence: the first run's
// measurement query is captured and written into a query log as a real
// Prometheus log line, and the second run, configured to read that log,
// must reach the same answer as the first.
func TestAggregateRunTwiceAgainstTheSameLogProducesTheSameResult(t *testing.T) {
	dir := t.TempDir()

	var loggedQuery string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[{"name":"http_requests_total","value":400}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[
				{"name":"path:http_requests_total:sum","type":"recording","query":"sum by (path) (http_requests_total)"}
			]}]}}`))
		case "/api/v1/query":
			// Prometheus itself would write exactly this query string to
			// its own query log -- captured here rather than hand-built,
			// so this test fails loudly if measureKeptSeries' query shape
			// ever changes without this test being updated to match.
			loggedQuery = r.URL.Query().Get("query")
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"12"]}]}}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer prom.Close()

	cfgPath := filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\n"), 0o644)

	var first bytes.Buffer
	if rc := aggregateCmd([]string{"-config", cfgPath}, &first, &bytes.Buffer{}, noEnv); rc != 0 {
		t.Fatalf("first run: aggregateCmd = %d", rc)
	}
	const wantLine = "http_requests_total: 400 series -> 12 kept (saves 388)"
	if !strings.Contains(first.String(), wantLine) {
		t.Fatalf("first run does not propose the metric as expected:\n%s", first.String())
	}
	if loggedQuery == "" {
		t.Fatal("test setup: the measurement query was never captured")
	}

	// Write a query log carrying exactly the line a real Prometheus with
	// global.query_log_file set would have written for that call.
	logPath := filepath.Join(dir, "queries.log")
	logLine := fmt.Sprintf(`{"time":"2026-09-19T00:00:00.000Z","httpRequest":{"clientIP":"1.2.3.4","method":"GET","path":"/api/v1/query"},"params":{"query":%q}}`+"\n", loggedQuery)
	if err := os.WriteFile(logPath, []byte(logLine), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgWithLog := filepath.Join(dir, "jetsam-with-log.yaml")
	os.WriteFile(cfgWithLog, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\n"+
		"query_log:\n  path: "+logPath+"\n  min_window: 1s\n"), 0o644)

	var second bytes.Buffer
	if rc := aggregateCmd([]string{"-config", cfgWithLog}, &second, &bytes.Buffer{}, noEnv); rc != 0 {
		t.Fatalf("second run: aggregateCmd = %d:\n%s", rc, second.String())
	}
	if !strings.Contains(second.String(), wantLine) {
		t.Fatalf("second run, with jetsam's own prior measurement query now in the log, no longer "+
			"reaches the same answer as the first:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
	if strings.Contains(second.String(), "disagree") {
		t.Errorf("second run treats jetsam's own logged measurement query as a disagreeing consumer:\n%s", second.String())
	}
	if strings.Contains(second.String(), "CAVEAT") {
		t.Errorf("second run adds a caveat for jetsam's own query, which is tooling, not real usage:\n%s", second.String())
	}
}

// TestDashboardConsumersNamesTheDashboardThatReadsAMetric is a direct unit
// test of the helper cmd/jetsam builds to feed aggregate.Decide's
// dashboardConsumers parameter, since Corpus itself carries no per-dashboard
// breakdown (see decide.go's own doc comment on why the caller must supply
// it).
func TestDashboardConsumersNamesTheDashboardThatReadsAMetric(t *testing.T) {
	dashboards := []grafana.Dashboard{
		{Title: "Node Exporter Full", Queries: []string{"rate(http_requests_total[5m])"}},
	}
	got := dashboardConsumers(dashboards, []string{"http_requests_total"})
	names := got["http_requests_total"]
	if len(names) != 1 || names[0] != "Node Exporter Full" {
		t.Errorf("dashboardConsumers = %v, want [\"Node Exporter Full\"]", got)
	}
}

// TestDashboardConsumersDeduplicatesByTitle: a metric a dashboard reads
// through two different panels must not be named twice.
func TestDashboardConsumersDeduplicatesByTitle(t *testing.T) {
	dashboards := []grafana.Dashboard{
		{Title: "D", Queries: []string{"http_requests_total", "rate(http_requests_total[5m])"}},
	}
	got := dashboardConsumers(dashboards, []string{"http_requests_total"})
	if len(got["http_requests_total"]) != 1 {
		t.Errorf("dashboardConsumers = %v, want a single deduplicated entry", got["http_requests_total"])
	}
}

// TestRulesReadingFindsEveryRuleTouchingTheMetric is a direct unit test of
// the helper that fills aggregate.Proposal.Consumers, which decide.go
// explicitly leaves nil because Corpus carries no rule list.
func TestRulesReadingFindsEveryRuleTouchingTheMetric(t *testing.T) {
	rules := []promapi.Rule{
		{Group: "g", Name: "r1", Query: "sum by (path) (http_requests_total)", Type: "recording"},
		{Group: "g", Name: "r2", Query: "up == 0", Type: "alerting"},
	}
	got := rulesReading("http_requests_total", rules, []string{"http_requests_total", "up"})
	if len(got) != 1 || got[0].Name != "r1" || got[0].Kind != "rule" {
		t.Errorf("rulesReading = %v, want exactly r1", got)
	}
}

// TestWithholdReasonCatchesKeptSeriesAtLeastRawSeries is the isolated
// mutation-pinning test for the first withholding check: with minSaved
// pinned at 0 -- a value config.Load itself can never produce, since it
// treats min_series_saved: 0 as unset and defaults it to 100 -- the only
// thing that can still withhold a proposal whose kept series equal its raw
// series is the KeptSeries >= RawSeries check on its own.
func TestWithholdReasonCatchesKeptSeriesAtLeastRawSeries(t *testing.T) {
	p := aggregate.Proposal{Metric: "m", Keep: []string{"path"}, RawSeries: 400, KeptSeries: 400}
	if got := withholdReason(p, 0); got == "" {
		t.Fatal("withholdReason did not withhold a proposal that saves nothing, even with the minimum-saved check disabled")
	}
}

// TestWithholdReasonCatchesBelowTheMinimum is the isolated counterpart: a
// real, positive saving that still falls short of minSaved must be
// withheld even though the first check does not fire.
func TestWithholdReasonCatchesBelowTheMinimum(t *testing.T) {
	p := aggregate.Proposal{Metric: "m", Keep: []string{"path"}, RawSeries: 400, KeptSeries: 350}
	if got := withholdReason(p, 100); got == "" {
		t.Fatal("withholdReason did not withhold a proposal saving only 50 series against a configured minimum of 100")
	}
}

// TestWithholdReasonAllowsAGenuineSaving is the negative case for both:
// a real saving clearing the minimum must not be withheld.
func TestWithholdReasonAllowsAGenuineSaving(t *testing.T) {
	p := aggregate.Proposal{Metric: "m", Keep: []string{"path"}, RawSeries: 400, KeptSeries: 5}
	if got := withholdReason(p, 100); got != "" {
		t.Errorf("withholdReason withheld a genuine 395-series saving: %q", got)
	}
}

// TestBuildConsumerFindingHandlesAPartialRewrite is the direct unit test of
// the glue between aggregateCmd and emit.RewriteConsumer, using the exact
// case a code reviewer flagged: a consumer's expression contains one
// aggregate that matches the proposal (sum) and a second, over the same
// metric, that does not (min). RewriteConsumer rewrites the first and
// declines the second, and buildConsumerFinding must carry both through --
// this cannot be reached via a real corpus, because a rule that aggregates
// the same metric under two disagreeing operators in one query is exactly
// what corpus.Build's own conflicting-operator check refuses upstream (see
// internal/corpus/labels.go's opSeen check), so aggregateCmd's own glue
// code is the only place left to pin this contract directly.
func TestBuildConsumerFindingHandlesAPartialRewrite(t *testing.T) {
	p := aggregate.Proposal{
		Metric: "m_total", Keep: []string{"path"}, Op: "sum",
		Fn: "rate", Window: "5m", RuleName: "path:m_total:sum_rate5m",
	}
	c := aggregate.Consumer{
		Kind: "rule", Group: "g", Name: "Mixed",
		Query: "sum by (path) (rate(m_total[5m])) + min by (path) (rate(m_total[5m]))",
	}

	got := buildConsumerFinding(c, p)

	wantRewritten := "path:m_total:sum_rate5m + min by (path) (rate(m_total[5m]))"
	if got.Rewritten != wantRewritten {
		t.Errorf("Rewritten = %q, want %q", got.Rewritten, wantRewritten)
	}
	if got.Declined == "" {
		t.Fatal("Declined is empty, want the reason the min aggregate was left alone")
	}
	if !strings.Contains(got.Declined, "min") || !strings.Contains(got.Declined, "sum") {
		t.Errorf("Declined = %q, want it to name both operators", got.Declined)
	}

	// Sanity: confirm this really is RewriteConsumer's own partial case,
	// not a mistake in the fixture -- both a change and a decline present
	// at once.
	rewritten, declined, err := emit.RewriteConsumer(c.Query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if rewritten == c.Query || declined == "" {
		t.Fatalf("test fixture does not exercise RewriteConsumer's partial-rewrite case: rewritten=%q declined=%q", rewritten, declined)
	}
}
