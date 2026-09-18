//go:build integration

package fixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/emit"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
	"github.com/SaiPisey2/jetsam/internal/report"
)

// loadgenMetric is the fixture's own consumer metric: 400 series, 5 distinct
// path values, exactly one of which anything reads (see
// fixture/loadgen/main.go's own doc comment). It is the only metric in this
// stack an aggregation proposal can be checked against, because no vendored
// metric has a known, exact cardinality by construction.
const loadgenMetric = "jetsam_demo_requests_total"

// rulesOnlySources returns the live inventory together with a corpus.Sources
// carrying only the real rules Prometheus evaluates -- no dashboards, no
// query log. It exists for the same reason TestRulesAloneLeaveTheDashboard-
// CanariesUnreferenced and withholdingBaseline build their own restricted
// Sources in verdict_test.go rather than trusting liveSources() directly:
// the live dashboard and the live query log both also read loadgenMetric
// (see VENDOR.md's "contaminated" section), so a test that wants to isolate
// what the RULE consumer alone licenses has to exclude both.
func rulesOnlySources(t *testing.T) (inventory.Inventory, corpus.Sources) {
	t.Helper()
	inv, src := liveSources(t)
	src.Dashboards = nil
	src.DashboardsConfigured = false
	src.DashboardsReachable = false
	src.QueryLog = nil
	src.LogQualifies = false
	return inv, src
}

// proposalFor and refusalFor mirror verdictFor in verdict_test.go: failing
// loudly when the named metric is absent, rather than letting a caller read
// "not proposed" and "never appeared in the input at all" as the same thing.
func proposalFor(t *testing.T, proposals []aggregate.Proposal, metric string) aggregate.Proposal {
	t.Helper()
	for _, p := range proposals {
		if p.Metric == metric {
			return p
		}
	}
	t.Fatalf("%s has no proposal", metric)
	return aggregate.Proposal{}
}

func refusalFor(t *testing.T, refusals []aggregate.Refusal, metric string) aggregate.Refusal {
	t.Helper()
	for _, r := range refusals {
		if r.Metric == metric {
			return r
		}
	}
	t.Fatalf("%s was not refused", metric)
	return aggregate.Refusal{}
}

// consumersOf mirrors cmd/jetsam's own rulesReading: every rule whose query
// resolves to metric, via the same Extract+Resolve pair corpus.Build itself
// uses to decide FromRule. Duplicated here rather than imported because
// rulesReading lives in package main, which nothing outside it can import.
func consumersOf(metric string, rules []promapi.Rule, allMetrics []string) []aggregate.Consumer {
	var out []aggregate.Consumer
	for _, r := range rules {
		refs, err := corpus.Extract(r.Query)
		if err != nil {
			continue
		}
		for _, m := range refs.Resolve(allMetrics) {
			if m == metric {
				out = append(out, aggregate.Consumer{Kind: "rule", Group: r.Group, Name: r.Name, Query: r.Query})
				break
			}
		}
	}
	return out
}

// instantSample is one entry of an /api/v1/query vector result.
type instantSample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// queryInstant issues one instant query against the live Prometheus, at at
// (or "now" when at is zero), and returns its vector result via the same
// get() helper stack_test.go's own tests use.
//
// query is trusted to already carry promapi.IgnoreUsageLabel on any
// selector of loadgenMetric it builds -- see queryCount and the equivalence
// check below for why: Prometheus logs every /api/v1/query call, and an
// untagged read of loadgenMetric issued BY THIS TEST would land in
// fixture/querylog/queries.log as an ordinary httpRequest entry, indistin-
// guishable from a real one, and skew what a later run of this same suite
// -- or TestAggregateRefusalTracksDashboardPresence, if the log ever stops
// being excluded from it -- finds when it reads that log back.
func queryInstant(t *testing.T, query string, at time.Time) []instantSample {
	t.Helper()
	v := url.Values{"query": {query}}
	if !at.IsZero() {
		// Full sub-second precision, not a rounded-down Unix second: the
		// equivalence check below needs both queries evaluated at exactly
		// the recording rule's own lastEvaluation instant, and rounding
		// that down could cross a 5s scrape boundary the rule's own
		// evaluation was on the near side of, reading one fewer sample.
		v.Set("time", strconv.FormatFloat(float64(at.UnixNano())/1e9, 'f', -1, 64))
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []instantSample `json:"result"`
		} `json:"data"`
	}
	get(t, "/api/v1/query?"+v.Encode(), &body)
	if body.Status != "success" {
		t.Fatalf("query %q: %s", query, body.Error)
	}
	return body.Data.Result
}

