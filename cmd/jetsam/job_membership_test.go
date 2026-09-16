package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// jobMembershipFixture stands up a fake Prometheus exposing the given
// unreferenced metrics (name -> series count) and resolves each one's job
// via jobsFor (name -> the job labels QueryJobsFor should return for it).
// prometheus.yml is written to define exactly definedJobs, modelling the
// ordinary real-world case this fix round addresses: a scrape config that
// legitimately covers only some of a Prometheus's jobs, because the rest
// live in another file (scrape_config_files), arrive via service
// discovery, or simply were not the file the operator pointed jetsam at.
func jobMembershipFixture(t *testing.T, metrics map[string]int, jobsFor map[string][]string, definedJobs []string) (cfgPath, promYAMLPath string) {
	t.Helper()

	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			var entries []string
			for name, n := range metrics {
				entries = append(entries, fmt.Sprintf(`{"name":%q,"value":%d}`, name, n))
			}
			fmt.Fprintf(w, `{"status":"success","data":{"seriesCountByMetricName":[%s]}}`, strings.Join(entries, ","))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		case "/api/v1/query":
			q := r.URL.Query().Get("query")
			var jobs []string
			for name, js := range jobsFor {
				if strings.Contains(q, name) {
					jobs = js
					break
				}
			}
			var results []string
			for _, j := range jobs {
				results = append(results, fmt.Sprintf(`{"metric":{"job":%q}}`, j))
			}
			fmt.Fprintf(w, `{"status":"success","data":{"result":[%s]}}`, strings.Join(results, ","))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)

	dir := t.TempDir()
	promYAMLPath = filepath.Join(dir, "prometheus.yml")
	var scrapeConfigs strings.Builder
	for i, j := range definedJobs {
		fmt.Fprintf(&scrapeConfigs, "  - job_name: %s\n    static_configs:\n      - targets: ['localhost:%d']\n", j, 9100+i)
	}
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n"+scrapeConfigs.String()), 0o644)

	cfgPath = filepath.Join(dir, "jetsam.yaml")
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o644)
	return cfgPath, promYAMLPath
}

// TestProposeDeclinesAMetricWhoseJobIsAbsent pins the actual bug the
// coordinator found by running the real command: one droppable metric
// produced by a job absent from the target file must be excluded and
// reported as declined, not abort the whole run. A second candidate whose
// job IS present must still be proposed in the same run.
func TestProposeDeclinesAMetricWhoseJobIsAbsent(t *testing.T) {
	cfgPath, promYAMLPath := jobMembershipFixture(t,
		map[string]int{"orphan_a": 10, "orphan_b": 20},
		map[string][]string{"orphan_a": {"ghost"}, "orphan_b": {"api"}},
		[]string{"api"},
	)

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0 (a declined candidate is not a failure): stderr=%s", rc, stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, `orphan_a`) || !strings.Contains(out, `"ghost"`) {
		t.Errorf("declined entry does not name the metric and the missing job:\n%s", out)
	}
	if !strings.Contains(out, promYAMLPath) {
		t.Errorf("declined entry does not name the file the job is missing from:\n%s", out)
	}
	if !strings.Contains(out, "declined 1 metric(s)") {
		t.Errorf("declined count is wrong or missing:\n%s", out)
	}
	// orphan_b's job IS present, so it must still be proposed in the same
	// run -- one declined candidate must not take the rest down with it.
	if !strings.Contains(out, "orphan_b") {
		t.Errorf("the other candidate, whose job is present, was not proposed:\n%s", out)
	}
	if !strings.Contains(out, "metric_relabel_configs") {
		t.Errorf("nothing was actually rendered despite one valid candidate:\n%s", out)
	}
}

