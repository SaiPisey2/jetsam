package demo

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/promql/parser"
)

// wantRules is what VENDOR.md records for each vendored rule file. These
// are asserted rather than trusted: an upstream bump that changes them is
// a deliberate, reviewable event, not a silent one.
var wantRules = map[string]struct{ groups, rules, recording int }{
	"prometheus/rules/node-exporter.yaml": {groups: 2, rules: 41, recording: 15},
	"prometheus/rules/prometheus.yaml":    {groups: 1, rules: 23, recording: 0},
}

func TestVendoredRulesParseAndMatchVendorDoc(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	for path, want := range wantRules {
		t.Run(path, func(t *testing.T) {
			groups, errs := rulefmt.ParseFile(path, false, model.UTF8Validation, p, slog.Default())
			if len(errs) > 0 {
				t.Fatalf("%s does not parse as a Prometheus rule file: %v", path, errs)
			}
			rules, recording := 0, 0
			for _, g := range groups.Groups {
				for _, r := range g.Rules {
					rules++
					if r.Record != "" {
						recording++
					}
				}
			}
			if len(groups.Groups) != want.groups || rules != want.rules || recording != want.recording {
				t.Errorf("%s has %d groups / %d rules / %d recording, want %d / %d / %d -- "+
					"upstream changed; re-run demo/vendor.sh and update demo/VENDOR.md deliberately",
					path, len(groups.Groups), rules, recording, want.groups, want.rules, want.recording)
			}
		})
	}
}

// TestVendoredDashboardCarriesTheTemplateVariableHazard pins the finding
// that shapes sub-project B: every panel query in a real dashboard is
// template-variable laden and therefore not valid PromQL until
// substituted. If this ever stops being true, B's substitution layer is
// solving a problem the fixture no longer contains, and we want to know.
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

	if len(exprs) != 284 {
		t.Errorf("dashboard has %d panel queries, want 284 -- upstream revision changed; "+
			"re-run demo/vendor.sh and update demo/VENDOR.md deliberately", len(exprs))
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