// mustFloat parses one /api/v1/query sample value -- always a JSON string,
// per the Prometheus HTTP API -- as a float64.
func mustFloat(t *testing.T, v any) float64 {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("sample value %v (%T) is not a string", v, v)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("sample value %q does not parse as a float: %v", s, err)
	}
	return f
}

// selectorTagged builds {__name__="metric", __ignore_usage__=""} -- the same
// shape production's measureKeptSeries and promapi.QueryJobsFor build --
// substituted wherever a query below reads loadgenMetric directly.
func selectorTagged(metric string) string {
	return fmt.Sprintf(`{__name__=%q, %s=""}`, metric, promapi.IgnoreUsageLabel)
}

// queryCount issues one instant query counting how many series match
// query and returns it as an int, failing if the result is not the single
// scalar-shaped vector a count(...) query always produces.
func queryCount(t *testing.T, query string) int {
	t.Helper()
	res := queryInstant(t, query, time.Time{})
	if len(res) != 1 {
		t.Fatalf("count query %q returned %d results, want exactly 1", query, len(res))
	}
	n := int(mustFloat(t, res[0].Value[1]))
	return n
}

// TestAggregateProposesLoadgenWithMeasuredNumbers is the brief's first
// acceptance bullet: loadgenMetric collapses to (path), and the raw and
// kept series counts the printed report states are numbers THIS TEST
// measured independently against the live Prometheus, not constants baked
// into the assertion. A hardcoded "400 series -> 5 kept" would go on
// passing even if aggregate.Decide's own series accounting broke, as long
// as jetsam and the test happened to agree on the same wrong number; a
// measured "raw" and "kept" cannot agree with a wrong report for the wrong
// reason, because nothing here tells them what to agree on. A metric name
// or a series count deleted from writeFinding's headline line, or Decide
// silently changing which label loadgenMetric collapses to, would each
// break the wantLine match below.
func TestAggregateProposesLoadgenWithMeasuredNumbers(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	cor := corpus.Build(src, names)

	proposals, _ := aggregate.Decide(inv, cor, nil)
	p := proposalFor(t, proposals, loadgenMetric)
	if p.Op != "sum" || len(p.Keep) != 1 || p.Keep[0] != "path" {
		t.Fatalf("unexpected proposal shape, test assumptions are stale: %+v", p)
	}

	raw := queryCount(t, "count("+selectorTagged(loadgenMetric)+")")
	kept := queryCount(t, fmt.Sprintf("count(count by (%s) (%s))", strings.Join(p.Keep, ", "), selectorTagged(loadgenMetric)))
	// This is the correctness floor emit/decide itself enforces (see
	// cmd/jetsam's withholdReason): a proposal that does not actually
	// reduce the series count is not a real proposal at all. Asserting it
	// here, independently measured, is what makes the "collapsing 400 to
	// 5" claim below meaningful rather than an assumption.
	if kept >= raw {
		t.Fatalf("measured kept series (%d) is not fewer than raw series (%d) -- this fixture no longer demonstrates a real reduction", kept, raw)
	}
	p.RawSeries = raw
	p.KeptSeries = kept

	ruleYAML, err := emit.RenderRule("", p)
	if err != nil {
		t.Fatalf("RenderRule: %v", err)
	}
	var consumers []report.ConsumerFinding
	for _, c := range consumersOf(loadgenMetric, src.Rules, names) {
		rewritten, declined, err := emit.RewriteConsumer(c.Query, p)
		if err != nil {
			t.Fatalf("RewriteConsumer: %v", err)
		}
		consumers = append(consumers, report.ConsumerFinding{Consumer: c, Rewritten: rewritten, Declined: declined})
	}

	var buf bytes.Buffer
	report.Aggregate(&buf, []report.AggregateFinding{{Proposal: p, Rule: ruleYAML, Consumers: consumers}}, nil, nil)
	out := buf.String()

	wantLine := fmt.Sprintf("%s: %d series -> %d kept (saves %d)", loadgenMetric, raw, kept, raw-kept)
	if !strings.Contains(out, wantLine) {
		t.Errorf("report does not state the measured numbers:\nwant substring: %s\ngot:\n%s", wantLine, out)
	}
}

