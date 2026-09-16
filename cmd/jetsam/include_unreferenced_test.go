package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// TestEligibleForDrop pins the widening rule -include-unreferenced applies
// at the propose layer, independently of verdict.Compute: it only ever
// ADDS GradeUnreferenced verdicts to what was already droppable, never
// touches a GradeUsed verdict (a rule reads it, or a recording rule writes
// it), and never fires at all once blocked is true.
func TestEligibleForDrop(t *testing.T) {
	cases := []struct {
		name                string
		v                   verdict.Verdict
		blocked             bool
		includeUnreferenced bool
		want                bool
	}{
		{
			name: "already droppable stays droppable regardless of the flag",
			v:    verdict.Verdict{Grade: verdict.GradeUnqueried, Droppable: true},
			want: true,
		},
		{
			name: "unreferenced is not droppable by default",
			v:    verdict.Verdict{Grade: verdict.GradeUnreferenced, Droppable: false},
			want: false,
		},
		{
			name:                "unreferenced becomes droppable with the flag",
			v:                   verdict.Verdict{Grade: verdict.GradeUnreferenced, Droppable: false},
			includeUnreferenced: true,
			want:                true,
		},
		{
			name:                "unreferenced stays withheld when blocked, even with the flag",
			v:                   verdict.Verdict{Grade: verdict.GradeUnreferenced, Droppable: false},
			blocked:             true,
			includeUnreferenced: true,
			want:                false,
		},
		{
			name:                "a recording rule's output (Used) is never widened",
			v:                   verdict.Verdict{Grade: verdict.GradeUsed, Droppable: false},
			includeUnreferenced: true,
			want:                false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eligibleForDrop(tc.v, tc.blocked, tc.includeUnreferenced); got != tc.want {
				t.Errorf("eligibleForDrop(%+v, blocked=%v, include=%v) = %v, want %v",
					tc.v, tc.blocked, tc.includeUnreferenced, got, tc.want)
			}
		})
	}
}

// proposeFixture stands up a fake Prometheus exposing one metric with no
// rule referencing it (so it grades GradeUnreferenced, never droppable by
// default) and a real prometheus.yml jetsam can render a drop into. Passing
// withBlockedRule=true adds one unparseable rule, which must forbid every
// drop -- -include-unreferenced included.
func proposeFixture(t *testing.T, withBlockedRule bool) (cfgPath string) {
	t.Helper()
	rulesBody := `{"status":"success","data":{"groups":[]}}`
	if withBlockedRule {
		rulesBody = `{"status":"success","data":{"groups":[{"name":"g","rules":[` +
			`{"name":"Broken","type":"alerting","query":"rate(x[$interval])"}]}]}}`
	}

	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"orphan_metric_total","value":123}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(rulesBody))
		case "/api/v1/query":
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"api"}}]}}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"), 0o644)

	cfgPath = filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o644)
	return cfgPath
}

func TestProposeWithoutFlagProposesNothingForAnUnreferencedMetric(t *testing.T) {
	cfgPath := proposeFixture(t, false)
	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "orphan_metric_total") {
		t.Errorf("metric was proposed without -include-unreferenced:\n%s", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed:\n%s", out)
	}
}

func TestProposeWithFlagProposesAnUnreferencedMetric(t *testing.T) {
	cfgPath := proposeFixture(t, false)
	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "orphan_metric_total") {
		t.Errorf("metric was not proposed with -include-unreferenced:\n%s", out)
	}
	if !strings.Contains(out, "unreferenced") {
		t.Errorf("terminal output does not say the drop rests on unreferenced grade:\n%s", out)
	}
	if !strings.Contains(out, "-include-unreferenced") {
		t.Errorf("terminal output does not name the flag that made this drop possible:\n%s", out)
	}
}

// TestProposeWithFlagNeverProposesARecordingRulesOutput exercises the same
// guarantee as TestEligibleForDrop's "Used is never widened" case, but end
// to end through proposeCmd: a metric a recording rule PRODUCES is graded
// GradeUsed, never GradeUnreferenced, so -include-unreferenced must not
// touch it -- dropping it would mean editing a rule file, not a scrape
// config, which v0.1 does not do at all.
func TestProposeWithFlagNeverProposesARecordingRulesOutput(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"job:latency:p99","value":42}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[` +
				`{"name":"job:latency:p99","type":"recording","query":"vector(1)"}]}]}}`))
		default:
			t.Errorf("unexpected request to %s: a recording rule's own output must never be looked up as a drop candidate", r.URL.Path)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"), 0o644)
	cfgPath := filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o644)

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "job:latency:p99") {
		t.Errorf("a recording rule's own output was proposed for dropping:\n%s", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed:\n%s", out)
	}
}

func TestProposeWithFlagAndABlockedRuleStillProposesNothing(t *testing.T) {
	cfgPath := proposeFixture(t, true)
	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "orphan_metric_total") {
		t.Errorf("metric was proposed despite a blocked rule, even with -include-unreferenced:\n%s", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed:\n%s", out)
	}
}