// TestProposeWithEveryCandidateDeclinedExitsCleanly covers "all candidates
// absent": exit 0, a clear message, and nothing rendered -- not an error,
// and not a silent empty result either.
func TestProposeWithEveryCandidateDeclinedExitsCleanly(t *testing.T) {
	cfgPath, _ := jobMembershipFixture(t,
		map[string]int{"orphan_a": 10, "orphan_b": 20},
		map[string][]string{"orphan_a": {"ghost1"}, "orphan_b": {"ghost2"}},
		[]string{"api"},
	)

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed:\n%s", out)
	}
	if !strings.Contains(out, "declined 2 metric(s)") {
		t.Errorf("declined count is wrong or missing:\n%s", out)
	}
	if !strings.Contains(out, "ghost1") || !strings.Contains(out, "ghost2") {
		t.Errorf("declined entries do not name both missing jobs:\n%s", out)
	}
	if strings.Contains(out, "metric_relabel_configs") {
		t.Errorf("something was rendered despite every candidate being declined:\n%s", out)
	}
}

// TestProposeSplitsAMultiJobMetricAcrossPresentAndAbsentJobs covers a
// metric QueryJobsFor reports as produced by several jobs, only some of
// which the target file defines: the present job gets the drop, the
// absent one is declined individually -- the whole metric is never
// declined just because one of its jobs is missing.
func TestProposeSplitsAMultiJobMetricAcrossPresentAndAbsentJobs(t *testing.T) {
	cfgPath, _ := jobMembershipFixture(t,
		map[string]int{"shared_metric": 30},
		map[string][]string{"shared_metric": {"api", "ghost"}},
		[]string{"api"},
	)

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "metric_relabel_configs") {
		t.Errorf("the present job's half of the metric was not rendered:\n%s", out)
	}
	if !strings.Contains(out, "shared_metric") {
		t.Errorf("shared_metric is missing from the rendered output entirely:\n%s", out)
	}
	if !strings.Contains(out, `"ghost"`) {
		t.Errorf("the absent job half was not declined by name:\n%s", out)
	}
	if !strings.Contains(out, "declined 1 metric(s)") {
		t.Errorf("declined count is wrong or missing:\n%s", out)
	}
	// The job actually rendered into must be api, not ghost.
	api := out[strings.Index(out, "job_name: api"):]
	if !strings.Contains(api, "shared_metric") {
		t.Errorf("shared_metric's drop did not land under job api:\n%s", out)
	}
}

// TestProposeDeclinedEntriesAppearInBothTerminalAndBody covers the
// requirement that the declined list is counted and shown in BOTH the
// terminal dry-run output and the PR body -- which, in dry-run mode, are
// both written to the same stdout, so this checks for the body's own
// "### Declined" section in addition to the terminal-specific listing
// printed ahead of the diff.
func TestProposeDeclinedEntriesAppearInBothTerminalAndBody(t *testing.T) {
	cfgPath, _ := jobMembershipFixture(t,
		map[string]int{"orphan_a": 10, "orphan_b": 20},
		map[string][]string{"orphan_a": {"ghost"}, "orphan_b": {"api"}},
		[]string{"api"},
	)

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()

	// The terminal-specific line, printed ahead of the diff.
	if !strings.Contains(out, "declined 1 metric(s): their job has no scrape_config") {
		t.Errorf("terminal output does not list the declined candidate:\n%s", out)
	}
	// The PR body's own Declined section (internal/emit.Body), reusing the
	// same section blocked rules use, with the count including this entry.
	if !strings.Contains(out, "### Declined") {
		t.Errorf("PR body does not carry a Declined section:\n%s", out)
	}
	if !strings.Contains(out, "jetsam refused to propose anything for 1 input(s)") {
		t.Errorf("PR body's Declined section does not count the declined candidate:\n%s", out)
	}
	if !strings.Contains(out, "orphan_a") {
		t.Errorf("PR body's Declined section does not name the declined metric:\n%s", out)
	}
}

// A quick sanity check that the fixture's job-count arithmetic in the
// prometheus.yml it writes actually produces valid, distinct target ports
// -- not load-bearing for the bug itself, just guards the test helper.
func TestJobMembershipFixtureWritesDistinctPorts(t *testing.T) {
	_, promYAMLPath := jobMembershipFixture(t, nil, nil, []string{"a", "b", "c"})
	data, err := os.ReadFile(promYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if !strings.Contains(string(data), strconv.Itoa(9100+i)) {
			t.Errorf("missing port for job %d in:\n%s", i, data)
		}
	}
}