// TestAggregateRefusalTracksDashboardPresence is the brief's second
// acceptance bullet: the same metric, the same real rules, decided twice --
// once with the live dashboards in the corpus, once without -- to show that
// the dashboard specifically is what turns the proposal into a refusal,
// the same isolation discipline TestDashboardsAloneFlipTheDashboardCanaries
// already uses in verdict_test.go for the drop path.
//
// What would make this fail: removing decide.go's
// `if c.UsedBy[metric]&corpus.FromDashboard != 0` check would let the "with
// dashboards" half propose loadgenMetric instead of refusing it -- exactly
// the safety property this bullet exists to pin.
func TestAggregateRefusalTracksDashboardPresence(t *testing.T) {
	inv, fullSrc := liveSources(t)
	names := metricNames(inv)
	// This runs against the FULL live corpus, query log included -- no
	// exclusion. Every query anything under fixture/ issues against
	// jetsam_demo_requests_total is tagged with promapi.IgnoreUsageLabel
	// (see selectorTagged and TestFixtureQueriesLeaveNoUntaggedTraceOfLoadgen),
	// except fixture/query.sh's own deliberate, untagged read, which
	// agrees with LoadgenPathErrors' operator and grouping -- so the log
	// has nothing left in it that could make this metric disagree with
	// itself regardless of dashboards.

	withDash := corpus.Build(fullSrc, names)
	_, refusals := aggregate.Decide(inv, withDash, nil)
	r := refusalFor(t, refusals, loadgenMetric)
	if !strings.Contains(r.Reason, "dashboard") {
		t.Errorf("refusal reason does not mention a dashboard: %q", r.Reason)
	}

	noDashSrc := fullSrc
	noDashSrc.Dashboards = nil
	noDashSrc.DashboardsConfigured = false
	noDashSrc.DashboardsReachable = false
	withoutDash := corpus.Build(noDashSrc, names)
	proposals, _ := aggregate.Decide(inv, withoutDash, nil)
	proposalFor(t, proposals, loadgenMetric)
}

// TestAggregateRefusesATopkConsumerDespiteEmptyGrouping is the brief's third
// bullet. topk(3, m) parses to an AggregateExpr with an EMPTY Grouping --
// indistinguishable by shape from sum(m), which collapses to nothing -- but
// topk hands back the raw series with every label intact. See
// internal/corpus/labels.go's preservesEverySeries doc comment for the trap
// this exists to catch.
//
// What would make this fail: removing parser.TOPK from labels.go's
// preservesEverySeries map would make topk's empty Grouping read as "needs
// no label," and loadgenMetric would be proposed with Keep empty instead
// of refused.
func TestAggregateRefusesATopkConsumerDespiteEmptyGrouping(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	src.Rules = []promapi.Rule{
		{Group: "synthetic", Name: "TopPaths", Type: "recording", Query: fmt.Sprintf("topk(3, %s)", loadgenMetric)},
	}
	cor := corpus.Build(src, names)

	_, refusals := aggregate.Decide(inv, cor, nil)
	r := refusalFor(t, refusals, loadgenMetric)
	if !strings.Contains(r.Reason, "every label") {
		t.Errorf("topk refusal does not say every label must survive: %q", r.Reason)
	}
}

