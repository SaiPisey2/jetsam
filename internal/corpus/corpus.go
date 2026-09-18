// Package corpus collects every query anything is known to run against a
// Prometheus, and reports which metrics those queries touch.
package corpus

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
)

// Source is which kind of thing read a metric, as a bitmask: a metric is
// routinely read by more than one.
//
// It exists because a verdict's reason is read by someone deciding whether
// to permanently stop storing a metric, and "read by a rule" said of a
// metric no rule mentions is worse than no reason at all -- it points the
// reader at a file where they will not find it, and the natural conclusion
// from not finding it is that jetsam is wrong and the metric is droppable.
type Source uint8

const (
	// FromRule: a rule Prometheus evaluates reads it.
	FromRule Source = 1 << iota
	// FromDashboard: a Grafana panel or template variable reads it.
	FromDashboard
	// FromLog: a query recorded in the query log read it.
	FromLog
)

// Reason renders the sources as the sentence a report prints next to a
// metric. The empty Source renders "", which no used metric can have.
func (s Source) Reason() string {
	var parts []string
	if s&FromRule != 0 {
		parts = append(parts, "a rule")
	}
	if s&FromDashboard != 0 {
		parts = append(parts, "a dashboard")
	}
	if s&FromLog != 0 {
		parts = append(parts, "a logged query")
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return "read by " + parts[0]
	default:
		return "read by " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

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
	// QueryLogUnreadable is true when a query log path was configured and
	// could not be read. It is deliberately NOT the same as QueryLog being
	// nil: no log configured means jetsam never had negative evidence, while
	// an unreadable log means it should have had some and does not, which
	// must withhold every drop rather than quietly grade as if no log had
	// ever been asked for.
	QueryLogUnreadable bool
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
	// UsedBy is which sources read each metric in Used. Kept alongside Used
	// rather than replacing it because every caller that only asks "is this
	// read?" should not have to know about sources -- but a caller writing
	// the sentence a human approves a deletion against must.
	UsedBy map[string]Source
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
	// DashboardVariablesUnparsed names every dashboard template-variable
	// query that would not parse even after substitution -- kept separate
	// from DashboardPanelsUnparsed because a variable query is a different
	// kind of thing (almost always a Grafana template function like
	// label_values(...), not PromQL a panel would run) and reporting it
	// under the panel label would misleadingly suggest a panel is broken.
	//
	// Only its COUNT is surfaced to an operator, with that explanation
	// attached. Naming each entry would print three broken-looking lines on
	// every scan of a perfectly healthy install, because label_values() is
	// never PromQL and never will be.
	DashboardVariablesUnparsed []string
	// QueryLogUnparsed names every logged query that would not parse. The
	// query log is the one source whose SILENCE licenses a deletion, so a
	// query it could not read is the one kind of parse failure that must
	// not be invisible.
	QueryLogUnparsed []string

	LogRead bool
	LogSpan time.Duration
	// LogEnd is the timestamp of the log's latest entry. Span alone says
	// how much time the log covers, not when that coverage stopped: a glob
	// matching only last year's rotated archives satisfies any minimum
	// window while saying nothing about this year. Printing the end makes
	// that gap visible even though nothing yet refuses on it.
	LogEnd       time.Time
	LogQualifies bool
	// LogUnreadable is the configured-but-unreadable case -- see
	// Sources.QueryLogUnreadable.
	LogUnreadable bool
}

// LogShortfall says, in one clause, why the query log does not license a
// drop, or "" when it does. One sentence with one owner: four places used
// to phrase this independently and two of them said "no query log is
// configured" about a log that was configured and merely short.
func (c Corpus) LogShortfall() string {
	switch {
	case c.LogUnreadable:
		return "the query log is configured but could not be read"
	case !c.LogRead:
		return "no query log is configured"
	case !c.LogQualifies:
		return fmt.Sprintf("the query log covers only %s, short of the configured minimum",
			c.LogSpan.Round(time.Hour))
	default:
		return ""
	}
}

// LogEndText renders LogEnd for a human, or "unknown" when the log had no
// parseable bounds at all.
func (c Corpus) LogEndText() string {
	if c.LogEnd.IsZero() {
		return "unknown"
	}
	return c.LogEnd.UTC().Format(time.RFC3339)
}

// SourceList names the evidence sources that actually contributed queries,
// for the sentence that quotes Queries. Naming a source that is not
// configured overstates the corpus to the person approving a deletion.
func (c Corpus) SourceList() string {
	parts := []string{"rules"}
	if c.DashboardsConfigured && c.DashboardsReachable {
		parts = append(parts, "dashboards")
	}
	if c.LogRead {
		parts = append(parts, "the query log")
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// metricToken matches a metric-name-shaped token, for the conservative
// extraction applied to a query that will not parse.
var metricToken = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

func (c *Corpus) markUsed(metric string, src Source) {
	c.Used[metric] = true
	c.UsedBy[metric] |= src
}

// readQueries reads a batch of queries from one source.
//
// A query that will not parse even after substitution is CONTAINED, not
// fatal: one malformed dashboard panel, template variable or logged query
// in a large install must not make every drop unreachable. Its
// metric-name-shaped tokens are marked used where they name a metric this
// Prometheus actually stores, which over-protects rather than
// under-protects, and describe(q, err) names it in unparsed so the failure
// is reported rather than swallowed.
func (c *Corpus) readQueries(qs []string, src Source, known map[string]bool, allMetrics []string,
	describe func(q string, err error) string, unparsed *[]string) {
	for _, q := range qs {
		c.Queries++
		refs, err := Extract(Substitute(q))
		if err != nil {
			*unparsed = append(*unparsed, describe(q, err))
			for _, tok := range metricToken.FindAllString(q, -1) {
				if known[tok] {
					c.markUsed(tok, src)
				}
			}
			continue
		}
		for _, m := range refs.Resolve(allMetrics) {
			c.markUsed(m, src)
		}
	}
}

// Build reads every source and reports what it touches. Every rule
// Prometheus evaluates counts as a live consumer, so a metric read only by
// a recording rule is used, whether or not anything reads that rule's
// output. That is the safe direction: the alternative -- proving a
// recording rule dead and discounting it -- needs dashboard evidence.
//
// An unparseable RULE is fatal: it lands in Blocked and forbids every drop,
// because rules are Prometheus' own configuration, and one jetsam cannot
// read means it is misreading something fundamental. Every OTHER source
// goes through readQueries, which contains a parse failure instead of
// escalating it, marks the query's metric-shaped tokens used, and names it
// in a reported list. That includes a dashboard's own TEMPLATE-VARIABLE
// queries, which fail routinely because a template variable is usually a
// Grafana function like label_values(...) rather than PromQL, and logged
// queries, which fail rarely -- but the query log is the one source whose
// SILENCE licenses a deletion, so a query it could not read is exactly the
// failure that must not be silent.
func Build(src Sources, allMetrics []string) Corpus {
	c := Corpus{
		Used:                 map[string]bool{},
		UsedBy:               map[string]Source{},
		Produced:             map[string]bool{},
		Dashboards:           len(src.Dashboards),
		DashboardsConfigured: src.DashboardsConfigured,
		DashboardsReachable:  src.DashboardsReachable,
		LogUnreadable:        src.QueryLogUnreadable,
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
			c.markUsed(m, FromRule)
		}
	}

	known := make(map[string]bool, len(allMetrics))
	for _, m := range allMetrics {
		known[m] = true
	}

	for _, d := range src.Dashboards {
		title, uid := d.Title, d.UID
		c.readQueries(d.Queries, FromDashboard, known, allMetrics,
			func(q string, err error) string {
				return fmt.Sprintf("dashboard %s (%s): %v", title, uid, err)
			}, &c.DashboardPanelsUnparsed)

		// A dashboard reads metrics through its template-variable
		// definitions too -- typically label_values(metric, label), which
		// populates a "job"/"nodename"/"instance" dropdown -- and Grafana
		// issues that query to Prometheus every time the dashboard loads.
		// label_values(...) is a Grafana template function, not PromQL, so
		// Extract reliably fails on it and this reliably takes the
		// conservative fallback, named under its own label so a reader is
		// not misled into thinking a panel is broken.
		c.readQueries(d.VariableQueries, FromDashboard, known, allMetrics,
			func(q string, err error) string {
				return fmt.Sprintf("dashboard %s (%s): variable %s", title, uid, q)
			}, &c.DashboardVariablesUnparsed)
	}

	if src.QueryLog != nil {
		c.LogRead = true
		c.LogSpan = src.QueryLog.Span
		c.LogEnd = src.QueryLog.End
		c.LogQualifies = src.LogQualifies
		c.readQueries(src.QueryLog.Queries, FromLog, known, allMetrics,
			func(q string, err error) string {
				return fmt.Sprintf("query log: %v", err)
			}, &c.QueryLogUnparsed)
	}
	return c
}
