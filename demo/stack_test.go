//go:build integration

package demo

import (
	"encoding/json"
	"net/http"
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