// TestAggregateRefusesWhenConsumersDisagreeOnOperator is the brief's fourth
// bullet: two separate rule consumers, one summing loadgenMetric by path and
// one taking its min by path, must refuse rather than let jetsam guess which
// operator to trust.
func TestAggregateRefusesWhenConsumersDisagreeOnOperator(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	src.Rules = []promapi.Rule{
		{Group: "synthetic", Name: "SumByPath", Type: "recording", Query: fmt.Sprintf("sum by (path) (%s)", loadgenMetric)},
		{Group: "synthetic", Name: "MinByPath", Type: "recording", Query: fmt.Sprintf("min by (path) (%s)", loadgenMetric)},
	}
	cor := corpus.Build(src, names)

	_, refusals := aggregate.Decide(inv, cor, nil)
	r := refusalFor(t, refusals, loadgenMetric)
	if !strings.Contains(r.Reason, "disagree") {
		t.Errorf("refusal does not mention the consumers' disagreement: %q", r.Reason)
	}
}

// TestAggregateRefusesABareSelectorFromTheQueryLogAndNamesIt is the brief's
// fifth bullet. `{job="loadgen"}` names no metric at all -- Extract's own
// MatchesEverything case -- so corpus.Build conservatively charges it
// against every metric in the inventory, including loadgenMetric, and
// LabelsNeeded finds no selector of loadgenMetric that this query's shape
// actually touches. foldNeeds' own !Touches branch is what turns that
// "could not confirm" into a refusal that names the query verbatim, rather
// than the generic "some consumer needs every label" a real topk or bare
// unaggregated selector produces (see labels.go's Blocker doc comment for
// why those two cases are told apart).
func TestAggregateRefusesABareSelectorFromTheQueryLogAndNamesIt(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	const bareQuery = `{job="loadgen"}`
	src.QueryLog = &querylog.Reading{Queries: []string{bareQuery}}
	src.LogQualifies = true
	cor := corpus.Build(src, names)

	_, refusals := aggregate.Decide(inv, cor, nil)
	r := refusalFor(t, refusals, loadgenMetric)
	if !strings.Contains(r.Reason, bareQuery) {
		t.Errorf("refusal does not name the bare selector that forced it: %q", r.Reason)
	}
}

// TestAggregateReportAppliesNothingEvenWhenEveryMetricIsRefused is the
// brief's sixth bullet: report.Aggregate's own doc comment names this
// failure mode directly -- a run where every metric was refused must still
// carry the not-applied notice, not silently drop it because there was
// nothing else to say. This reuses the topk scenario above because it is
// the simplest source of a corpus with exactly one Needs entry and zero
// proposals: with src.Rules holding only the topk consumer, no other metric
// in the whole live inventory has a Needs entry at all, so proposals is
// unconditionally empty and refusals unconditionally non-empty -- "every
// metric refused" by construction, not by coincidence of what the live
// stack happens to contain today.
func TestAggregateReportAppliesNothingEvenWhenEveryMetricIsRefused(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	src.Rules = []promapi.Rule{
		{Group: "synthetic", Name: "TopPaths", Type: "recording", Query: fmt.Sprintf("topk(3, %s)", loadgenMetric)},
	}
	cor := corpus.Build(src, names)

	proposals, refusals := aggregate.Decide(inv, cor, nil)
	if len(proposals) != 0 {
		t.Fatalf("setup invalid: want zero proposals with only a topk consumer in the corpus, got %d", len(proposals))
	}
	if len(refusals) == 0 {
		t.Fatal("setup invalid: want at least one refusal")
	}

	var buf bytes.Buffer
	report.Aggregate(&buf, nil, nil, refusals)
	if !strings.Contains(buf.String(), "Nothing here is applied") {
		t.Errorf("report drops the not-applied notice when every metric was refused:\n%s", buf.String())
	}
}

