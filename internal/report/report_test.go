package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

func TestScanStatesWhyNothingIsProposed(t *testing.T) {
	inv := inventory.Build(promapi.Status{Counts: map[string]int{"unread": 400}, HeadSeries: 400})
	c := corpus.Corpus{Queries: 5, Used: map[string]bool{}, Produced: map[string]bool{}}
	res := verdict.Compute(inv, c)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	// A report that shows an unread metric but never says why it is not
	// being proposed reads as a broken tool rather than an honest one.
	if !strings.Contains(out, "unreferenced") {
		t.Errorf("report does not grade the metric:\n%s", out)
	}
	if !strings.Contains(out, "query log") {
		t.Errorf("report does not explain that a query log is missing:\n%s", out)
	}
}

// TestScanBlamesABlockedRuleNotAMissingQueryLog pins the "small" fix: when
// DroppableSeries is 0 because a blocked rule withholds every drop, the
// message must say so -- not blame a missing query log, which would tell
// an operator to configure query_log.path when doing so would change
// nothing until the blocked rule itself is fixed.
func TestScanBlamesABlockedRuleNotAMissingQueryLog(t *testing.T) {
	inv := inventory.Build(promapi.Status{Counts: map[string]int{"unread": 400}, HeadSeries: 400})
	c := corpus.Corpus{
		Queries: 1, Used: map[string]bool{}, Produced: map[string]bool{},
		Blocked: []string{"rule g/Broken: parse error"},
		// LogRead/LogQualifies true: even WITH a qualifying query log, a
		// blocked rule still forbids every drop, so "no query log" would be
		// doubly wrong here.
		LogRead: true, LogQualifies: true,
	}
	res := verdict.Compute(inv, c)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	if strings.Contains(out, "Droppable  none -- no query log") {
		t.Errorf("report blames a missing query log when a blocked rule is the actual reason:\n%s", out)
	}
	if !strings.Contains(out, "Droppable  none -- 1 rule(s) could not be read") {
		t.Errorf("report does not blame the blocked rule:\n%s", out)
	}
}

// TestScanSaysEverythingIsAccountedForWhenNothingIsMissing covers the third
// condition: a query log IS configured, nothing is blocked, and every
// metric is genuinely used -- "Droppable none" here is not evidence of a
// gap, and the message must not claim one exists.
func TestScanSaysEverythingIsAccountedForWhenNothingIsMissing(t *testing.T) {
	inv := inventory.Build(promapi.Status{Counts: map[string]int{"used_metric": 400}, HeadSeries: 400})
	c := corpus.Corpus{
		Queries: 1, Used: map[string]bool{"used_metric": true}, Produced: map[string]bool{},
		LogRead: true, LogQualifies: true,
	}
	res := verdict.Compute(inv, c)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	if strings.Contains(out, "no query log") {
		t.Errorf("report blames a missing query log although one is configured and nothing was missed:\n%s", out)
	}
	if strings.Contains(out, "could not be read") {
		t.Errorf("report blames a blocked rule that does not exist:\n%s", out)
	}
	if !strings.Contains(out, "Droppable  none -- every metric is referenced") {
		t.Errorf("report does not state that everything is accounted for:\n%s", out)
	}
}

func TestScanNeverEmitsARawEscapeByte(t *testing.T) {
	// A metric name carrying an ANSI escape can clear or forge lines in a
	// terminal. The remote Prometheus is not trusted, so the raw ESC byte
	// (0x1b) must never reach the rendered report.
	const hostile = "up\x1b[2K\x1b[1;31mFAKE\x1b[0m"
	inv := inventory.Build(promapi.Status{Counts: map[string]int{hostile: 400}, HeadSeries: 400})
	c := corpus.Corpus{Queries: 0, Used: map[string]bool{}, Produced: map[string]bool{}}
	res := verdict.Compute(inv, c)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("report contains a raw ESC byte:\n%q", out)
	}
	if !strings.Contains(out, `\x1b`) {
		t.Errorf("report does not render the escape visibly:\n%s", out)
	}
}

// TestScanWarnsLoudlyWhenTheInventoryWasTruncated pins C2's detection half:
// a truncated inventory (metric_limit cut the metric list short) must print
// a loud warning naming both the configured limit and the true metric-name
// count, not just silently compute a percentage against the wrong total.
func TestScanWarnsLoudlyWhenTheInventoryWasTruncated(t *testing.T) {
	inv := inventory.Build(promapi.Status{
		Counts:     map[string]int{"a": 10},
		HeadSeries: 15777,
		Limit:      40,
		NameCount:  1374,
		Truncated:  true,
	})
	c := corpus.Corpus{Queries: 0, Used: map[string]bool{}, Produced: map[string]bool{}}
	res := verdict.Compute(inv, c)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	if !strings.Contains(out, "WARNING") {
		t.Errorf("report does not warn about truncation at all:\n%s", out)
	}
	if !strings.Contains(out, "40") || !strings.Contains(out, "1374") {
		t.Errorf("report's truncation warning does not name both the limit and the true metric-name count:\n%s", out)
	}
}

