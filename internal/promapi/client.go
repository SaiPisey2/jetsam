// Package promapi is a typed client for the subset of the Prometheus HTTP
// API jetsam needs.
package promapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds every response body jetsam decodes. A legitimate
// TSDB status or rules response from even a very large Prometheus is
// orders of magnitude smaller than this; the bound exists to turn a
// compromised or misbehaving Prometheus's unbounded body into a clear
// error rather than either exhausting memory or, worse, silently
// truncating the JSON into a partial decode. A partial decode is the
// dangerous direction here: a short read looks like "this Prometheus has
// fewer metrics than it does", and every metric missing from that
// truncated read is then graded as unused downstream.
const maxResponseBytes = 64 << 20 // 64 MiB

// safeURL renders a URL for an error message without anything that could
// be a credential: no userinfo (url.URL.Host never includes it) and no
// query string, which may carry a token as a query parameter. Do not
// "simplify" this back to u.String() -- that would put the full request,
// including any secret, into stderr, logs and CI output.
func safeURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// Client reads a Prometheus HTTP API. It is read-only by construction:
// every method here is a GET.
type Client struct {
	base string
	hc   *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		hc:   &http.Client{Timeout: timeout},
	}
}

// envelope is the wrapper every Prometheus API response carries. A 200
// with Status "error" is the API's normal way of rejecting a request, so
// Status is checked on every call.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", path, err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// http.Client.Do wraps failures in a *url.Error whose Error()
		// embeds the complete request URL, query string included. No
		// credential exists in v0.1, but a later version's request may
		// carry one (a token query parameter or userinfo), and that must
		// never reach stderr, logs or CI output. Report the underlying
		// cause plus a scheme/host/path-only location instead of the
		// wrapper's own message.
		var uerr *url.Error
		cause := error(err)
		if errors.As(err, &uerr) {
			cause = uerr.Err
		}
		return fmt.Errorf("get %s: %w", safeURL(req.URL), cause)
	}
	defer resp.Body.Close()

	// Bound the read: see maxResponseBytes. Reading one byte past the
	// limit, rather than exactly at it, lets us tell "body was exactly
	// the limit" apart from "body was longer and got cut off" -- the
	// latter must be a clear error, never a silent truncated decode.
	lr := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		return fmt.Errorf("read %s (http %d): %w", safeURL(req.URL), resp.StatusCode, err)
	}
	if lr.N == 0 {
		return fmt.Errorf("%s: response body exceeds %d byte limit", safeURL(req.URL), maxResponseBytes)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode %s (http %d): %w", path, resp.StatusCode, err)
	}
	if env.Status != "success" {
		return fmt.Errorf("%s: %s: %s", path, env.ErrorType, env.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode %s data: %w", path, err)
	}
	return nil
}

type nameValue struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// Status is what /api/v1/status/tsdb reports, decoded into the shape the
// rest of jetsam needs. Counts is bounded by the limit passed to
// TSDBStatus; HeadSeries and NameCount are not -- they come from
// headStats.numSeries and the labelValueCountByLabelName entry for
// "__name__", both of which describe the WHOLE Prometheus regardless of
// how many entries Counts holds. A caller computing "how many series does
// this Prometheus store" by summing Counts is answering that question
// wrong the moment the metric list is truncated; HeadSeries is the
// number that is actually true.
type Status struct {
	// Counts is metric name -> series count, at most `limit` entries.
	Counts map[string]int
	// Limit is the limit this call was made with, carried through so a
	// caller can report "truncated at metric_limit=N" without threading
	// the config value through separately.
	Limit int
	// HeadSeries is headStats.numSeries: the true total number of series
	// this Prometheus holds right now, independent of limit.
	HeadSeries int
	// NameCount is the true number of distinct metric names this
	// Prometheus holds, independent of limit -- the labelValueCountByLabelName
	// entry for "__name__".
	NameCount int
	// Truncated is true when Counts does not cover every metric name:
	// either the call returned exactly `limit` entries (there may be more
	// Prometheus never sent), or NameCount exceeds len(Counts) outright.
	// A caller must not treat Counts as exhaustive when this is true.
	Truncated bool
}

// TSDBStatus returns every metric name with its series count, plus the
// head-level totals needed to tell whether that per-metric list was
// truncated.
//
// limit matters: without it the endpoint returns only the top ten entries,
// which would make every metric outside the top ten look like it does not
// exist. Pass a limit above the instance's distinct metric-name count --
// but do not assume it always is: Status.Truncated says whether it wasn't.
func (c *Client) TSDBStatus(ctx context.Context, limit int) (Status, error) {
	var data struct {
		HeadStats struct {
			NumSeries int `json:"numSeries"`
		} `json:"headStats"`
		LabelValueCountByLabelName []nameValue `json:"labelValueCountByLabelName"`
		SeriesCountByMetricName    []nameValue `json:"seriesCountByMetricName"`
	}
	q := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if err := c.get(ctx, "/api/v1/status/tsdb", q, &data); err != nil {
		return Status{}, err
	}
	out := make(map[string]int, len(data.SeriesCountByMetricName))
	for _, nv := range data.SeriesCountByMetricName {
		out[nv.Name] = nv.Value
	}
	nameCount := 0
	for _, nv := range data.LabelValueCountByLabelName {
		if nv.Name == "__name__" {
			nameCount = nv.Value
			break
		}
	}
	truncated := len(out) >= limit || nameCount > len(out)
	return Status{
		Counts:     out,
		Limit:      limit,
		HeadSeries: data.HeadStats.NumSeries,
		NameCount:  nameCount,
		Truncated:  truncated,
	}, nil
}

// Rule is one alerting or recording rule as Prometheus reports it.
type Rule struct {
	Name  string
	Query string
	Type  string // "alerting" or "recording"
	Group string
}

// AlertingAndRecordingRules returns every rule Prometheus currently
// evaluates, both kinds. Recording rules matter as much as alerting ones:
// a metric read only by a recording rule whose output is on a dashboard is
// in use, and dropping it silently breaks that rule.
func (c *Client) AlertingAndRecordingRules(ctx context.Context) ([]Rule, error) {
	var data struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Name  string `json:"name"`
				Query string `json:"query"`
				Type  string `json:"type"`
			} `json:"rules"`
		} `json:"groups"`
	}
	if err := c.get(ctx, "/api/v1/rules", nil, &data); err != nil {
		return nil, err
	}
	var out []Rule
	for _, g := range data.Groups {
		for _, r := range g.Rules {
			out = append(out, Rule{Name: r.Name, Query: r.Query, Type: r.Type, Group: g.Name})
		}
	}
	return out, nil
}

// QueryJobsFor returns the distinct job label values that currently expose
// metric. emit needs this to put a drop in the right scrape config: the
// TSDB status endpoint says how many series a metric has but never which
// job produces them.
func (c *Client) QueryJobsFor(ctx context.Context, metric string) ([]string, error) {
	var data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	}
	q := url.Values{"query": []string{fmt.Sprintf("count by (job) ({__name__=%q})", metric)}}
	if err := c.get(ctx, "/api/v1/query", q, &data); err != nil {
		return nil, err
	}
	var jobs []string
	for _, r := range data.Result {
		if j := r.Metric["job"]; j != "" {
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}