// TestAggregateRewriteKeepsTheAlertingThreshold is the brief's seventh
// bullet, checked against the real LoadgenPathErrors alerting rule
// fixture/prometheus/rules/local.yaml adds to the live stack: RewriteConsumer
// must replace only the aggregate subtree it recognises inside
// `sum by (path) (rate(jetsam_demo_requests_total[5m])) > 100` and leave the
// comparison against 100 standing.
//
// Mutation-checked: temporarily changing internal/emit/rewrite.go's matched
// branch to record span{0, len(query)} instead of the AggregateExpr's own
// PositionRange -- replacing the whole query text rather than just the
// subtree jetsam recognises -- makes this test fail with rewritten equal to
// the bare rule name and no "> 100" left at all. Confirmed by making that
// edit, running this test, watching it fail, and reverting it.
func TestAggregateRewriteKeepsTheAlertingThreshold(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	cor := corpus.Build(src, names)

	proposals, _ := aggregate.Decide(inv, cor, nil)
	p := proposalFor(t, proposals, loadgenMetric)

	consumers := consumersOf(loadgenMetric, src.Rules, names)
	if len(consumers) != 1 {
		t.Fatalf("expected exactly one live rule consumer of %s, got %d: %+v", loadgenMetric, len(consumers), consumers)
	}
	c := consumers[0]
	if !strings.Contains(c.Query, "> 100") {
		t.Fatalf("test assumptions are stale: local.yaml's consumer query no longer contains \"> 100\": %q", c.Query)
	}

	rewritten, declined, err := emit.RewriteConsumer(c.Query, p)
	if err != nil {
		t.Fatalf("RewriteConsumer: %v", err)
	}
	if declined != "" {
		t.Errorf("consumer was declined, want a clean rewrite: %q", declined)
	}
	want := p.RuleName + " > 100"
	if rewritten != want {
		t.Errorf("rewritten = %q, want %q", rewritten, want)
	}
}

