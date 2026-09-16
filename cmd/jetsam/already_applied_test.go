package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/emit"
	"github.com/SaiPisey2/jetsam/internal/forge"
)

// alreadyAppliedFixture stands up a fake Prometheus exposing one
// unreferenced metric, and writes a prometheus.yml that ALREADY carries the
// drop rule propose would otherwise add -- computed by calling emit.Render
// once up front, exactly the state a real checkout is in the run after a
// proposal merges. Re-running propose against it is the ordinary
// second-run-after-merge case this fix (I3) addresses.
func alreadyAppliedFixture(t *testing.T) (cfgPath string) {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"orphan_metric_total","value":123}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		case "/api/v1/query":
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"api"}}]}}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)

	before := "global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"
	after, err := emit.Render(before, []emit.Drop{{Metric: "orphan_metric_total", Series: 123, Job: "api"}})
	if err != nil {
		t.Fatalf("computing the already-applied fixture: %v", err)
	}

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte(after), 0o644)

	cfgPath = filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o600)
	return cfgPath
}

// TestProposeDryRunOnAnAlreadyAppliedConfigOpensNothing pins I3's dry-run
// side: a target file that already carries every drop rule this run would
// propose must report that plainly and return, not print a diff (there is
// none) or claim a PR would open.
func TestProposeDryRunOnAnAlreadyAppliedConfigOpensNothing(t *testing.T) {
	cfgPath := alreadyAppliedFixture(t)
	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "already carries every drop rule") {
		t.Errorf("output does not say the config already carries the rule:\n%s", out)
	}
	if !strings.Contains(out, "nothing to open") {
		t.Errorf("output does not say nothing needs opening:\n%s", out)
	}
	if strings.Contains(out, "dry run") {
		t.Errorf("output still talks about a dry run despite there being nothing to propose:\n%s", out)
	}
	if strings.Contains(out, "stop storing") {
		t.Errorf("a PR title/body was printed despite there being no change to propose:\n%s", out)
	}
}

// TestProposeApplyOnAnAlreadyAppliedConfigOpensNothing pins I3's -apply
// side, the actual bug: falling through used to create a branch, commit
// identical bytes, and have GitHub reject OpenPR with 422 "No commits
// between" -- every run, leaving a stray branch each time. newProvider
// must never even be called.
func TestProposeApplyOnAnAlreadyAppliedConfigOpensNothing(t *testing.T) {
	cfgPath := alreadyAppliedFixture(t)
	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced", "-apply", "-owner", "o", "-repo", "r"},
		&stdout, &stderr, sentinelEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "already carries every drop rule") {
		t.Errorf("output does not say the config already carries the rule:\n%s", out)
	}
	if !strings.Contains(out, "nothing to open") {
		t.Errorf("output does not say nothing needs opening:\n%s", out)
	}
	if strings.Contains(out, sentinel) || strings.Contains(stderr.String(), sentinel) {
		t.Errorf("sentinel token leaked into output: stdout=%q stderr=%q", out, stderr.String())
	}
}

// TestProposeApplyOnAnAlreadyAppliedConfigTouchesNoForgeMethod is the same
// scenario driven against a real *forge.FakeProvider (instead of
// failProvider, which fails the test the instant newProvider is called at
// all) so the assertion is specifically "no branch, no commit, no PR" --
// not merely "newProvider was never constructed".
func TestProposeApplyOnAnAlreadyAppliedConfigTouchesNoForgeMethod(t *testing.T) {
	cfgPath := alreadyAppliedFixture(t)
	var fake *forge.FakeProvider
	newProvider := func(token string) forge.Provider {
		fake = forge.NewFakeProvider()
		return fake
	}

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced", "-apply", "-owner", "o", "-repo", "r"},
		&stdout, &stderr, sentinelEnv, newProvider)
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	if fake != nil {
		t.Fatal("newProvider was called although the config already carries every drop rule; -apply must not touch the forge in that case")
	}
}
