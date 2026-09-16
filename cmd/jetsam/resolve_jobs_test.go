package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/promapi"
)

// manyJobsFixture stands up a fake Prometheus that answers /api/v1/query
// for n distinct metrics, each produced by its own distinct job, so a
// worker pool resolving them concurrently has real concurrency to race
// (n comfortably exceeds jobResolveWorkers).
func manyJobsFixture(t *testing.T, n int) (*promapi.Client, []string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		// The query embeds the metric name inside {__name__="metric_N"};
		// extract N by finding which metric_<i> substring it contains.
		for i := 0; i < n; i++ {
			// Quoted match, not a bare substring: "metric_1" is itself a
			// substring of "metric_10", "metric_11", etc., which would
			// otherwise match the wrong metric and make this fixture (not
			// resolveJobs) the source of a false failure.
			name := fmt.Sprintf(`"metric_%d"`, i)
			if strings.Contains(q, name) {
				fmt.Fprintf(w, `{"status":"success","data":{"result":[{"metric":{"job":"job_%d"}}]}}`, i)
				return
			}
		}
		t.Errorf("query %q did not match any known metric", q)
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	metrics := make([]string, n)
	for i := range metrics {
		metrics[i] = fmt.Sprintf("metric_%d", i)
	}
	return promapi.New(srv.URL, 5*time.Second), metrics
}

// TestResolveJobsIsDeterministicAcrossRepeatedRuns pins C1's determinism
// requirement: running the same bounded worker-pool resolution repeatedly
// against the same input, in one process, must produce byte-identical
// results every time -- an unstable order would make every PR diff look
// different between runs for no reason, even though nothing changed.
func TestResolveJobsIsDeterministicAcrossRepeatedRuns(t *testing.T) {
	cl, metrics := manyJobsFixture(t, 50)

	var first [][]string
	for run := 0; run < 20; run++ {
		got, err := resolveJobs(context.Background(), cl, metrics)
		if err != nil {
			t.Fatalf("run %d: resolveJobs: %v", run, err)
		}
		if run == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d: got %d results, want %d", run, len(got), len(first))
		}
		for i := range got {
			if !stringSlicesEqual(got[i], first[i]) {
				t.Fatalf("run %d: result[%d] = %v, want %v (matches run 0)", run, i, got[i], first[i])
			}
		}
	}
}

// TestResolveJobsMatchesEachMetricToItsOwnJob is a correctness check
// alongside the determinism one: with real concurrency (50 metrics against
// 8 workers), every metric's result must be ITS OWN job, never another
// metric's -- pinning that indexed-slice writes never cross wires between
// goroutines.
func TestResolveJobsMatchesEachMetricToItsOwnJob(t *testing.T) {
	cl, metrics := manyJobsFixture(t, 50)

	got, err := resolveJobs(context.Background(), cl, metrics)
	if err != nil {
		t.Fatalf("resolveJobs: %v", err)
	}
	for i, jobs := range got {
		want := fmt.Sprintf("job_%d", i)
		if len(jobs) != 1 || jobs[0] != want {
			t.Errorf("metrics[%d] = %q resolved to %v, want [%q]", i, metrics[i], jobs, want)
		}
	}
}

// TestResolveJobsPropagatesTheFirstError pins that a failure is never
// silently dropped: one metric's query fails, and resolveJobs must return
// an error naming it rather than a partial or empty success.
func TestResolveJobsPropagatesTheFirstError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "boom") {
			w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"synthetic failure"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"api"}}]}}`))
	}))
	defer srv.Close()

	cl := promapi.New(srv.URL, 5*time.Second)
	metrics := []string{"fine_a", "fine_b", "boom", "fine_c"}
	_, err := resolveJobs(context.Background(), cl, metrics)
	if err == nil {
		t.Fatal("resolveJobs succeeded despite one metric's query failing, want an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error does not name the metric that failed: %v", err)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
