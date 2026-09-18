package grafana

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stub serves the two endpoints the client uses.
func stub(t *testing.T, search string, byUID map[string]string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Unauthorized"}`)
			return
		}
		fmt.Fprint(w, search)
	})
	mux.HandleFunc("/api/dashboards/uid/", func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")
		body, ok := byUID[uid]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Dashboard not found"}`)
			return
		}
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok", 30*time.Second)
	c.Client = srv.Client()
	return c
}

// TestDashboardsWalksNestedPanels is the one that matters: a Grafana row
// carries its own panels array, so a flat pass over the top level misses most
// of a real dashboard's queries.
func TestDashboardsWalksNestedPanels(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[
			{"targets":[{"expr":"top_level_metric"}]},
			{"type":"row","panels":[
				{"targets":[{"expr":"nested_metric"}]},
				{"type":"row","panels":[{"targets":[{"expr":"deeply_nested_metric"}]}]}
			]}
		]}}`},
	)
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d dashboards, want 1", len(got))
	}
	want := map[string]bool{"top_level_metric": true, "nested_metric": true, "deeply_nested_metric": true}
	if len(got[0].Queries) != len(want) {
		t.Fatalf("got %d queries %v, want %d", len(got[0].Queries), got[0].Queries, len(want))
	}
	for _, q := range got[0].Queries {
		if !want[q] {
			t.Errorf("unexpected query %q", q)
		}
	}
}

// TestDashboardsCollectsVariableQueriesBothShapes pins the two shapes a
// template variable's query takes across a real dashboard's own
// templating.list: a plain string for a datasource variable, and an
// object with its own "query" field for a Prometheus query variable. The
// vendored dashboard uses both across its entries, so a reader that only
// handles one silently drops the other's metric references.
func TestDashboardsCollectsVariableQueriesBothShapes(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[],"templating":{"list":[
			{"name":"ds_prometheus","query":"prometheus"},
			{"name":"job","query":"label_values(node_uname_info, job)"},
			{"name":"node","query":{"query":"label_values(node_uname_info{job=\"$job\"}, instance)","refId":"Prometheus-node-Variable-Query"}}
		]}}}`},
	)
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d dashboards, want 1", len(got))
	}
	// None of these entries carries a "type", which is the shape this
	// test is about: an entry whose type is unknown is read, since
	// reading one extra string can only widen what looks used. A typed
	// datasource variable is excluded instead -- see
	// TestDatasourceVariablesAreNotCountedAsQueries.
	want := map[string]bool{
		"prometheus":                         true,
		"label_values(node_uname_info, job)": true,
		`label_values(node_uname_info{job="$job"}, instance)`: true,
	}
	if len(got[0].VariableQueries) != len(want) {
		t.Fatalf("VariableQueries = %v, want %d entries: %v", got[0].VariableQueries, len(want), want)
	}
	for _, q := range got[0].VariableQueries {
		if !want[q] {
			t.Errorf("unexpected variable query %q", q)
		}
	}
}

func TestDashboardsSkipsTargetsWithNoExpr(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[
			{"targets":[{"expr":""},{"refId":"B"},{"expr":"real_metric"}]}
		]}}`},
	)
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(got[0].Queries) != 1 || got[0].Queries[0] != "real_metric" {
		t.Errorf("Queries = %v, want [real_metric]", got[0].Queries)
	}
}

// A dashboard that vanishes between the search and the fetch must not take
// the whole run down -- but every other failure must.
func TestDashboardsToleratesOneVanishedDashboard(t *testing.T) {
	c := stub(t,
		`[{"uid":"gone","title":"Gone"},{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[{"targets":[{"expr":"m"}]}]}}`},
	)
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(got) != 1 || got[0].UID != "a" {
		t.Errorf("got %v, want just dashboard a", got)
	}
}

func TestDashboardsFailsOnBadCredential(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Unauthorized"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "wrong", 30*time.Second)
	c.Client = srv.Client()

	if _, err := c.Dashboards(context.Background()); err == nil {
		t.Fatal("want an error on 401, got nil -- an unreadable Grafana must never look like an empty one")
	}
}

// An empty Grafana and an unreachable one must be distinguishable: the first
// is a finding, the second is the absence of one.
func TestDashboardsReturnsEmptyNotErrorWhenGrafanaHasNone(t *testing.T) {
	c := stub(t, `[]`, map[string]string{})
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d dashboards, want 0", len(got))
	}
}

func TestErrorNeverCarriesTheToken(t *testing.T) {
	const token = "glsa_notarealtokenbutlongenough"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"message":"rejected Authorization: Bearer %s"}`, token)
	}))
	defer srv.Close()
	c := New(srv.URL, token, 30*time.Second)
	c.Client = srv.Client()

	_, err := c.Dashboards(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token is in the error text, which reaches stderr and CI logs: %v", err)
	}
}

// A JSON `null` at HTTP 200 is a no-op to json.Unmarshal, not an empty list --
// so treating it as a legitimate empty search result would make an unreadable
// Grafana indistinguishable from one with no dashboards, unprotecting every
// metric at once.
func TestANullSearchResponseIsAnError(t *testing.T) {
	c := stub(t, `null`, map[string]string{})
	got, err := c.Dashboards(context.Background())
	if err == nil {
		t.Fatalf("want an error for a null search response, got %d dashboards and nil error -- an empty result and an unreadable one must not be confused", len(got))
	}
}

// Same blind spot one level down: a dashboard body of {"dashboard": null}
// unmarshals without error and yields zero queries, silently protecting
// nothing while looking like a normal (if boring) dashboard.
func TestANullDashboardBodyIsAnError(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard": null}`},
	)
	if _, err := c.Dashboards(context.Background()); err == nil {
		t.Fatal("want an error for a null dashboard body, got nil")
	}
}

// TestDatasourceVariablesAreNotCountedAsQueries: a datasource variable's
// query field is the plain string "prometheus" -- a datasource name, not
// PromQL. Counting it inflates the query total a pull request quotes as
// its evidence, and it parses as a vector selector, so it also adds a
// phantom read of a metric called "prometheus".
func TestDatasourceVariablesAreNotCountedAsQueries(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[],"templating":{"list":[
			{"name":"ds_prometheus","type":"datasource","query":"prometheus"},
			{"name":"interval","type":"interval","query":"1m,5m"},
			{"name":"job","type":"query","query":"label_values(node_uname_info, job)"}
		]}}}`},
	)
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	want := []string{"label_values(node_uname_info, job)"}
	if len(got[0].VariableQueries) != len(want) || got[0].VariableQueries[0] != want[0] {
		t.Errorf("VariableQueries = %v, want %v", got[0].VariableQueries, want)
	}
}
