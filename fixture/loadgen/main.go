// Command loadgen exposes one metric whose cardinality is exact and whose
// labels are deliberately unequal in how much anything reads them.
//
// This is the only fixture in the project written with knowledge of what
// it is testing. It exists because no real exporter will guarantee a
// partially-used metric at a known cardinality, which is precisely the
// shape an aggregation proposal has to be checked against. Everything
// else the stack measures comes from upstream artifacts.
//
// The metric is jetsam_demo_requests_total{path,status,pod}: 5 x 4 x 20 =
// 400 series. One dashboard panel reads it, grouped by path alone, so
// path is required and status and pod are not. A correct aggregation
// proposal collapses 400 series to 5.
package main

import (
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var (
	paths    = []string{"/", "/api/v1/query", "/api/v1/series", "/healthz", "/metrics"}
	statuses = []string{"200", "400", "404", "500"}
	pods     = 20
)

// ticks drives the counters. It advances on a timer in production and is
// stepped directly by the tests, so the exposition is deterministic
// rather than wall-clock dependent -- a fixture whose numbers depend on
// how long the suite took is a fixture that fails intermittently.
var ticks atomic.Int64

func tick() { ticks.Add(1) }

func metrics(w http.ResponseWriter, _ *http.Request) {
	t := ticks.Load()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP jetsam_demo_requests_total Synthetic request counter. Only the path label is read by anything.")
	fmt.Fprintln(w, "# TYPE jetsam_demo_requests_total counter")
	for i, p := range paths {
		for j, s := range statuses {
			for k := 0; k < pods; k++ {
				// Distinct per series and strictly increasing in t, so
				// every series has its own rate and none is flat.
				v := t*int64(i+1) + int64(j*7+k)
				fmt.Fprintf(w, "jetsam_demo_requests_total{path=%q,status=%q,pod=\"pod-%02d\"} %d\n", p, s, k, v)
			}
		}
	}
}

func main() {
	go func() {
		for range time.Tick(time.Second) {
			tick()
		}
	}()
	http.HandleFunc("/metrics", metrics)
	addr := ":8081"
	fmt.Fprintf(os.Stderr, "loadgen listening on %s\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
