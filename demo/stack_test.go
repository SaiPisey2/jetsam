//go:build integration

package demo

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const promURL = "http://localhost:9090"

// get decodes a Prometheus API response, failing with an instruction
// rather than a bare connection error when the stack is not up.
func get(t *testing.T, path string, out any) {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(promURL + path)
	if err != nil {
		t.Fatalf("prometheus unreachable, run `make demo-up`: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// TestStackLoadsEveryVendoredRule checks the count VENDOR.md records
// actually reached Prometheus. A rule file that fails to load leaves
// Prometheus running and serving, so without this the stack looks healthy
// while the corpus it exists to provide is missing.
func TestStackLoadsEveryVendoredRule(t *testing.T) {
	var body struct {
		Data struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	get(t, "/api/v1/rules", &body)

	rules, recording := 0, 0
	for _, g := range body.Data.Groups {
		for _, r := range g.Rules {
			rules++
			if r.Type == "recording" {
				recording++
			}
		}
	}
	if len(body.Data.Groups) != 3 || rules != 64 || recording != 15 {
		t.Errorf("prometheus loaded %d groups / %d rules / %d recording, want 3 / 64 / 15 (see demo/VENDOR.md)",
			len(body.Data.Groups), rules, recording)
	}
}

// TestEveryScrapeTargetIsUp catches the failure that makes every later
// assertion meaningless: a target that is not scraped contributes no
// metrics, so its metrics grade as absent rather than unread.
func TestEveryScrapeTargetIsUp(t *testing.T) {
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	get(t, "/api/v1/query?query=up", &body)

	up := map[string]bool{}
	for _, r := range body.Data.Result {
		up[r.Metric["job"]] = r.Value[1] == "1"
	}
	for _, job := range []string{"prometheus", "node", "loadgen"} {
		if !up[job] {
			t.Errorf("job %q is not up (targets seen: %v)", job, up)
		}
	}
}

// TestLoadgenExposesItsExactCardinality is the fixture's own contract,
// checked through Prometheus rather than against loadgen directly: this
// is the number sub-project C's aggregation proposal is measured by.
func TestLoadgenExposesItsExactCardinality(t *testing.T) {
	var body struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	get(t, "/api/v1/query?query=count(jetsam_demo_requests_total)", &body)

	if len(body.Data.Result) != 1 {
		t.Fatalf("count() returned %d results, want 1 -- is loadgen being scraped?", len(body.Data.Result))
	}
	if got := body.Data.Result[0].Value[1]; got != "400" {
		t.Errorf("jetsam_demo_requests_total has %v series, want exactly 400", got)
	}
}

// TestGrafanaServesBothDashboards proves the seam sub-project B depends
// on: a Grafana that is not merely running but reachable with a token and
// actually serving the dashboards that make up the corpus.
func TestGrafanaServesBothDashboards(t *testing.T) {
	token, err := os.ReadFile("grafana/.token")
	if err != nil {
		t.Fatalf("no grafana token, run `make demo-up`: %v", err)
	}
	req, _ := http.NewRequest("GET", "http://localhost:3000/api/search?type=dash-db", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("grafana unreachable, run `make demo-up`: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/search: status %d -- is the service account token valid?", resp.StatusCode)
	}
	var found []struct {
		Title string `json:"title"`
		UID   string `json:"uid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	titles := map[string]bool{}
	for _, d := range found {
		titles[d.Title] = true
	}
	for _, want := range []string{"Node Exporter Full", "loadgen"} {
		if !titles[want] {
			t.Errorf("dashboard %q is not provisioned (found: %v)", want, titles)
		}
	}
}

// queriedMetrics are the metrics demo/query.sh deliberately reads. A
// metric in this list must grade as queried once sub-project B ingests
// the log; a metric absent from it must not.
var queriedMetrics = []string{
	"node_cpu_seconds_total",
	"node_memory_MemAvailable_bytes",
	"jetsam_demo_requests_total",
}

// TestQueryLogCapturesTheKnownQueries proves the second seam sub-project
// B depends on: that the query log exists on the HOST (prometheus writes
// it inside its container) and that its contents are knowable.
//
// It also records what else lands there. Prometheus logs more than API
// queries, and B has to tell a human's read apart from the server's own
// rule evaluation -- a distinction that is invisible until you look at a
// real log, which is why this test prints the breakdown rather than only
// asserting.
func TestQueryLogCapturesTheKnownQueries(t *testing.T) {
	data, err := os.ReadFile("querylog/queries.log")
	if err != nil {
		t.Fatalf("no query log, run `make demo-up`: %v", err)
	}
	var apiQueries, ruleQueries int
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Params struct {
				Query string `json:"query"`
			} `json:"params"`
			RuleGroup *struct {
				Name string `json:"name"`
			} `json:"ruleGroup"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("query log line is not JSON: %q: %v", line, err)
		}
		if entry.RuleGroup != nil {
			ruleQueries++
			continue
		}
		apiQueries++
		for _, m := range queriedMetrics {
			if strings.Contains(entry.Params.Query, m) {
				seen[m] = true
			}
		}
	}
	t.Logf("query log: %d API queries, %d rule evaluations", apiQueries, ruleQueries)

	for _, m := range queriedMetrics {
		if !seen[m] {
			t.Errorf("query log does not contain a read of %s -- did demo/query.sh run?", m)
		}
	}
	if ruleQueries == 0 {
		t.Log("NOTE: no rule evaluations appear in the log. Sub-project B can treat every " +
			"entry as a real read; record this in VENDOR.md.")
	}
}
