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

// deadServerURL starts an httptest.Server and immediately closes it, so the
// returned URL points at a port nothing is listening on. Connection is
// refused right away -- no timeout to wait out -- which is what "Grafana is
// configured but unreachable" needs to look like in a test, portably.
func deadServerURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(nil)
	url := s.URL
	s.Close()
	return url
}

// singleMetricFixture stands up a fake Prometheus exposing one metric no
// rule references, plus a real prometheus.yml jetsam can render a drop
// into, and writes cfgYAML (already containing prometheus.url) to
// jetsam.yaml. It is a lower-level sibling of proposeFixture that lets the
// caller add a grafana: or query_log: section.
func singleMetricFixture(t *testing.T, extraYAML string) (cfgPath string) {
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

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"), 0o644)

	cfgPath = filepath.Join(dir, "jetsam.yaml")
	body := "prometheus:\n  url: " + prom.URL + "\n  timeout: 5s\nprometheus_file: " + promYAMLPath + "\n" + extraYAML
	os.WriteFile(cfgPath, []byte(body), 0o644)
	return cfgPath
}

// TestProposeWithFlagStillWithholdsWhenGrafanaIsUnreachable is the
// regression test for the first fix in this round: proposeCmd's own
// `blocked` boolean used to check only len(res.Blocked) > 0, so
// -include-unreferenced could re-widen a metric that verdict.Compute had
// already withheld because dashboard evidence was configured but could not
// be fetched. That is precisely the safety property this task exists to
// guarantee -- "no dashboard reads it" and "nobody looked" must never be
// treated as the same claim, flag or no flag.
//
// If proposeCmd's blocked computation regresses to `len(res.Blocked) > 0`
// alone, this test fails: orphan_metric_total would be proposed for
// dropping despite Grafana being unreachable.
func TestProposeWithFlagStillWithholdsWhenGrafanaIsUnreachable(t *testing.T) {
	dead := deadServerURL(t)
	cfgPath := singleMetricFixture(t, "grafana:\n  url: "+dead+"\n")

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "orphan_metric_total") {
		t.Errorf("-include-unreferenced re-widened a metric withheld for unreachable dashboard evidence:\n%s", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed:\n%s", out)
	}
	if !strings.Contains(out, "could not be fetched") {
		t.Errorf("output does not say dashboard evidence could not be fetched:\n%s", out)
	}
}

// writeQueryLog writes a query log file with two rule-evaluation lines
// (no real reads) spanning the given duration, which is all Reading.Span
// needs -- see querylog.Read's doc comment: a rule evaluation is just as
// good evidence the log was running as a real read is.
func writeQueryLog(t *testing.T, dir string, span string) string {
	t.Helper()
	path := filepath.Join(dir, "queries.log")
	lines := `{"time":"2020-01-01T00:00:00.000Z","ruleGroup":{"name":"g","file":"f"},"params":{"query":"vector(1)"}}` + "\n" +
		`{"time":"2020-01-01T` + span + `.000Z","ruleGroup":{"name":"g","file":"f"},"params":{"query":"vector(1)"}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// singleMetricFixtureReferenced is singleMetricFixture's sibling where the
// one metric IS referenced by a rule, so it grades GradeUsed regardless of
// query log state -- used for the "everything is already accounted for"
// fallback case, where there must be nothing left for a query log to make
// eligible.
func singleMetricFixtureReferenced(t *testing.T, extraYAML string) (cfgPath string) {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"referenced_metric_total","value":123}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[` +
				`{"name":"r","type":"alerting","query":"referenced_metric_total > 0"}]}]}}`))
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
	body := "prometheus:\n  url: " + prom.URL + "\n  timeout: 5s\nprometheus_file: " + promYAMLPath + "\n" + extraYAML
	os.WriteFile(cfgPath, []byte(body), 0o644)
	return cfgPath
}

