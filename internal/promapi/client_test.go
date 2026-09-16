package promapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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
	if len(got.Counts) != 2 {
		t.Fatalf("got %d metrics, want 2", len(got.Counts))
	}
	if got.Counts["go_gc_duration_seconds"] != 1258 {
		t.Errorf("go_gc_duration_seconds = %d, want 1258", got.Counts["go_gc_duration_seconds"])
	}
}

func TestTSDBStatusReturnsHeadSeriesAndNameCountFromTheSameResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{
			"headStats":{"numSeries":15777},
			"labelValueCountByLabelName":[{"name":"__name__","value":1374},{"name":"job","value":3}],
			"seriesCountByMetricName":[{"name":"go_gc_duration_seconds","value":1258}]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 5000)
	if err != nil {
		t.Fatalf("TSDBStatus: %v", err)
	}
	if got.HeadSeries != 15777 {
		t.Errorf("HeadSeries = %d, want 15777 (the response's headStats.numSeries)", got.HeadSeries)
	}
	if got.NameCount != 1374 {
		t.Errorf("NameCount = %d, want 1374 (the __name__ entry, not some other label)", got.NameCount)
	}
}

func TestTSDBStatusDetectsTruncationWhenCountsHitTheLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// limit=1 forces the response to look truncated at metric_limit,
		// mirroring what a real Prometheus does when limit is below its
		// actual metric-name count: it returns exactly `limit` rows.
		w.Write([]byte(`{"status":"success","data":{
			"headStats":{"numSeries":15777},
			"labelValueCountByLabelName":[{"name":"__name__","value":1374}],
			"seriesCountByMetricName":[{"name":"go_gc_duration_seconds","value":1258}]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("TSDBStatus: %v", err)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true: len(Counts) == limit means more metrics may exist")
	}
}

func TestTSDBStatusDetectsTruncationWhenNameCountExceedsCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{
			"headStats":{"numSeries":15777},
			"labelValueCountByLabelName":[{"name":"__name__","value":1374}],
			"seriesCountByMetricName":[{"name":"go_gc_duration_seconds","value":1258}]}}`))
	}))
	defer srv.Close()

	// limit=5000 is well above len(Counts) == 1, so the len(Counts)>=limit
	// signal alone would miss this; NameCount (1374) exceeding len(Counts)
	// (1) must catch it independently.
	got, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 5000)
	if err != nil {
		t.Fatalf("TSDBStatus: %v", err)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true: NameCount exceeds len(Counts)")
	}
}

func TestTSDBStatusNotTruncatedWhenCountsCoverEveryName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{
			"headStats":{"numSeries":2088},
			"labelValueCountByLabelName":[{"name":"__name__","value":2}],
			"seriesCountByMetricName":[
				{"name":"go_gc_duration_seconds","value":1258},
				{"name":"node_systemd_unit_state","value":830}]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 5000)
	if err != nil {
		t.Fatalf("TSDBStatus: %v", err)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false: Counts already covers every metric name")
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

func TestAlertingAndRecordingRulesReturnsBothKindsWithGroupAndType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"groups":[
			{"name":"group1","rules":[
				{"name":"HighErrorRate","query":"rate(errors[5m]) > 0.1","type":"alerting"},
				{"name":"job:requests:rate5m","query":"rate(requests_total[5m])","type":"recording"}
			]},
			{"name":"group2","rules":[]}
		]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).AlertingAndRecordingRules(context.Background())
	if err != nil {
		t.Fatalf("AlertingAndRecordingRules: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rules, want 2 (the empty group2 must contribute nothing)", len(got))
	}

	byName := make(map[string]Rule, len(got))
	for _, r := range got {
		byName[r.Name] = r
	}

	alert, ok := byName["HighErrorRate"]
	if !ok {
		t.Fatal("missing rule HighErrorRate")
	}
	if alert.Type != "alerting" {
		t.Errorf("HighErrorRate.Type = %q, want %q", alert.Type, "alerting")
	}
	if alert.Group != "group1" {
		t.Errorf("HighErrorRate.Group = %q, want %q", alert.Group, "group1")
	}
	if alert.Query != "rate(errors[5m]) > 0.1" {
		t.Errorf("HighErrorRate.Query = %q, want %q", alert.Query, "rate(errors[5m]) > 0.1")
	}

	rec, ok := byName["job:requests:rate5m"]
	if !ok {
		t.Fatal("missing rule job:requests:rate5m")
	}
	if rec.Type != "recording" {
		t.Errorf("job:requests:rate5m.Type = %q, want %q", rec.Type, "recording")
	}
	if rec.Group != "group1" {
		t.Errorf("job:requests:rate5m.Group = %q, want %q", rec.Group, "group1")
	}
}

func TestAlertingAndRecordingRulesRejectsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"bad rules query"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, 5*time.Second).AlertingAndRecordingRules(context.Background()); err == nil {
		t.Fatal("AlertingAndRecordingRules succeeded on a status:\"error\" body, want an error")
	}
}