// promReload triggers Prometheus to reload its configuration and rule
// files. It requires --web.enable-lifecycle, which fixture/docker-compose.yml
// sets specifically for this test.
func promReload(t *testing.T) {
	t.Helper()
	resp, err := http.Post(promURL+"/-/reload", "text/plain", nil)
	if err != nil {
		t.Fatalf("reload prometheus, run `make demo-up`: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("reload prometheus: status %d: %s (is --web.enable-lifecycle set?)", resp.StatusCode, body)
	}
}

// ruleTotals reports how many groups and rules /api/v1/rules currently
// lists, regardless of which files they came from -- used by the
// equivalence check below to prove the stack returns to exactly its
// pre-test shape once its scratch rule file is removed.
func ruleTotals(t *testing.T) (groups, rules int) {
	t.Helper()
	var body struct {
		Data struct {
			Groups []struct {
				Rules []struct{} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	get(t, "/api/v1/rules", &body)
	groups = len(body.Data.Groups)
	for _, g := range body.Data.Groups {
		rules += len(g.Rules)
	}
	return groups, rules
}

// waitForTwoRuleEvaluations polls /api/v1/rules until the named rule, in the
// named group, has reported at least two DISTINCT lastEvaluation timestamps
// -- proof it has actually run, more than once, since the reload that
// picked it up, rather than a blind sleep sized to the evaluation interval
// and hoping -- and returns the most recent of those timestamps.
//
// That returned instant is deliberately what the caller then queries BOTH
// expressions at (see TestAggregateRuleEquivalesTheOriginalAcrossTheRule-
// Boundary), rather than "now": jetsam_demo_requests_total is still very
// young when this test runs moments after `make demo-up`, and Prometheus's
// rate() divides by the full requested window regardless of how much of it
// is actually populated, so its value measurably DRIFTS as more history
// accumulates -- querying "now" for one side and a discrete evaluation
// instant up to 15s earlier for the other compares two different points on
// that drift curve, not the same computation twice. Querying both
// expressions at the identical instant removes the drift entirely, because
// it is then a function of (instant, stored samples) alone, which both
// queries share.
func waitForTwoRuleEvaluations(t *testing.T, group, rule string) time.Time {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	seen := map[string]bool{}
	var latest time.Time
	for {
		func() {
			resp, err := http.Get(promURL + "/api/v1/rules")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var body struct {
				Data struct {
					Groups []struct {
						Name  string `json:"name"`
						Rules []struct {
							Name           string `json:"name"`
							LastEvaluation string `json:"lastEvaluation"`
						} `json:"rules"`
					} `json:"groups"`
				} `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) != nil {
				return
			}
			for _, g := range body.Data.Groups {
				if g.Name != group {
					continue
				}
				for _, r := range g.Rules {
					if r.Name != rule || r.LastEvaluation == "" {
						continue
					}
					seen[r.LastEvaluation] = true
					if ts, err := time.Parse(time.RFC3339Nano, r.LastEvaluation); err == nil && ts.After(latest) {
						latest = ts
					}
				}
			}
		}()
		if len(seen) >= 2 {
			return latest
		}
		if time.Now().After(deadline) {
			t.Fatalf("rule %s/%s did not report two distinct evaluations within the deadline (%d seen)", group, rule, len(seen))
		}
		time.Sleep(3 * time.Second)
	}
}

// equivalenceRulePath is the scratch rule file this check writes into the
// live stack's rules directory and removes again in its own t.Cleanup
// below. It is deliberately not gitignored: the test guarantees its own
// removal, and a stray copy left behind by a run that did not clean up
// after itself is meant to be loud (git status dirty), not silently
// tolerated by an ignore rule that would hide exactly that failure.
const equivalenceRulePath = "prometheus/rules/equivalence-check.yaml"

// TestAggregateRuleEquivalesTheOriginalAcrossTheRuleBoundary is the check
// the brief calls the one that matters. Comparing the rewritten consumer's
// text against the ORIGINAL consumer's text proves nothing here: after the
// rate-vs-counter change (see aggregate.Proposal's own doc comment) the two
// are the same string up to the recorded name, and the recorded name
// returns no data until the rule exists to write it. The only comparison
// that actually exercises the rule jetsam rendered is one that crosses the
// rule boundary:
//
//  1. render the recording rule and write it into a file the live
//     Prometheus already globs (fixture/prometheus/prometheus.yml's
//     rule_files),
//  2. reload, and wait for the rule to actually evaluate at least twice,
//  3. query the recorded series and the original expression at the SAME
//     instant, independently, and compare per path.
//
// "The same instant" is not "now" for one side and "whenever the rule last
// ran" for the other: this test queries BOTH expressions at the recording
// rule's own lastEvaluation timestamp (returned by waitForTwoRuleEvaluations
// -- see its own doc comment for why "now" does not work here). A
// Prometheus instant query is a pure function of (expression, time, stored
// samples); querying the raw expression at exactly the timestamp the rule
// itself last evaluated at reads the identical range of underlying samples
// the rule's own evaluation read, so the two values should agree to within
// floating-point/string round-tripping, not "close enough given staleness."
//
// Tolerance: 0.1% relative, to absorb exactly that round-tripping (a
// recorded sample is a float64 written once; the query result is the same
// computation re-run and then formatted back through Prometheus's own
// %v-style float encoding in its JSON API) and nothing else. It is still
// wide enough to say nothing useful passed by accident: a swapped operator,
// a dropped label, half the series missing from the aggregation, or a
// materially wrong window would each move a path's value by many multiples
// of 0.1%, not less.
//
// Mutation-checked: temporarily hardcoding renderExpr's outer operator to
// "min" regardless of p.Op (internal/emit/record.go) makes this test fail
// loudly -- min by (path) of these series is a small fraction of their sum,
// nowhere near 0.1% -- while leaving the pre-existing report-level
// assertions untouched, because report.Aggregate only ever prints back
// whatever Proposal and RenderRule already agreed on. Confirmed by making
// that edit, running this test, watching it fail with a large per-path
// disagreement, and reverting it.
func TestAggregateRuleEquivalesTheOriginalAcrossTheRuleBoundary(t *testing.T) {
	inv, src := rulesOnlySources(t)
	names := metricNames(inv)
	cor := corpus.Build(src, names)

	proposals, _ := aggregate.Decide(inv, cor, nil)
	p := proposalFor(t, proposals, loadgenMetric)
	if p.Op != "sum" || p.Fn != "rate" || p.Window != "5m" || len(p.Keep) != 1 || p.Keep[0] != "path" {
		t.Fatalf("proposal shape changed, this test's setup is stale: %+v", p)
	}

	ruleYAML, err := emit.RenderRule("", p)
	if err != nil {
		t.Fatalf("RenderRule: %v", err)
	}

	beforeGroups, beforeRules := ruleTotals(t)
	if err := os.WriteFile(equivalenceRulePath, []byte(ruleYAML), 0o644); err != nil {
		t.Fatalf("write scratch rule file: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(equivalenceRulePath); err != nil {
			t.Errorf("remove scratch rule file: %v", err)
		}
		promReload(t)
		afterGroups, afterRules := ruleTotals(t)
		if afterGroups != beforeGroups || afterRules != beforeRules {
			t.Errorf("prometheus did not return to its pre-test rule count after cleanup: %d groups / %d rules, want %d / %d",
				afterGroups, afterRules, beforeGroups, beforeRules)
		}
	})

	promReload(t)
	// "jetsam" is the group name emit.RenderRule invents for a rule file
	// that started with none -- see groupRules' own doc comment. at is the
	// rule's own most recent evaluation instant, not time.Now() -- see
	// waitForTwoRuleEvaluations' and this test's own doc comments for why.
	at := waitForTwoRuleEvaluations(t, "jetsam", p.RuleName)

	original := fmt.Sprintf("sum by (path) (rate(%s[5m]))", selectorTagged(loadgenMetric))
	recorded := selectorTagged(p.RuleName)

	origSamples := queryInstant(t, original, at)
	recSamples := queryInstant(t, recorded, at)
	if len(origSamples) == 0 || len(recSamples) == 0 {
		t.Fatalf("expected samples from both queries at %v, got %d original / %d recorded", at, len(origSamples), len(recSamples))
	}

	orig := map[string]float64{}
	for _, s := range origSamples {
		orig[s.Metric["path"]] = mustFloat(t, s.Value[1])
	}
	rec := map[string]float64{}
	for _, s := range recSamples {
		rec[s.Metric["path"]] = mustFloat(t, s.Value[1])
	}
	if len(orig) != len(rec) {
		t.Fatalf("original expression has %d paths, recorded series has %d: orig=%v rec=%v", len(orig), len(rec), orig, rec)
	}

	const relTolerance = 0.001 // see this test's own doc comment for why
	for path, want := range orig {
		got, ok := rec[path]
		if !ok {
			t.Errorf("path %q is present in the original expression but missing from the recorded series", path)
			continue
		}
		diff := math.Abs(got - want)
		if want == 0 {
			if diff > 0 {
				t.Errorf("path %q: recorded %.6f, original 0, want equal", path, got)
			}
			continue
		}
		if rel := diff / math.Abs(want); rel > relTolerance {
			t.Errorf("path %q: recorded %.4f, original %.4f, differ by %.2f%%, want within %.0f%%",
				path, got, want, 100*rel, 100*relTolerance)
		}
	}
	t.Logf("equivalence check at %v: %d path(s) compared, original=%v recorded=%v", at, len(orig), orig, rec)
}
