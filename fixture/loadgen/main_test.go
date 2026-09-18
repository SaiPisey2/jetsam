package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// The whole point of this service is that its cardinality is exact and
// knowable. 5 paths x 4 statuses x 20 pods = 400 series, always.
func TestExposesExactlyFourHundredSeries(t *testing.T) {
	rec := httptest.NewRecorder()
	metrics(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	series := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "jetsam_demo_requests_total{") {
			series++
		}
	}
	if series != 400 {
		t.Errorf("exposed %d series, want exactly 400", series)
	}
}

// Every series must carry all three labels. Sub-project C proposes
// collapsing this metric to the one label a dashboard reads, and that
// test is meaningless if the other two are not actually present.
func TestEverySeriesCarriesAllThreeLabels(t *testing.T) {
	rec := httptest.NewRecorder()
	metrics(rec, httptest.NewRequest("GET", "/metrics", nil))

	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "jetsam_demo_requests_total{") {
			continue
		}
		for _, label := range []string{"path=", "status=", "pod="} {
			if !strings.Contains(line, label) {
				t.Fatalf("series is missing %s: %s", label, line)
			}
		}
	}
}

// A counter that never moves makes rate() zero and the dashboard panel
// empty, which would make the fixture look used when it is not.
func TestCountersAreMonotonic(t *testing.T) {
	first := scrapeValue(t, "/", "200", "pod-00")
	tick()
	second := scrapeValue(t, "/", "200", "pod-00")
	if second <= first {
		t.Errorf("counter did not advance: %d then %d", first, second)
	}
}

func scrapeValue(t *testing.T, path, status, pod string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	want := `jetsam_demo_requests_total{path="` + path + `",status="` + status + `",pod="` + pod + `"} `
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, want) {
			var v int
			if _, err := fmt.Sscan(strings.TrimPrefix(line, want), &v); err != nil {
				t.Fatalf("parse value from %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("series not found: %s", want)
	return 0
}
