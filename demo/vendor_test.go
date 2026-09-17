package demo

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"
)

// Every count this fixture pins lives here. This is the only file in
// package demo with no build tag, so the integration-tagged files
// (stack_test.go, verdict_test.go) compile against these same constants.
// The numbers used to be spelled out independently in four files, which
// made VENDOR.md's bump procedure -- "update the counts here and in
// vendor_test.go" -- silently incomplete.
const (
	wantGroups    = 3
	wantRules     = 64
	wantRecording = 15
	wantAlerting  = 49

	wantPanelQueries = 284
	// wantParsingQueries is how many of those panel queries are valid
	// PromQL with no template substitution at all. See
	// TestVendoredDashboardCarriesTheTemplateVariableHazard.
	wantParsingQueries = 160
)

// vendoredRuleFiles is what VENDOR.md records for each vendored rule file.
// These are asserted rather than trusted: an upstream bump that changes
// them is a deliberate, reviewable event, not a silent one.
var vendoredRuleFiles = map[string]struct{ groups, rules, recording int }{
	"prometheus/rules/node-exporter.yaml": {groups: 2, rules: 41, recording: 15},
	"prometheus/rules/prometheus.yaml":    {groups: 1, rules: 23, recording: 0},
}

// ruleFile mirrors just enough of the plain Prometheus rule file shape --
// groups of rules, each either a recording rule (record) or an alerting
// rule (alert), each carrying a PromQL expr -- to count and validate
// without pulling in rulefmt itself.
type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Record string `yaml:"record"`
			Alert  string `yaml:"alert"`
			Expr   string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestVendoredRulesParseAndMatchVendorDoc(t *testing.T) {
	// Same parser construction as internal/corpus/refs.go: at
	// prometheus/prometheus v0.314.0 there is no package-level
	// parser.ParseExpr, and this is the exact parser that reads these
	// rules' expr fields in production.
	p := parser.NewParser(parser.Options{})
	for path, want := range vendoredRuleFiles {
		t.Run(path, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s does not parse as a Prometheus rule file: %v", path, err)
			}
			var rf ruleFile
			if err := yaml.Unmarshal(b, &rf); err != nil {
				t.Fatalf("%s does not parse as a Prometheus rule file: %v", path, err)
			}
			rules, recording := 0, 0
			for _, g := range rf.Groups {
				for _, r := range g.Rules {
					rules++
					if r.Record != "" {
						recording++
					}
					if _, err := p.ParseExpr(r.Expr); err != nil {
						name := r.Record
						if name == "" {
							name = r.Alert
						}
						t.Errorf("%s: rule %q in group %q has an expr that is not valid PromQL: %v",
							path, name, g.Name, err)
					}
				}
			}
			if len(rf.Groups) != want.groups || rules != want.rules || recording != want.recording {
				t.Errorf("%s has %d groups / %d rules / %d recording, want %d / %d / %d -- "+
					"upstream changed; re-run demo/vendor.sh and update demo/VENDOR.md deliberately",
					path, len(rf.Groups), rules, recording, want.groups, want.rules, want.recording)
			}
		})
	}

	// The per-file table and the pinned totals are two spellings of the
	// same numbers, and the build-tagged tests assert the totals against
	// the running stack. If a bump updates one and not the other, the
	// fixture disagrees with itself; say so here rather than letting a
	// stack test fail with no clue why.
	groups, rules, recording := 0, 0, 0
	for _, want := range vendoredRuleFiles {
		groups += want.groups
		rules += want.rules
		recording += want.recording
	}
	if groups != wantGroups || rules != wantRules || recording != wantRecording {
		t.Errorf("the per-file table sums to %d groups / %d rules / %d recording, "+
			"but the pinned totals are %d / %d / %d -- a bump updated one and not the other",
			groups, rules, recording, wantGroups, wantRules, wantRecording)
	}
	if wantRecording+wantAlerting != wantRules {
		t.Errorf("the pinned totals do not add up: %d recording + %d alerting != %d rules",
			wantRecording, wantAlerting, wantRules)
	}
}

// TestVendoredDashboardCarriesTheTemplateVariableHazard pins the finding
// that shapes sub-project B -- as a split, not as a slogan.
//
// All 284 panel queries contain a template variable, but that does not
// make them all unparseable. A `$` inside a label value (instance="$node")
// is perfectly good PromQL; only a `$` inside a duration
// ([$__rate_interval]) is a syntax error. Parsed with the same parser
// internal/corpus uses, 160 of the 284 parse and 124 fail.
//
// That split IS the hazard. A corpus reader with no substitution does not
// refuse the dashboard outright -- it silently recovers 160 queries, and
// the metric names in them, biased towards the panels that do not call
// rate(). A partial corpus that looks like it is working is more
// dangerous than uniform refusal, because nothing announces the gap.
//
// Asserting the split rather than "every query contains a $" is what lets
// this test notice the hazard DISAPPEARING: if upstream rewrote every
// [$__rate_interval] to [5m], the `$` assertion would still hold while
// the fixture quietly stopped containing the problem B exists to solve.
func TestVendoredDashboardCarriesTheTemplateVariableHazard(t *testing.T) {
	b, err := os.ReadFile("grafana/dashboards/node-exporter-full.json")
	if err != nil {
		t.Fatalf("read vendored dashboard: %v", err)
	}
	var dash struct {
		Panels []panel `json:"panels"`
	}
	if err := json.Unmarshal(b, &dash); err != nil {
		t.Fatalf("vendored dashboard is not valid JSON: %v", err)
	}
	var exprs []string
	collectExprs(dash.Panels, &exprs)

	if len(exprs) != wantPanelQueries {
		t.Errorf("dashboard has %d panel queries, want %d -- upstream revision changed; "+
			"re-run demo/vendor.sh and update demo/VENDOR.md deliberately", len(exprs), wantPanelQueries)
	}
	withVars := 0
	for _, e := range exprs {
		if strings.Contains(e, "$") {
			withVars++
		}
	}
	if withVars != len(exprs) {
		t.Errorf("%d of %d panel queries carry a template variable, want all of them", withVars, len(exprs))
	}

	p := parser.NewParser(parser.Options{})
	parses := 0
	for _, e := range exprs {
		if _, err := p.ParseExpr(e); err == nil {
			parses++
		}
	}
	if parses != wantParsingQueries {
		t.Errorf("%d of %d panel queries parse as PromQL with no substitution (%d fail), want %d / %d -- "+
			"if this went UP the fixture has lost the hazard sub-project B's substitution layer exists for; "+
			"re-run demo/vendor.sh and update demo/VENDOR.md deliberately",
			parses, len(exprs), len(exprs)-parses, wantParsingQueries, wantPanelQueries-wantParsingQueries)
	}
}

type panel struct {
	Panels  []panel `json:"panels"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

// collectExprs walks nested panels: Grafana rows carry their own panels,
// so a flat pass over the top level misses most of the queries.
func collectExprs(panels []panel, out *[]string) {
	for _, p := range panels {
		for _, t := range p.Targets {
			if t.Expr != "" {
				*out = append(*out, t.Expr)
			}
		}
		collectExprs(p.Panels, out)
	}
}
