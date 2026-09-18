package emit

import (
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"
)

// parsedRule is one groups[].rules[] entry, read back off the rendered
// document rather than off its text -- the same discipline relabel_test.go
// uses for metric_relabel_configs, and for the same reason: a rule that
// merely CONTAINS the right substrings could still be sitting next to
// structure an injection added or removed.
type parsedRule struct {
	Record, Expr string
}

// parsedRules walks ruleYAML's groups[].rules[] and returns every entry it
// finds, failing the test outright if a rule is not a mapping at all --
// that shape mismatch is what a successful YAML injection would produce.
func parsedRules(t *testing.T, ruleYAML string) []parsedRule {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(ruleYAML), &doc); err != nil {
		t.Fatalf("rendered rule file does not parse: %v\n%s", err, ruleYAML)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("rendered rule file is empty")
	}
	root := doc.Content[0]
	groups := mappingValue(root, "groups")
	if groups == nil {
		t.Fatalf("rendered rule file has no groups:\n%s", ruleYAML)
	}
	var out []parsedRule
	for _, g := range groups.Content {
		rules := mappingValue(g, "rules")
		if rules == nil {
			continue
		}
		for _, r := range rules.Content {
			if r.Kind != yaml.MappingNode {
				t.Fatalf("rule is not a mapping (kind=%v) -- possible YAML injection:\n%s", r.Kind, ruleYAML)
			}
			out = append(out, parsedRule{
				Record: scalarValue(mappingValue(r, "record")),
				Expr:   scalarValue(mappingValue(r, "expr")),
			})
		}
	}
	return out
}

// findRule returns the one parsed rule named record, failing the test if
// there is not exactly one.
func findRule(t *testing.T, ruleYAML, record string) parsedRule {
	t.Helper()
	var matches []parsedRule
	for _, r := range parsedRules(t, ruleYAML) {
		if r.Record == record {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("got %d rules named %q, want 1:\n%s", len(matches), record, ruleYAML)
	}
	return matches[0]
}

// mustParsePromQL fails the test unless expr parses with the same parser
// jetsam uses everywhere else, proving the rendered rule is not merely
// well-formed YAML but a query Prometheus can actually load and evaluate.
func mustParsePromQL(t *testing.T, expr string) {
	t.Helper()
	if _, err := parser.NewParser(parser.Options{}).ParseExpr(expr); err != nil {
		t.Fatalf("rendered expr %q does not parse as PromQL: %v", expr, err)
	}
}

func TestRenderRuleBuildsGroupsFromAnEmptyFile(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Keep:     []string{"path"},
		Op:       "sum",
		Fn:       "rate",
		Window:   "5m",
		RuleName: "path:m_total:sum_rate5m",
	}
	got, err := RenderRule("", p)
	if err != nil {
		t.Fatalf("RenderRule: %v", err)
	}
	rule := findRule(t, got, p.RuleName)
	const want = "sum by (path) (rate(m_total[5m]))"
	if rule.Expr != want {
		t.Fatalf("expr = %q, want %q", rule.Expr, want)
	}
	mustParsePromQL(t, rule.Expr)
}

// TestRenderRuleFnEmptyRecordsTheMetricNotARate covers the case where no
// consumer applies rate/irate/increase: the rule records the metric's own
// aggregation, with no range function wrapped around it.
func TestRenderRuleFnEmptyRecordsTheMetricNotARate(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Keep:     []string{"path"},
		Op:       "sum",
		RuleName: "path:m_total:sum",
	}
	got, err := RenderRule("", p)
	if err != nil {
		t.Fatalf("RenderRule: %v", err)
	}
	rule := findRule(t, got, p.RuleName)
	const want = "sum by (path) (m_total)"
	if rule.Expr != want {
		t.Fatalf("expr = %q, want %q", rule.Expr, want)
	}
	mustParsePromQL(t, rule.Expr)
}

// TestRenderRuleKeepEmptyOmitsTheByClause covers the case where every
// consumer collapses the metric to a single series: Keep is empty, and the
// grouping clause must be OMITTED entirely -- "sum(...)", never the
// meaningless "sum by () (...)".
func TestRenderRuleKeepEmptyOmitsTheByClause(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Op:       "sum",
		Fn:       "rate",
		Window:   "5m",
		RuleName: ":m_total:sum_rate5m",
	}
	got, err := RenderRule("", p)
	if err != nil {
		t.Fatalf("RenderRule: %v", err)
	}
	rule := findRule(t, got, p.RuleName)
	const want = "sum(rate(m_total[5m]))"
	if rule.Expr != want {
		t.Fatalf("expr = %q, want %q", rule.Expr, want)
	}
	if strings.Contains(rule.Expr, "by") {
		t.Fatalf("expr = %q, must not contain a by clause when Keep is empty", rule.Expr)
	}
	mustParsePromQL(t, rule.Expr)
}

const sampleRuleYAML = `groups:
  - name: existing
    rules:
      # an operator's own rule, unrelated to anything jetsam adds
      - record: instance:up:count
        expr: count by (instance) (up)
`

