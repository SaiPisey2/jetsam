package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/forge"
)

// sentinel stands in for $GITHUB_TOKEN in every test below: a value
// distinctive enough that its appearance anywhere in stdout, stderr, or a
// returned error is unambiguous evidence of a leak.
const sentinel = "ghp_SENTINEL_DO_NOT_LEAK_0123456789"

func noEnv(string) string { return "" }

func sentinelEnv(key string) string {
	if key == "GITHUB_TOKEN" {
		return sentinel
	}
	return ""
}

func failProvider(t *testing.T) func(string) forge.Provider {
	return func(token string) forge.Provider {
		t.Fatalf("newProvider called with token %q: -apply must fail validation before ever building a provider", token)
		return nil
	}
}

// TestProposeApplyFailsFastWithoutOwnerRepoOrToken pins requirement 3 of
// the apply contract: -apply without -owner/-repo, or without a token, is
// refused before any network call, any file read, and any scan work. Each
// case below points -config at a file that does not exist, so a validation
// order bug (checking flags only after config.Load) would surface as a
// "no such file" error instead of the flag error asserted here -- and
// newProvider is wired to fail the test outright if it is ever called at
// all, which it must not be when validation fails.
func TestProposeApplyFailsFastWithoutOwnerRepoOrToken(t *testing.T) {
	noSuchConfig := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	cases := []struct {
		name           string
		args           []string
		env            func(string) string
		wantInErr      []string
		mustNotContain string
	}{
		{
			name:           "missing owner and repo and token",
			args:           []string{"-config", noSuchConfig, "-apply"},
			env:            noEnv,
			wantInErr:      []string{"-owner", "-repo", "$GITHUB_TOKEN"},
			mustNotContain: sentinel,
		},
		{
			name:           "missing owner and repo, token present",
			args:           []string{"-config", noSuchConfig, "-apply"},
			env:            sentinelEnv,
			wantInErr:      []string{"-owner", "-repo"},
			mustNotContain: sentinel,
		},
		{
			name:           "missing token only",
			args:           []string{"-config", noSuchConfig, "-apply", "-owner", "o", "-repo", "r"},
			env:            noEnv,
			wantInErr:      []string{"$GITHUB_TOKEN"},
			mustNotContain: sentinel,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := proposeCmd(tc.args, &stdout, &stderr, tc.env, failProvider(t))
			if rc == 0 {
				t.Fatalf("proposeCmd = 0, want a non-zero exit for missing -apply prerequisites (stdout=%q stderr=%q)",
					stdout.String(), stderr.String())
			}
			errText := stderr.String()
			if strings.Contains(errText, "no such file") || strings.Contains(errText, "does-not-exist.yaml") {
				t.Errorf("validation ran after config.Load (config-not-found error leaked through): %s", errText)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(errText, want) {
					t.Errorf("error %q does not mention %q", errText, want)
				}
			}
			if strings.Contains(errText, tc.mustNotContain) || strings.Contains(stdout.String(), tc.mustNotContain) {
				t.Errorf("sentinel token leaked into output: stdout=%q stderr=%q", stdout.String(), errText)
			}
		})
	}
}

// TestProposeApplyWithNothingToProposeOpensNothing exercises the entire
// -apply path -- flag validation, config load, a real (fake, via
// httptest) Prometheus scan, and the point where apply would call through
// to the forge -- with $GITHUB_TOKEN set to a sentinel and a
// forge.NewFakeProvider() wired in through newProvider so nothing ever
// reaches a real repository.
//
// Under the standing rule that runPropose always calls verdict.Compute
// with a corpus carrying no query log evidence (v0.1 wires none in), a
// default scan never finds anything droppable, so this is the only shape
// of -apply run this binary can produce today: it must still fail-safe by
// not creating a branch or opening a PR, and it must not print the token
// anywhere while doing so.
func TestProposeApplyWithNothingToProposeOpensNothing(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"go_gc_duration_seconds","value":1258}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		default:
			t.Errorf("unexpected request to %s: -apply must not query jobs when nothing is droppable", r.URL.Path)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	defer prom.Close()

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"), 0o644)

	cfgPath := filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o644)

	var fake *forge.FakeProvider
	newProvider := func(token string) forge.Provider {
		if token != sentinel {
			t.Errorf("newProvider received token %q, want the sentinel", token)
		}
		fake = forge.NewFakeProvider()
		return fake
	}

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-apply", "-owner", "o", "-repo", "r"}, &stdout, &stderr, sentinelEnv, newProvider)
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0 (nothing droppable is not an error): stderr=%s", rc, stderr.String())
	}

	out := stdout.String() + stderr.String()
	if strings.Contains(out, sentinel) {
		t.Errorf("sentinel token leaked into output: %q", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed: %q", out)
	}
	// This is the fix for the Minor finding: with -apply given and nothing
	// to propose, the output must say so -- not the dry-run "re-run with
	// -apply" line, which would tell an operator who already passed
	// -apply to do the very thing they just did, and would misreport a
	// no-op apply run as though it had just opened something.
	if strings.Contains(out, "re-run with -apply") || strings.Contains(out, "Re-run with -apply") {
		t.Errorf("an -apply run that found nothing to propose still told the operator to re-run with -apply: %q", out)
	}
	if !strings.Contains(out, "nothing was opened") {
		t.Errorf("output does not say nothing was opened for this -apply run: %q", out)
	}
	// newProvider is only called on the path that would actually open a
	// PR; with nothing droppable it must never be reached at all.
	if fake != nil {
		t.Fatal("newProvider was called although there was nothing to propose; -apply must not touch the forge in that case")
	}
}

// TestProposeDryRunWithNothingToProposeSaysSo is the dry-run counterpart:
// without -apply, and with nothing droppable, the output must describe a
// dry run with nothing to propose -- not claim a PR would have opened.
func TestProposeDryRunWithNothingToProposeSaysSo(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"go_gc_duration_seconds","value":1258}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		default:
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	defer prom.Close()

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"), 0o644)

	cfgPath := filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o644)

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "dry run") {
		t.Errorf("output does not identify itself as a dry run: %q", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed: %q", out)
	}
}

// TestApplyAndReportOpensThroughTheFakeForge is the counterpart with
// something to propose: it drives the exact code applyAndReport runs
// (what proposeCmd calls once -apply has something to commit) against
// forge.NewFakeProvider(), and confirms the PR is opened, is idempotent,
// and that the report never needs -- and never gets passed -- a token.
func TestApplyAndReportOpensThroughTheFakeForge(t *testing.T) {
	p := forge.NewFakeProvider()
	var stdout, stderr bytes.Buffer

	rc := applyAndReport(context.Background(), &stdout, &stderr, p, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if rc != 0 {
		t.Fatalf("applyAndReport = %d, want 0: stderr=%s", rc, stderr.String())
	}
	if strings.Contains(stdout.String(), sentinel) || strings.Contains(stderr.String(), sentinel) {
		t.Errorf("sentinel leaked despite never being passed to applyAndReport: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if len(p.PRs) != 1 {
		t.Fatalf("got %d PRs on the fake forge, want 1", len(p.PRs))
	}
}