// TestProposeNothingToProposeMessageNamesTheActualReason is the regression
// test for the second fix in this round: proposeCmd's "nothing to
// propose" fallback used to say "no query log is configured" in all three
// of these cases, even the two where one plainly was. Each case must say
// something different, and each is only true in its own case.
//
// If the fallback reverts to one hardcoded message, at least two of the
// three subtests below fail: "log qualifies" would wrongly blame a
// missing log, and "log too short" would too.
func TestProposeNothingToProposeMessageNamesTheActualReason(t *testing.T) {
	t.Run("no query log configured at all", func(t *testing.T) {
		cfgPath := singleMetricFixture(t, "")
		var stdout, stderr bytes.Buffer
		rc := proposeCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv, failProvider(t))
		if rc != 0 {
			t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "no query log is configured") {
			t.Errorf("output does not say no query log is configured:\n%s", out)
		}
		if strings.Contains(out, "already referenced") || strings.Contains(out, "not long enough") {
			t.Errorf("output claims something about a log that does not exist:\n%s", out)
		}
	})

	t.Run("query log configured but shorter than min_window", func(t *testing.T) {
		dir := t.TempDir()
		logPath := writeQueryLog(t, dir, "00:00:01") // 1 second of span
		cfgPath := singleMetricFixture(t, "query_log:\n  path: "+logPath+"\n  min_window: 1h\n")
		var stdout, stderr bytes.Buffer
		rc := proposeCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv, failProvider(t))
		if rc != 0 {
			t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "not long enough") {
			t.Errorf("output does not say the log's window fell short:\n%s", out)
		}
		if strings.Contains(out, "no query log is configured") {
			t.Errorf("output denies a query log exists although query_log.path is set:\n%s", out)
		}
		if strings.Contains(out, "already referenced") {
			t.Errorf("output claims the corpus is fully accounted for although the log did not qualify:\n%s", out)
		}
	})

	t.Run("query log qualifies and nothing is left unread", func(t *testing.T) {
		dir := t.TempDir()
		logPath := writeQueryLog(t, dir, "02:00:00") // 2h of span
		cfgPath := singleMetricFixtureReferenced(t, "query_log:\n  path: "+logPath+"\n  min_window: 1h\n")
		var stdout, stderr bytes.Buffer
		rc := proposeCmd([]string{"-config", cfgPath}, &stdout, &stderr, noEnv, failProvider(t))
		if rc != 0 {
			t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "already referenced") {
			t.Errorf("output does not say everything is already accounted for:\n%s", out)
		}
		if strings.Contains(out, "no query log is configured") || strings.Contains(out, "not long enough") {
			t.Errorf("output blames the query log although it qualifies:\n%s", out)
		}
	})
}

// TestProposeDegradesWhenTheQueryLogCannotBeRead: gather used to return an
// error when the log could not be read, so one truncated line in a
// multi-GB rotated set made jetsam exit 1 and do nothing at all. It must
// degrade exactly as an unreachable Grafana does -- the run still works,
// the failure is loud, and every drop is withheld including under
// -include-unreferenced. What it must never do is treat the log as empty:
// an empty log says nobody queried anything, which licenses dropping
// everything.
func TestProposeDegradesWhenTheQueryLogCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "queries.log")
	// A line with no parseable time field: querylog.Read refuses the file
	// rather than absorbing it, which is the behaviour being degraded from.
	if err := os.WriteFile(logPath, []byte("not a query log line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := singleMetricFixture(t, "query_log:\n  path: "+logPath+"\n  min_window: 1h\n")

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced"}, &stdout, &stderr, noEnv, failProvider(t))
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0 -- an unreadable log must not stop the run: stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "orphan_metric_total") {
		t.Errorf("-include-unreferenced re-widened a metric withheld for an unreadable query log:\n%s", out)
	}
	if !strings.Contains(out, "nothing to propose") {
		t.Errorf("output does not say nothing was proposed:\n%s", out)
	}
	if !strings.Contains(out, "could not be read") {
		t.Errorf("output does not say the query log could not be read:\n%s", out)
	}
}
