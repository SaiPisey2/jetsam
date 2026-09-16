package promapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTSDBStatusReturnsSeriesCountsByMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "500" {
			t.Errorf("limit = %q, want 500: the endpoint returns only 10 entries without it", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
			{"name":"go_gc_duration_seconds","value":1258},
			{"name":"node_systemd_unit_state","value":830}]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 500)
	if err != nil {
		t.Fatalf("TSDBStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d metrics, want 2", len(got))
	}
	if got["go_gc_duration_seconds"] != 1258 {
		t.Errorf("go_gc_duration_seconds = %d, want 1258", got["go_gc_duration_seconds"])
	}
}

func TestTSDBStatusRejectsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"limit too large"}`))
	}))
	defer srv.Close()

	// A 200 carrying status:"error" is the Prometheus API's normal way of
	// reporting a bad request. Treating it as success yields an empty
	// inventory, which reads as "no metrics exist" -- and every metric then
	// looks unused.
	if _, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 500); err == nil {
		t.Fatal("TSDBStatus succeeded on a status:\"error\" body, want an error")
	}
}
