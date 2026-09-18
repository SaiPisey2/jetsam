// Package corpus collects every query anything is known to run against a
// Prometheus, and reports which metrics those queries touch.
package corpus

import (
	"fmt"
	"regexp"
	"time"

	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
)

// Sources is everything jetsam knows about who reads what. Only Rules is
// always present.
type Sources struct {
	Rules      []promapi.Rule
	Dashboards []grafana.Dashboard
	QueryLog   *querylog.Reading
	// LogQualifies is whether QueryLog's span met the configured minimum. The
	// caller decides this because the threshold is configuration; the corpus
	// only records the answer, so that nothing downstream can assert the
	// evidence exists without it existing.
	LogQualifies bool
	// DashboardsConfigured is true when a Grafana URL was set, whether or not
	// the fetch succeeded. Dashboards being nil while this is true is the
	// "configured but unreachable" case, which must withhold every drop.
	DashboardsConfigured bool
	DashboardsReachable  bool
}

// Corpus is every query jetsam knows about, reduced to the question that
// matters: which metrics does anything read?
type Corpus struct {
	// Queries is how many queries were read. The PR body quotes it, so a
	// reader can judge how much evidence a verdict rests on.
	Queries int
	// Used is every metric some query touches.
	Used map[string]bool
	// Produced is every metric a recording rule writes. v0.1 never
	// proposes dropping one: that would mean editing a rule file, not a
	// scrape config.
	Produced map[string]bool
	// Blocked holds FATAL problems only: a rule that does not parse. An
	// unparseable dashboard panel is contained, not fatal -- see
	// DashboardPanelsUnparsed.
	Blocked []string

	// Dashboards is how many dashboards were read, regardless of whether any
	// of their panels parsed. The report layer must not recompute something
	// the corpus already knows.
	Dashboards int

	DashboardsConfigured bool
	DashboardsReachable  bool
	// DashboardPanelsUnparsed names every dashboard panel that would not
	// parse even after substitution. Reported, never fatal.
	DashboardPanelsUnparsed []string

	LogRead      bool
	LogSpan      time.Duration
	LogQualifies bool
}

// metricToken matches a metric-name-shaped token, for the conservative
// extraction applied to a dashboard panel that will not parse.
var metricToken = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// Build reads every source and reports what it touches. Every rule
// Prometheus evaluates counts as a live consumer, so a metric read only by
// a recording rule is used, whether or not anything reads that rule's
// output. That is the safe direction: the alternative -- proving a
// recording rule dead and discounting it -- needs dashboard evidence.
//
// An unparseable RULE is fatal: it lands in Blocked and forbids every drop,
// because rules are Prometheus' own configuration, and one jetsam cannot
// read means it is misreading something fundamental. An unparseable
// DASHBOARD PANEL is contained instead: one malformed panel in a large
// Grafana install must not make every drop unreachable, so its
// metric-name-shaped tokens are conservatively marked used and the panel is
// named in DashboardPanelsUnparsed for reporting.
func Build(src Sources, allMetrics []string) Corpus {
	c := Corpus{
		Used:                 map[string]bool{},
		Produced:             map[string]bool{},
		Dashboards:           len(src.Dashboards),
		DashboardsConfigured: src.DashboardsConfigured,
		DashboardsReachable:  src.DashboardsReachable,
	}

	for _, r := range src.Rules {
		c.Queries++
		// What a recording rule WRITES comes from the rules API independently
		// of whether its query parses, so Produced is populated first and
		// stays protected even for a rule that ends up in Blocked.
		if r.Type == "recording" {
			c.Produced[r.Name] = true
		}
		refs, err := Extract(r.Query)
		if err != nil {
			c.Blocked = append(c.Blocked, fmt.Sprintf("rule %s/%s: %v", r.Group, r.Name, err))
			continue
		}
		for _, m := range refs.Resolve(allMetrics) {
			c.Used[m] = true
		}
	}

	known := make(map[string]bool, len(allMetrics))
	for _, m := range allMetrics {
		known[m] = true
	}

	for _, d := range src.Dashboards {
		for _, q := range d.Queries {
			c.Queries++
			refs, err := Extract(Substitute(q))
			if err != nil {
				// Contained, not fatal. One malformed panel in a large
				// Grafana install must not make every drop unreachable --
				// that is the trap the write path was already in. Mark the
				// metric-name-shaped tokens it mentions as used, which
				// over-protects rather than under-protects.
				c.DashboardPanelsUnparsed = append(c.DashboardPanelsUnparsed,
					fmt.Sprintf("dashboard %s (%s): %v", d.Title, d.UID, err))
				for _, tok := range metricToken.FindAllString(q, -1) {
					if known[tok] {
						c.Used[tok] = true
					}
				}
				continue
			}
			for _, m := range refs.Resolve(allMetrics) {
				c.Used[m] = true
			}
		}
	}

	if src.QueryLog != nil {
		c.LogRead = true
		c.LogSpan = src.QueryLog.Span
		c.LogQualifies = src.LogQualifies
		for _, q := range src.QueryLog.Queries {
			c.Queries++
			refs, err := Extract(q)
			if err != nil {
				// A logged query that does not parse is odd but not fatal:
				// Prometheus rejected it too, so it read nothing.
				continue
			}
			for _, m := range refs.Resolve(allMetrics) {
				c.Used[m] = true
			}
		}
	}
	return c
}