// TestRenderRuleAddsToAnExistingGroupWithoutDisturbingIt asks RenderRule to
// add a rule to a file that already has a group with its own rule in it.
// The pre-existing rule, and its comment, must survive untouched.
func TestRenderRuleAddsToAnExistingGroupWithoutDisturbingIt(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Keep:     []string{"path"},
		Op:       "sum",
		Fn:       "rate",
		Window:   "5m",
		RuleName: "path:m_total:sum_rate5m",
	}
	got, err := RenderRule(sampleRuleYAML, p)
	if err != nil {
		t.Fatalf("RenderRule: %v", err)
	}
	if !strings.Contains(got, "an operator's own rule") {
		t.Fatalf("pre-existing comment lost:\n%s", got)
	}
	existing := findRule(t, got, "instance:up:count")
	if existing.Expr != "count by (instance) (up)" {
		t.Fatalf("pre-existing rule was disturbed: expr = %q", existing.Expr)
	}
	added := findRule(t, got, p.RuleName)
	if added.Expr != "sum by (path) (rate(m_total[5m]))" {
		t.Fatalf("added rule expr = %q", added.Expr)
	}
}

// TestRenderRuleRefusesANameCollision plants a rule already named what the
// proposal would name its own rule, and expects a refusal rather than a
// silent overwrite -- overwriting an existing rule of the same name could
// silently replace something an operator wrote by hand, or something an
// earlier jetsam run already recorded with different inputs.
func TestRenderRuleRefusesANameCollision(t *testing.T) {
	p := aggregate.Proposal{
		Metric:   "m_total",
		Keep:     []string{"instance"},
		Op:       "count",
		RuleName: "instance:up:count",
	}
	if _, err := RenderRule(sampleRuleYAML, p); err == nil {
		t.Fatal("RenderRule accepted a rule name that already exists, want an error")
	}
}

// TestRenderRuleRejectsAHostileMetricName feeds metric names that, embedded
// directly into the PromQL text this package builds, would break out of
// what looks like a metric name and inject PromQL syntax of their own. Each
// is a perfectly quotable YAML scalar -- the YAML side of this is not the
// problem -- but an unparseable expr, which is why RenderRule must parse
// what it renders and refuse rather than write it.
func TestRenderRuleRejectsAHostileMetricName(t *testing.T) {
	cases := []string{
		`m_total"injected`,
		"m_total\ninjected",
		"m_total]",
	}
	for _, metric := range cases {
		t.Run(metric, func(t *testing.T) {
			p := aggregate.Proposal{
				Metric:   metric,
				Op:       "sum",
				RuleName: "x:m:sum",
			}
			if _, err := RenderRule("", p); err == nil {
				t.Fatalf("RenderRule accepted hostile metric name %q, want an error", metric)
			}
		})
	}
}

// TestRenderRuleRejectsAHostileRuleName mirrors the metric-name case for the
// record field itself: a record name is written as a plain YAML scalar, so
// a quote or a newline cannot break out of ITS scalar either, but a name
// that does not read back as a bare metric selector naming itself is a rule
// Prometheus will refuse to load -- taking every other rule already in the
// file down with it.
func TestRenderRuleRejectsAHostileRuleName(t *testing.T) {
	cases := []string{
		`path:m"injected`,
		"path:m\ninjected",
		`m_total{job="x"}`,
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			p := aggregate.Proposal{
				Metric:   "m_total",
				Op:       "sum",
				RuleName: name,
			}
			if _, err := RenderRule("", p); err == nil {
				t.Fatalf("RenderRule accepted hostile rule name %q, want an error", name)
			}
		})
	}
}

// TestRenderRuleExpressionsRoundTripThroughPromQL renders a spread of
// proposals covering every shape RenderRule can produce (Fn set or empty,
// Keep populated or empty, multiple kept labels) and re-parses each result
// with the PromQL parser directly -- independent of whatever check
// RenderRule performs internally -- so this test still catches a
// regression even if that internal check is ever weakened.
func TestRenderRuleExpressionsRoundTripThroughPromQL(t *testing.T) {
	proposals := []aggregate.Proposal{
		{Metric: "m_total", Keep: []string{"path"}, Op: "sum", Fn: "rate", Window: "5m", RuleName: "path:m_total:sum_rate5m"},
		{Metric: "m_total", Keep: []string{"path"}, Op: "sum", RuleName: "path:m_total:sum"},
		{Metric: "m_total", Op: "sum", Fn: "rate", Window: "5m", RuleName: ":m_total:sum_rate5m"},
		{Metric: "m_total", Keep: []string{"path", "status"}, Op: "max", Fn: "irate", Window: "1m", RuleName: "path_status:m_total:max_irate1m"},
		{Metric: "m_total", Keep: []string{"path"}, Op: "min", Fn: "increase", Window: "10m", RuleName: "path:m_total:min_increase10m"},
	}
	for _, p := range proposals {
		got, err := RenderRule("", p)
		if err != nil {
			t.Fatalf("RenderRule(%+v): %v", p, err)
		}
		rule := findRule(t, got, p.RuleName)
		mustParsePromQL(t, rule.Expr)
	}
}
