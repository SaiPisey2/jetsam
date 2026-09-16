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

// TestExtractReturnsNoRefsAlongsideAnError pins the other half of Extract's
// error contract: not just that an unparseable query returns an error, but
// that it returns a completely empty Refs alongside it. Callers rely on
// this: corpus.Build's Blocked path skips a failed rule with `continue`,
// but even if that guard were ever removed, only the zero value here keeps
// a blocked rule from silently contributing reads -- a populated Refs
// (Names, NameMatchers, or MatchesEverything) returned alongside an error
// would let a rule jetsam could not read mark metrics used regardless of
// what the caller does with the error. This must be pinned here, where the
// contract is made, rather than left as an untested accident that
// corpus.Build merely happens to depend on.
func TestExtractReturnsNoRefsAlongsideAnError(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"grafana template variable", `rate(http_requests_total[$__rate_interval])`},
		{"empty string", ``},
		{"truncated expression", `sum(`},
		{"unterminated selector", `http_requests_total{`},
		{"no non-empty matcher", `{__name__!="foo"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := Extract(tc.query)
			if err == nil {
				t.Fatalf("Extract(%q) succeeded, want an error", tc.query)
			}
			if len(refs.Names) != 0 {
				t.Errorf("Names = %v, want empty alongside an error", refs.Names)
			}
			if len(refs.NameMatchers) != 0 {
				t.Errorf("NameMatchers = %v, want empty alongside an error", refs.NameMatchers)
			}
			if refs.MatchesEverything {
				t.Error("MatchesEverything = true, want false alongside an error")
			}
		})
	}
}
