package corpus

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSubstituteMakesRealDashboardQueriesParse(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "a variable inside a duration becomes a real duration",
			in:   `rate(node_cpu_seconds_total[$__rate_interval])`,
			want: `rate(node_cpu_seconds_total[5m])`,
		},
		{
			name: "a braced variable inside a duration too",
			in:   `rate(x_total[${interval}])`,
			want: `rate(x_total[5m])`,
		},
		{
			name: "the interval family outside a duration",
			in:   `sum(rate(x[5m])) * $__interval_ms`,
			want: `sum(rate(x[5m])) * 5m`,
		},
		{
			name: "a variable in a label value is left alone -- it already parses",
			in:   `node_load1{instance="$node",job="$job"}`,
			want: `node_load1{instance="$node",job="$job"}`,
		},
		{
			name: "a query with no variables is untouched",
			in:   `sum by (path) (rate(jetsam_demo_requests_total[5m]))`,
			want: `sum by (path) (rate(jetsam_demo_requests_total[5m]))`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Substitute(tc.in); got != tc.want {
				t.Errorf("Substitute(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSubstituteFixesTheVendoredDashboard is the measurement that justifies
// this function existing. Without substitution 124 of dashboard 1860's 284
// panel queries fail to parse -- and not randomly: every failure calls
// rate()/increase()/delta(). A corpus reader without substitution therefore
// recovers exactly the non-rate half, a partial and biased corpus that looks
// like it is working. With substitution all 284 parse.
func TestSubstituteFixesTheVendoredDashboard(t *testing.T) {
	b, err := os.ReadFile("../../fixture/grafana/dashboards/node-exporter-full.json")
	if err != nil {
		t.Fatalf("read vendored dashboard: %v", err)
	}
	var dash struct{ Panels []panelForTest `json:"panels"` }
	if err := json.Unmarshal(b, &dash); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}
	var exprs []string
	collectForTest(dash.Panels, &exprs)
	if len(exprs) != 284 {
		t.Fatalf("dashboard has %d panel queries, want 284", len(exprs))
	}

	rawOK, subOK := 0, 0
	for _, e := range exprs {
		if _, err := promParser.ParseExpr(e); err == nil {
			rawOK++
		}
		if _, err := promParser.ParseExpr(Substitute(e)); err == nil {
			subOK++
		}
	}
	if rawOK != 160 {
		t.Errorf("%d of 284 parse without substitution, want 160 -- upstream changed", rawOK)
	}
	if subOK != len(exprs) {
		t.Errorf("%d of %d parse WITH substitution, want all of them", subOK, len(exprs))
		for _, e := range exprs {
			if _, err := promParser.ParseExpr(Substitute(e)); err != nil {
				t.Logf("  still failing: %.100s", Substitute(e))
			}
		}
	}
}

type panelForTest struct {
	Panels  []panelForTest `json:"panels"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

func collectForTest(ps []panelForTest, out *[]string) {
	for _, p := range ps {
		for _, t := range p.Targets {
			if t.Expr != "" {
				*out = append(*out, t.Expr)
			}
		}
		collectForTest(p.Panels, out)
	}
}

func TestSubstituteLeavesMetricNamesAlone(t *testing.T) {
	// The safety argument for substitution is that it cannot change which
	// metrics a query names. Pin that.
	in := `sum by (job) (rate(http_requests_total{code="$code"}[$__rate_interval]))`
	refs, err := Extract(Substitute(in))
	if err != nil {
		t.Fatalf("substituted query does not parse: %v", err)
	}
	if len(refs.Names) != 1 || refs.Names[0] != "http_requests_total" {
		t.Errorf("Names = %v, want exactly [http_requests_total]", refs.Names)
	}
}
