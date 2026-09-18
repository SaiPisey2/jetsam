package grafana

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		// Honour page the way Grafana does: everything on page 1, nothing
		// after it. A stub that served the same body for every page would
		// be a stub of a server that ignores pagination, and the client
		// refuses to page against one of those on purpose.
		if page := r.URL.Query().Get("page"); page != "" && page != "1" {
			fmt.Fprint(w, `[]`)
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
	// test is about: an entry with no type is read, since reading one
	// extra string can only widen what looks used. Only "datasource" and
	// "adhoc" are excluded -- see
	// TestDatasourceAndAdhocVariablesAreNotCollected.
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

// pagingStub serves /api/search with real pagination: it honours limit and
// page exactly as Grafana does, returning a short final page.
func pagingStub(t *testing.T, uids []string, serverLimit int) (*Client, *int) {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		calls++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if limit <= 0 || limit > serverLimit {
			// Grafana caps the limit server-side. A client that asks for
			// more than the cap gets the cap, not what it asked for --
			// which is why "I asked for everything" is not a substitute
			// for paging.
			limit = serverLimit
		}
		if page <= 0 {
			page = 1
		}
		start := (page - 1) * limit
		if start > len(uids) {
			start = len(uids)
		}
		end := start + limit
		if end > len(uids) {
			end = len(uids)
		}
		var hits []string
		for _, u := range uids[start:end] {
			hits = append(hits, fmt.Sprintf(`{"uid":%q,"title":%q}`, u, u))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(hits, ","))
	})
	mux.HandleFunc("/api/dashboards/uid/", func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")
		fmt.Fprintf(w, `{"dashboard":{"uid":%q,"title":%q,"panels":[{"targets":[{"expr":"metric_%s"}]}]}}`, uid, uid, uid)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok", 30*time.Second)
	c.Client = srv.Client()
	return c, &calls
}

// TestDashboardsPagesThroughTheSearch is the one that matters for a large
// install: /api/search is paginated and defaults to 1000 results, so a
// request with neither limit nor page silently returns a prefix. Every
// dashboard past the cut is then absent from the corpus, and the metrics
// only those dashboards read grade as unread -- a partial corpus reporting
// itself as complete, which is the one failure mode this package exists to
// prevent.
//
// The stub caps its page size server-side at 3, below what the client asks
// for, so the test fails unless the client actually pages: asking for a
// large limit is not enough.
func TestDashboardsPagesThroughTheSearch(t *testing.T) {
	var uids []string
	for i := 0; i < 7; i++ {
		uids = append(uids, fmt.Sprintf("d%d", i))
	}
	c, calls := pagingStub(t, uids, 3)

	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(got) != len(uids) {
		t.Fatalf("got %d dashboards, want %d -- the search was not paged through", len(got), len(uids))
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[d.UID] = true
	}
	for _, u := range uids {
		if !seen[u] {
			t.Errorf("dashboard %s is missing from a paged search", u)
		}
	}
	// 7 dashboards at a server-capped 3 per page: three pages of results
	// and a fourth, empty one that ends the loop. The cap is below what
	// the client asks for on purpose -- a short page is not proof of the
	// end when the server chooses the page size.
	if *calls != 4 {
		t.Errorf("/api/search called %d times, want 4 (pages of 3 over 7 dashboards, then an empty page)", *calls)
	}
}

// TestDashboardsSendsLimitAndPage pins the request shape itself. Grafana
// honours both parameters; omitting them is what caused the truncation.
func TestDashboardsSendsLimitAndPage(t *testing.T) {
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		fmt.Fprint(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok", 30*time.Second)
	c.Client = srv.Client()

	if _, err := c.Dashboards(context.Background()); err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	if len(queries) != 1 {
		t.Fatalf("got %d search requests, want 1 (an empty first page ends the loop): %v", len(queries), queries)
	}
	for _, want := range []string{"type=dash-db", "limit=", "page=1"} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("search query %q does not contain %q", queries[0], want)
		}
	}
}

// TestSearchHitWithNoUIDIsAnError: a search array carrying [null] or [{}]
// decodes into a zero-value hit. Fetching it would 404, and the 404 path
// means "a dashboard was deleted between the search and the fetch", which
// is a thing jetsam is entitled to shrug at. A search response that is not
// what it claims to be is not.
func TestSearchHitWithNoUIDIsAnError(t *testing.T) {
	for _, body := range []string{`[null]`, `[{}]`, `[{"uid":"a","title":"A"},{"title":"no uid"}]`} {
		c := stub(t, body, map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[]}}`})
		got, err := c.Dashboards(context.Background())
		if err == nil {
			t.Errorf("search body %s returned %d dashboards and no error, want an error", body, len(got))
		}
	}
}

// TestDatasourceAndAdhocVariablesAreNotCollected pins the whole deny-list.
// A datasource variable's query is the plain string "prometheus" -- a
// datasource name that parses as a vector selector, so collecting it both
// inflates the query total a pull request quotes as its evidence and adds
// a phantom read of a metric called "prometheus". An adhoc variable's
// value is a set of label filters. Those two are the only types whose
// values are never metric names; everything else is read.
func TestDatasourceAndAdhocVariablesAreNotCollected(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[],"templating":{"list":[
			{"name":"ds_prometheus","type":"datasource","query":"prometheus"},
			{"name":"filters","type":"adhoc","query":"prometheus"},
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

// TestSearchThatIgnoresPagingIsAnError: a server that returns page 1 for
// every page would otherwise be paged over until maxSearchPages, or
// forever under a naive loop. Refusing is right rather than merely safe --
// a Grafana that ignores the page parameter cannot be enumerated at all,
// so its dashboard list is unknown, not short.
func TestSearchThatIgnoresPagingIsAnError(t *testing.T) {
	var body []string
	for i := 0; i < 3; i++ {
		body = append(body, fmt.Sprintf(`{"uid":"d%d","title":"D"}`, i))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.Join(body, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok", 30*time.Second)
	c.Client = srv.Client()

	if _, err := c.Dashboards(context.Background()); err == nil {
		t.Error("a search that ignores the page parameter returned no error")
	}
}

// TestConstantAndCustomVariablesAreCollected is the loss case the
// type allow-list introduced. A constant, custom or textbox variable
// routinely holds a metric name -- that is what they are for -- and a
// panel consuming $metric{job="x"} does not rescue them: that expression
// fails Extract, and the lexical fallback recovers only "metric", "job"
// and "x". So a metric named ONLY in one of these variables becomes
// droppable, which is precisely the failure the variable-reading work
// exists to prevent.
func TestConstantAndCustomVariablesAreCollected(t *testing.T) {
	c := stub(t,
		`[{"uid":"a","title":"A"}]`,
		map[string]string{"a": `{"dashboard":{"uid":"a","title":"A","panels":[],"templating":{"list":[
			{"name":"metric","type":"constant","query":"node_uname_info"},
			{"name":"pair","type":"custom","query":"node_cpu_seconds_total,up"},
			{"name":"free","type":"textbox","query":"node_memory_MemFree_bytes"},
			{"name":"weird","type":"something_new","query":"future_metric_total"},
			{"name":"job","type":"query","query":"label_values(node_uname_info, job)"}
		]}}}`},
	)
	got, err := c.Dashboards(context.Background())
	if err != nil {
		t.Fatalf("Dashboards: %v", err)
	}
	want := map[string]bool{
		"node_uname_info":                    true,
		"node_cpu_seconds_total,up":          true,
		"node_memory_MemFree_bytes":          true,
		"future_metric_total":                true,
		"label_values(node_uname_info, job)": true,
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
