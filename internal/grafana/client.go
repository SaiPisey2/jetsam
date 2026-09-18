// Package grafana reads dashboards from a Grafana instance, for the sole
// purpose of learning which metrics they query.
package grafana

import (
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
// queries its panels run.
type Dashboard struct {
	UID     string
	Title   string
	Queries []string
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
			Dashboard struct {
				UID    string  `json:"uid"`
				Title  string  `json:"title"`
				Panels []panel `json:"panels"`
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
		var qs []string
		collect(body.Dashboard.Panels, &qs)
		out = append(out, Dashboard{UID: h.UID, Title: h.Title, Queries: qs})
	}
	return out, nil
}
