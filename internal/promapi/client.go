// Package promapi is a typed client for the subset of the Prometheus HTTP
// API jetsam needs.
package promapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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
		return fmt.Errorf("get %s: %w", path, err)
	}
	defer resp.Body.Close()

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
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

// TSDBStatus returns every metric name with its series count.
//
// limit matters: without it the endpoint returns only the top ten entries,
// which would make every metric outside the top ten look like it does not
// exist. Pass a limit above the instance's distinct metric-name count.
func (c *Client) TSDBStatus(ctx context.Context, limit int) (map[string]int, error) {
	var data struct {
		SeriesCountByMetricName []nameValue `json:"seriesCountByMetricName"`
	}
	q := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if err := c.get(ctx, "/api/v1/status/tsdb", q, &data); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(data.SeriesCountByMetricName))
	for _, nv := range data.SeriesCountByMetricName {
		out[nv.Name] = nv.Value
	}
	return out, nil
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
	q := url.Values{"query": []string{fmt.Sprintf("count by (job) (%s)", metric)}}
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