func TestAlertingAndRecordingRulesOnAQuietPrometheusIsEmptyNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).AlertingAndRecordingRules(context.Background())
	if err != nil {
		t.Fatalf("AlertingAndRecordingRules: %v, want no error for a Prometheus with no rule groups", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rules, want 0", len(got))
	}
}

func TestQueryJobsForReturnsOnlySeriesCarryingAJobLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"result":[
			{"metric":{"job":"node-exporter","instance":"a"}},
			{"metric":{"job":"cadvisor","instance":"b"}},
			{"metric":{"instance":"c"}}
		]}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, 5*time.Second).QueryJobsFor(context.Background(), "up")
	if err != nil {
		t.Fatalf("QueryJobsFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d jobs %v, want 2 (the label-less series must be skipped, not counted as an empty job)", len(got), got)
	}
	sort.Strings(got)
	want := []string{"cadvisor", "node-exporter"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("jobs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, j := range got {
		if j == "" {
			t.Error("got an empty job value, want the label-less series skipped entirely")
		}
	}
}

func TestQueryJobsForRejectsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"invalid query"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, 5*time.Second).QueryJobsFor(context.Background(), "up"); err == nil {
		t.Fatal("QueryJobsFor succeeded on a status:\"error\" body, want an error")
	}
}

func TestGetErrorNeverEchoesUserinfoOrQueryString(t *testing.T) {
	// 127.0.0.1:1 is a reserved port nothing listens on, so this dials and
	// fails, exercising the *url.Error path from http.Client.Do. No
	// credential exists in v0.1, but a later version's request may carry
	// one (userinfo or a token query parameter), and the raw dial error
	// would otherwise put it straight into stderr, logs and CI output.
	_, err := New("http://user:sekrit@127.0.0.1:1", 2*time.Second).TSDBStatus(context.Background(), 500)
	if err == nil {
		t.Fatal("TSDBStatus succeeded against an unreachable address, want an error")
	}
	if strings.Contains(err.Error(), "sekrit") {
		t.Errorf("error leaks userinfo: %v", err)
	}
	if strings.Contains(err.Error(), "limit=") {
		t.Errorf("error leaks the query string: %v", err)
	}
}

func TestQueryJobsForQuotesAMetricNameThatIsAPromQLKeyword(t *testing.T) {
	const wantQuery = `count by (job) ({__name__="on"})`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != wantQuery {
			t.Errorf("query = %q, want %q: an unquoted metric name that is also a PromQL keyword produces a malformed query", got, wantQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()

	// "on" is a syntactically valid Prometheus metric name and also a
	// PromQL vector-matching keyword; unquoted interpolation breaks it.
	if _, err := New(srv.URL, 5*time.Second).QueryJobsFor(context.Background(), "on"); err != nil {
		t.Fatalf("QueryJobsFor: %v", err)
	}
}

func TestGetRejectsAnOversizedBodyRatherThanTruncating(t *testing.T) {
	// A body over the limit must produce a clear error, never a silently
	// truncated decode: a short read would look like "this Prometheus has
	// fewer metrics than it does," and every metric missing from that
	// truncated read is graded as unused downstream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Pad well past maxResponseBytes with whitespace before a body
		// that would otherwise decode fine.
		w.Write([]byte(`{"status":"success",`))
		pad := strings.Repeat(" ", 64<<20+1024)
		w.Write([]byte(pad))
		w.Write([]byte(`"data":{"seriesCountByMetricName":[]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, 30*time.Second).TSDBStatus(context.Background(), 500)
	if err == nil {
		t.Fatal("TSDBStatus succeeded against an oversized body, want an error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not report the size limit was exceeded: %v", err)
	}
}

// TestTSDBStatusRejectsANegativeSeriesCount: a series count cannot be
// negative. A hostile or compromised Prometheus sending one corrupts every
// number derived from it downstream -- the PR title's total, the
// percentage of the instance a drop represents -- so it must be rejected
// at this boundary rather than silently accepted and propagated.
func TestTSDBStatusRejectsANegativeSeriesCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
			{"name":"go_gc_duration_seconds","value":-5}]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 500)
	if err == nil {
		t.Fatal("TSDBStatus succeeded with a negative series count, want an error")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error does not say the series count is negative: %v", err)
	}
}

// TestTSDBStatusRejectsANegativeHeadSeries mirrors the above for
// headStats.numSeries, the true Prometheus-wide total that
// Inventory.TotalSeries and every percentage computed against it depend
// on.
func TestTSDBStatusRejectsANegativeHeadSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{
			"headStats":{"numSeries":-1},
			"seriesCountByMetricName":[]}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, 5*time.Second).TSDBStatus(context.Background(), 500)
	if err == nil {
		t.Fatal("TSDBStatus succeeded with a negative headStats.numSeries, want an error")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error does not say the head series count is negative: %v", err)
	}
}
