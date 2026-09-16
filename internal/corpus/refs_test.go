package corpus

import (
	"sort"
	"testing"
)

func TestExtractNamesLiteralMetrics(t *testing.T) {
	r, err := Extract(`sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))`)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(r.Names) != 1 || r.Names[0] != "http_requests_total" {
		t.Errorf("Names = %v, want [http_requests_total] deduplicated", r.Names)
	}
	if r.MatchesEverything {
		t.Error("MatchesEverything = true, want false: the query names a metric")
	}
}

func TestExtractBareSelectorMatchesEverything(t *testing.T) {
	// `{job="api"}` names no metric at all: it selects every metric that
	// job exposes. Reading it as "references nothing" would mark all of
	// them droppable.
	r, err := Extract(`{job="api"} > 0`)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !r.MatchesEverything {
		t.Error("MatchesEverything = false, want true for a selector with no __name__")
	}
}

func TestResolveExpandsANameRegex(t *testing.T) {
	r, err := Extract(`{__name__=~"node_cpu.*"} > 0`)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := r.Resolve([]string{"node_cpu_seconds_total", "node_cpu_guest_seconds_total", "go_goroutines"})
	sort.Strings(got)
	want := []string{"node_cpu_guest_seconds_total", "node_cpu_seconds_total"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Resolve = %v, want %v", got, want)
	}
}

func TestExtractRejectsAnUnparseableQuery(t *testing.T) {
	// A Grafana panel carrying $__rate_interval is not valid PromQL. It
	// must surface as an error so the caller can block drops rather than
	// silently treating the query as referencing nothing.
	if _, err := Extract(`rate(http_requests_total[$__rate_interval])`); err == nil {
		t.Fatal("Extract succeeded on a template-variable query, want an error")
	}
}
