// Package grafana reads dashboards from a Grafana instance, for the sole
// purpose of learning which metrics they query.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds every response body. A Grafana dashboard is large
// -- the most popular one in existence is 468 KB -- but not unbounded, and a
// misbehaving or compromised endpoint must produce a clear error rather than
// exhaust memory or, worse, hand json.Unmarshal a silently truncated body. A
// truncated dashboard is the dangerous direction: every query it loses is a
// metric that then grades as unread.
const maxResponseBytes = 64 << 20 // 64 MiB

// safeURL renders a URL for an error message without anything that could be a
// credential: no userinfo, no query string. Do not "simplify" this to
// u.String().
func safeURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// apiError is a non-2xx response, carrying the status as a number. Callers
// compare the code; they never search the message text. A repository, folder
// or dashboard whose NAME contains "404" is an ordinary thing to exist, and
// substring matching on an error that embeds the request path turns that into
// a wrong answer.
type apiError struct {
	Path       string
	StatusCode int
	Status     string
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GET %s: %s: %s", e.Path, e.Status, e.Body)
}

func statusCode(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// Dashboard is one dashboard reduced to the only thing jetsam needs: the
// queries its panels run, plus the queries its own template-variable
// definitions run.
//
// A dashboard reads metrics through its variables too, deliberately, not
// as an afterthought: a "job"/"nodename"/"instance" dropdown is typically
// backed by a label_values(metric, label) query, and Grafana issues that
// query to Prometheus every time the dashboard loads. A metric named only
// there -- never in any panel's target expr -- is still a real read, and
// dropping it breaks every panel whose variable resolves through it. See
// the vendored "Node Exporter Full" dashboard's job/nodename/node
// variables, which all query label_values(node_uname_info, ...): no
// panel names node_uname_info at all.
type Dashboard struct {
	UID     string
	Title   string
	Queries []string
	// VariableQueries holds each template variable's own query, kept
	// separate from Queries rather than merged into it: a variable query
	// is a different kind of thing (nearly always a Grafana template
	// function like label_values(...), not PromQL a panel would run), and
	// a caller reporting what it could not parse should be able to say
	// which kind it was instead of implying a panel is broken.
	VariableQueries []string
}

type Client struct {
	base   string
	token  string
	Client *http.Client
}

func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		token:  token,
		Client: &http.Client{Timeout: timeout},
	}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		// http.Client.Do wraps failures in a *url.Error whose Error() embeds
		// the complete request URL. Report the cause plus a
		// scheme/host/path-only location instead.
		var uerr *url.Error
		cause := error(err)
		if errors.As(err, &uerr) {
			cause = uerr.Err
		}
		return fmt.Errorf("get %s: %w", safeURL(req.URL), cause)
	}
	defer resp.Body.Close()

	lr := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if lr.N == 0 {
		return fmt.Errorf("%s: response body exceeds %d byte limit", path, maxResponseBytes)
	}
	if resp.StatusCode >= 300 {
		// A proxy can reflect the Authorization header back into an error
		// body. This error reaches stderr and CI logs.
		text := strings.TrimSpace(string(body))
		if c.token != "" {
			text = strings.ReplaceAll(text, c.token, "[REDACTED]")
		}
		return &apiError{Path: path, StatusCode: resp.StatusCode, Status: resp.Status, Body: text}
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return fmt.Errorf("%s: response body was JSON null, not a value -- refusing to treat an unreadable Grafana as an empty one", path)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

type searchHit struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
}

type panel struct {
	Panels  []panel `json:"panels"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

func collect(ps []panel, out *[]string) {
	for _, p := range ps {
		for _, t := range p.Targets {
			if t.Expr != "" {
				*out = append(*out, t.Expr)
			}
		}
		// Rows carry their own panels. A flat pass misses most of a real
		// dashboard.
		collect(p.Panels, out)
	}
}

// templateVar is one entry of a dashboard's templating.list. Its query
// field takes two shapes across a real dashboard: a plain string for a
// datasource variable ("prometheus"), and an object carrying its own
// "query" field for a Prometheus query variable
// ({"query":"label_values(...)","refId":"..."}). Decoding it as
// json.RawMessage and trying both shapes handles either without needing
// to know up front which one a given entry uses.
type templateVar struct {
	Type  string          `json:"type"`
	Query json.RawMessage `json:"query"`
}

// isQueryVar reports whether a template variable's query field is meant to
// be a query at all. A datasource variable's query is the plain string
// "prometheus" -- a datasource name, not PromQL -- and counting it inflates
// the query total a pull request quotes as its evidence. An entry with no
// type at all is treated as a query: unknown shapes err towards being read,
// since reading one extra string can only widen what looks used.
func isQueryVar(v templateVar) bool {
	return v.Type == "" || v.Type == "query"
}

// variableQuery extracts one template variable's query text, handling
// both shapes Query can take. It returns "" for a shape it does not
// recognise (a plain datasource name is not a query at all) rather than
// erroring: template variables not shaped like a Prometheus query are
// ordinary, not a sign of a malformed dashboard.
func variableQuery(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Query
	}
	return ""
}

// Dashboards returns every dashboard in the instance, with its panel queries.
//
// Every dashboard is read; there is no folder or tag filter. More corpus means
// fewer false drops, and a filter is a way to accidentally exclude the
// dashboard that would have saved a metric.
func (c *Client) Dashboards(ctx context.Context) ([]Dashboard, error) {
	var hits []searchHit
	if err := c.get(ctx, "/api/search?type=dash-db", &hits); err != nil {
		return nil, fmt.Errorf("list dashboards: %w", err)
	}

	out := make([]Dashboard, 0, len(hits))
	for _, h := range hits {
		var body struct {
			Dashboard *struct {
				UID        string  `json:"uid"`
				Title      string  `json:"title"`
				Panels     []panel `json:"panels"`
				Templating struct {
					List []templateVar `json:"list"`
				} `json:"templating"`
			} `json:"dashboard"`
		}
		path := "/api/dashboards/uid/" + url.PathEscape(h.UID)
		if err := c.get(ctx, path, &body); err != nil {
			// A dashboard deleted between the search and this fetch is not a
			// reason to abandon the run. Every other failure is: an
			// unreadable Grafana must never look like one with fewer
			// dashboards, because that is indistinguishable from a metric
			// nothing reads.
			if statusCode(err) == http.StatusNotFound {
				continue
			}
			return nil, fmt.Errorf("read dashboard %s: %w", h.UID, err)
		}
		if body.Dashboard == nil {
			return nil, fmt.Errorf("read dashboard %s: response contained no dashboard object", h.UID)
		}
		var qs []string
		collect(body.Dashboard.Panels, &qs)

		var vqs []string
		for _, v := range body.Dashboard.Templating.List {
			if !isQueryVar(v) {
				continue
			}
			if q := variableQuery(v.Query); q != "" {
				vqs = append(vqs, q)
			}
		}

		out = append(out, Dashboard{UID: h.UID, Title: h.Title, Queries: qs, VariableQueries: vqs})
	}
	return out, nil
}
