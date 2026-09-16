package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unresolvedAndDropFixture stands up a fake Prometheus exposing two
// unreferenced metrics: one whose job resolves to something with a real
// scrape_config (a genuine drop), and one whose query returns zero jobs at
// all -- a target that stopped being scraped, or a metric a deploy
// removed. Before this fix, the second metric's fate was reported ONLY
// when there were zero drops in the whole run; this fixture's whole point
// is that there is at least one real drop alongside it.
func unresolvedAndDropFixture(t *testing.T) (cfgPath string) {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"live_orphan_total","value":50},
				{"name":"stale_orphan_total","value":75}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		case "/api/v1/query":
			q := r.URL.Query().Get("query")
			switch {
			case strings.Contains(q, "live_orphan_total"):
				w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"api"}}]}}`))
			case strings.Contains(q, "stale_orphan_total"):
				// Zero results: nothing currently exposes this metric under
				// any job label at all.
				w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			default:
				t.Errorf("unexpected query %q", q)
				w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			}
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
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o600)
	return cfgPath
}

// TestProposeReportsUnresolvedAlongsideARealDrop pins I5: unresolved
// metrics (graded droppable, but currently produced by no job at all) must
// be reported in BOTH the terminal output and the PR body even when the
// same run also proposes one or more real drops -- not only in the
// "nothing to propose at all" branch, which is what silently dropped them
// before this fix.
func TestProposeReportsUnresolvedAlongsideARealDrop(t *testing.T) {
	cfgPath := unresolvedAndDropFixture(t)
	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()

	// The real drop must still be proposed.
	if !strings.Contains(out, "live_orphan_total") {
		t.Errorf("the resolvable metric was not proposed:\n%s", out)
	}
	if !strings.Contains(out, "metric_relabel_configs") {
		t.Errorf("nothing was rendered despite one resolvable candidate:\n%s", out)
	}

	// The unresolved metric must be named in the terminal output, not
	// silently absorbed because a drop existed elsewhere in the same run.
	if !strings.Contains(out, "stale_orphan_total") {
		t.Errorf("terminal output does not name the unresolved metric:\n%s", out)
	}
	if !strings.Contains(out, "no job currently exposing them") && !strings.Contains(out, "no job currently exposes it") {
		t.Errorf("terminal output does not explain why the metric was withheld:\n%s", out)
	}

	// And it must reach the PR body's own Declined section, the same way a
	// blocked rule or a job-missing decline does.
	if !strings.Contains(out, "### Declined") {
		t.Errorf("PR body does not carry a Declined section:\n%s", out)
	}
	bodySection := out[strings.Index(out, "### Declined"):]
	if !strings.Contains(bodySection, "stale_orphan_total") {
		t.Errorf("PR body's Declined section does not name the unresolved metric:\n%s", bodySection)
	}
}