// TestScanDoesNotWarnWhenNotTruncated is the negative case: an inventory
// that covers every metric name must not print a truncation warning.
func TestScanDoesNotWarnWhenNotTruncated(t *testing.T) {
	inv := inventory.Build(promapi.Status{
		Counts:     map[string]int{"a": 10},
		HeadSeries: 10,
		Limit:      5000,
		NameCount:  1,
		Truncated:  false,
	})
	c := corpus.Corpus{Queries: 0, Used: map[string]bool{}, Produced: map[string]bool{}}
	res := verdict.Compute(inv, c)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	if strings.Contains(out, "WARNING") {
		t.Errorf("report warns about truncation although the inventory was not truncated:\n%s", out)
	}
}

func TestScanStatesTheLogSpanAgainstTheMinimum(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "m", Series: 1}}, TotalSeries: 1}
	c := corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		LogRead: true, LogSpan: 3 * time.Hour, LogQualifies: false,
	}
	var b bytes.Buffer
	Scan(&b, inv, c, verdict.Compute(inv, c))
	out := b.String()
	if !strings.Contains(out, "3h") {
		t.Errorf("report does not state the log span:\n%s", out)
	}
	if !strings.Contains(out, "not long enough") {
		t.Errorf("report does not say the span was insufficient:\n%s", out)
	}
}

func TestScanSaysWhenDashboardEvidenceIsMissing(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "m", Series: 1}}, TotalSeries: 1}
	c := corpus.Corpus{
		Used: map[string]bool{}, Produced: map[string]bool{},
		DashboardsConfigured: true, DashboardsReachable: false,
	}
	var b bytes.Buffer
	Scan(&b, inv, c, verdict.Compute(inv, c))
	if !strings.Contains(b.String(), "could not be fetched") {
		t.Errorf("report does not say dashboard evidence was unavailable:\n%s", b.String())
	}
}

// TestScanSaysDashboardsMissingIsWhyNothingIsDroppable is the correction to
// Task 6's brief: the aggregate "Droppable none" switch must not fall
// through to the "every metric is referenced" message when the real reason
// is that dashboard evidence was configured but could not be fetched.
// Nothing is blocked, no metric is graded unreferenced (there is no query
// log at all here, so ordinarily that branch would fire first) -- the
// corpus is empty of any other explanation except the missing dashboards.
func TestScanSaysDashboardsMissingIsWhyNothingIsDroppable(t *testing.T) {
	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "used_metric", Series: 400}}, TotalSeries: 400}
	c := corpus.Corpus{
		Used: map[string]bool{"used_metric": true}, Produced: map[string]bool{},
		DashboardsConfigured: true, DashboardsReachable: false,
	}
	res := verdict.Compute(inv, c)

	var b bytes.Buffer
	Scan(&b, inv, c, res)
	out := b.String()

	if !strings.Contains(out, "Droppable  none -- dashboard evidence was configured but could not be fetched") {
		t.Errorf("report does not blame missing dashboard evidence for nothing being droppable:\n%s", out)
	}
	if strings.Contains(out, "every metric is referenced by a rule or was read within the query-log window") {
		t.Errorf("report falls through to the misleading default explanation:\n%s", out)
	}
}

// TestScanHeaderAttributesQueriesToAllThreeSources pins the "Queries" header
// line against a corpus actually built from all three evidence sources --
// rules, a dashboard, and a query log -- so the header cannot silently drift
// back to naming only rules the way it did before this fix: corpus.Queries
// sums all three, and on the live fixture the header once read "739 read
// from rules" when 285 of those queries came from Grafana.
func TestScanHeaderAttributesQueriesToAllThreeSources(t *testing.T) {
	rules := []promapi.Rule{{Group: "g", Name: "r", Type: "alerting", Query: `up == 0`}}
	dashboards := []grafana.Dashboard{{
		UID: "d", Title: "D",
		Queries: []string{`rate(dashboard_only_metric[5m])`},
	}}
	reading := &querylog.Reading{Queries: []string{"logged_only_metric"}, Span: 720 * time.Hour}

	src := corpus.Sources{
		Rules: rules, Dashboards: dashboards, QueryLog: reading,
		LogQualifies: true, DashboardsConfigured: true, DashboardsReachable: true,
	}
	allMetrics := []string{"up", "dashboard_only_metric", "logged_only_metric", "unread_metric"}
	c := corpus.Build(src, allMetrics)
	// Sanity: this really is a mixed corpus -- one query from each source.
	if c.Queries != 3 {
		t.Fatalf("test setup: c.Queries = %d, want 3 (one rule, one dashboard panel, one logged query)", c.Queries)
	}

	inv := inventory.Inventory{Metrics: []inventory.Metric{{Name: "unread_metric", Series: 1}}, TotalSeries: 1}
	var b bytes.Buffer
	Scan(&b, inv, c, verdict.Compute(inv, c))
	out := b.String()

	if !strings.Contains(out, "Queries    3 read from rules, dashboards and query log") {
		t.Errorf("header does not attribute queries to all three sources:\n%s", out)
	}
	if strings.Contains(out, "Queries    3 read from rules\n") {
		t.Errorf("header still attributes every query to rules alone:\n%s", out)
	}
}
