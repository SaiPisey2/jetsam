// Package corpus collects every query anything is known to run against a
// Prometheus, and reports which metrics those queries touch.
package corpus

import (
	"fmt"
	"regexp"
	"sort"
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
	// Needs is what the corpus as a whole requires of each metric's labels,
	// the union of every consumer that touches it. A metric gets an entry
	// here for exactly the same reason it gets one in Used: some query
	// touches it. A MISSING key means "nothing in the corpus touches this
	// metric" -- it never means "this metric needs nothing". That distinction
	// matters because the zero MetricNeed, {All: false, Required: nil}, reads
	// exactly like "needs no labels, safe to aggregate away entirely," so a
	// caller must look the metric up in Used (or Needs itself) before ever
	// treating a missing entry as permission to collapse anything.
	Needs map[string]MetricNeed
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
			c.LogSpanText())
	default:
		return ""
	}
}

// FormatSpan renders a duration for the sentence an operator reads to
// decide whether a deletion is justified, honestly at every scale.
//
// This used to be Round(time.Hour), which prints "0s" for anything under
// thirty minutes. A qualifying log covering twenty minutes therefore
// reported zero coverage while licensing drops -- which does not read as a
// rounding artifact, it reads as evidence that does not exist. At the other
// end the same call printed a month as "744h0m0s", which is technically
// true and unreadable.
//
// Sub-second spans keep their own units rather than rounding, so that no
// non-zero span can ever render as "0s". Only a genuinely zero span does.
func FormatSpan(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.String()
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	mins := int64(d.Round(time.Minute) / time.Minute)
	days, rem := mins/(60*24), mins%(60*24)
	hours, minutes := rem/60, rem%60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// LogSpanText renders how much time the query log covers. One owner, so
// the report and the pull request body cannot drift.
func (c Corpus) LogSpanText() string {
	return FormatSpan(c.LogSpan)
}

// LogEndText renders LogEnd for a human, or "unknown" when the log had no
// parseable bounds at all.
func (c Corpus) LogEndText() string {
	if c.LogEnd.IsZero() {
		return "unknown"
	}
	return c.LogEnd.UTC().Format(time.RFC3339)
}

// DashboardsRead reports whether dashboard evidence actually contributed.
// Configured-but-unreachable contributed nothing, so it counts as not
// consulted -- the one predicate behind every sentence that mentions
// dashboards, so no two of them can disagree about whether a dashboard was
// ever looked at.
func (c Corpus) DashboardsRead() bool {
	return c.DashboardsConfigured && c.DashboardsReachable
}

// Readers names the sources a "nothing reads it" sentence is entitled to
// claim it checked. Saying "no rule or dashboard reads it" on an install
// with no grafana.url asserts a search that never happened, on the row
// that proposes a deletion.
func (c Corpus) Readers() string {
	if c.DashboardsRead() {
		return "rule or dashboard"
	}
	return "rule"
}

// SourceList names the evidence sources that actually contributed queries,
// for the sentence that quotes Queries. Naming a source that is not
// configured overstates the corpus to the person approving a deletion.
func (c Corpus) SourceList() string {
	parts := []string{"rules"}
	if c.DashboardsRead() {
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

// MetricNeed is what the whole corpus requires of one metric: the union of
// every consumer's requirements. Aggregation is possible only when this says
// so, and it says so only when every consumer agrees.
type MetricNeed struct {
	All        bool     // some query needs every label
	Required   []string // sorted union across every query, when All is false
	Ops        []string // sorted distinct operators seen, for the report
	AllOpsSafe bool     // every operator seen composes
	Blockers   []string // human-readable reasons aggregation is impossible
	// Fns is the sorted distinct set of range functions applied to this
	// metric beneath an aggregation, and Windows their ranges as Prometheus
	// duration strings. The EMPTY STRING is a member of Fns when some
	// consumer reads the metric's instant vector directly, so a corpus that
	// mixes `sum(m)` with `sum(rate(m[5m]))` yields two entries and refuses.
	// Dropping the empty string would let that mix look unanimous.
	Fns     []string
	Windows []string
}

// window renders a range as the duration string Prometheus writes, so 5m0s
// reads as 5m and appears that way in a rule name.
//
// Do NOT build this by trimming suffixes off time.Duration.String(): "10m0s"
// trimmed of "0s" and then of "0m" yields "1".
func window(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	units := []struct {
		name string
		size time.Duration
	}{
		{"d", 24 * time.Hour},
		{"h", time.Hour},
		{"m", time.Minute},
		{"s", time.Second},
		{"ms", time.Millisecond},
	}
	var b strings.Builder
	for _, u := range units {
		if n := d / u.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.size
		}
	}
	if d > 0 {
		// Sub-millisecond remainder. Prometheus has no unit for it, so
		// report the raw form rather than silently round the window a
		// rule name is about to claim.
		return d.String()
	}
	return b.String()
}

// needAccumulator folds one query's LabelNeed into a metric's running union.
type needAccumulator struct {
	all      bool
	required map[string]bool
	ops      map[string]bool
	opsSafe  bool
	blockers []string
	// fns and windows fold LabelNeed.Fn/Window the same way ops folds Op --
	// see MetricNeed.Fns for why the empty string is a real member rather
	// than an absence.
	fns     map[string]bool
	windows map[string]bool
}

// ensureAccumulator returns the running union for metric, creating it on
// first touch. opsSafe starts true so that ANDing in each query's OpSafe
// only ever turns it false -- an accumulator nothing has spoken against yet
// must not look like one an unsafe operator has already condemned.
func ensureAccumulator(needs map[string]*needAccumulator, metric string) *needAccumulator {
	acc := needs[metric]
	if acc == nil {
		acc = &needAccumulator{
			required: map[string]bool{}, ops: map[string]bool{}, opsSafe: true,
			fns: map[string]bool{}, windows: map[string]bool{},
		}
		needs[metric] = acc
	}
	return acc
}

// addBlocker records msg on acc unless it is already there. A dashboard with
// many broken panels that all mention the same metric would otherwise pile up
// one near-identical entry per panel in the one slice a report prints, which
// buries the single fact an operator needs -- that this metric cannot be
// aggregated -- under a wall of repeats of it.
func addBlocker(acc *needAccumulator, msg string) {
	for _, b := range acc.blockers {
		if b == msg {
			return
		}
	}
	acc.blockers = append(acc.blockers, msg)
}

// foldUnreadable records that a query jetsam could not parse might touch
// metric. It is folded as All -- the same refusal LabelsNeeded itself
// defaults to on any doubt -- because a query this unreadable might need any
// label at all, and under-claiming here is exactly the mistake this file
// exists to prevent.
func foldUnreadable(needs map[string]*needAccumulator, metric, query string) {
	acc := ensureAccumulator(needs, metric)
	acc.all = true
	addBlocker(acc, fmt.Sprintf("could not parse a query that may touch it: %s", query))
	// Every accumulator this file creates must end up with a non-empty Fns
	// -- see the invariant note in foldNeeds -- and this is one of the two
	// places (besides foldNeeds' own !Touches branch) that creates one
	// without ever seeing a LabelNeed to fold a real Fn from.
	acc.fns[""] = true
}

// foldNeeds folds one query's requirement for each of its metrics into the
// corpus-wide union. metrics is the set that Resolve already attributed to
// this query, so every metric here is one Used will also carry -- but that
// is Resolve's answer to "which metrics could this query affect", worked out
// from the selector shape (including MatchesEverything, a name-less selector
// like {job="api"} that Resolve conservatively charges against every metric
// in the inventory). LabelsNeeded answers a narrower question -- "does this
// query name this particular metric" -- and for a MatchesEverything selector
// the two disagree by construction: LabelsNeeded finds no name binding for
// ANY metric and reports Touches: false for all of them.
//
// That disagreement must not read as "needs nothing": Touches: false and "no
// label needed" are both the zero LabelNeed, and collapsing them would let a
// query like `{job="api"} > 5` license deleting labels from every metric that
// selector might actually be reading. So an untouched metric here refuses
// instead of being skipped -- Resolve already put it in Used, and Needs must
// carry an entry for everything in Used (see the comment on Corpus.Needs), so
// silently skipping it would leave that promise broken as well as the label
// data unprotected.
func foldNeeds(needs map[string]*needAccumulator, query string, metrics []string) {
	for _, m := range metrics {
		need, err := LabelsNeeded(query, m)
		if err != nil {
			foldUnreadable(needs, m, query)
			continue
		}
		acc := ensureAccumulator(needs, m)
		if !need.Touches {
			acc.all = true
			addBlocker(acc, fmt.Sprintf("could not confirm which labels this query needs: %s", query))
			// See the invariant note below: this branch creates/touches an
			// accumulator without a LabelNeed worth folding a real Fn from,
			// so it records the empty string itself rather than leaving
			// Fns to end up empty.
			acc.fns[""] = true
			continue
		}
		if need.All {
			acc.all = true
		}
		// need.Blocker names the SPECIFIC reason for a refusal LabelsNeeded
		// can explain -- an unsupported range function, a subquery, a join
		// below the aggregate, conflicting claims within one query -- so
		// that decide.refuse's "some consumer needs every label" sentence
		// carries the real cause instead of a true-but-useless generic one.
		if need.Blocker != "" {
			addBlocker(acc, need.Blocker)
		}
		for _, r := range need.Required {
			acc.required[r] = true
		}
		if need.Op != "" {
			acc.ops[need.Op] = true
			if !need.OpSafe {
				acc.opsSafe = false
			}
		}
		// need.Fn is folded unconditionally, empty string included, unlike
		// need.Op above: MetricNeed.Fns' contract is that the empty string
		// IS a meaningful member (the instant-vector case), where Ops has
		// no such member because an unaggregated query never reaches here
		// with an Op at all. This is also what makes the invariant "a
		// metric with a Needs entry always has a non-empty Fns" hold: every
		// path that calls ensureAccumulator for a metric (this one,
		// foldNeeds' !Touches branch above, and foldUnreadable) adds to fns
		// before returning, so a MetricNeed built by needsToCorpus can never
		// carry an empty Fns.
		acc.fns[need.Fn] = true
		if need.Fn != "" {
			acc.windows[window(need.Window)] = true
		}
	}
}

// needsToCorpus converts the running per-metric unions into the sorted,
// read-only form Corpus exposes. Required is left nil when All is true,
// matching LabelsNeeded's own contract that Required is meaningful only
// when All is false -- a caller must never read Required as a bound on a
// metric this map already says needs everything.
func needsToCorpus(needs map[string]*needAccumulator) map[string]MetricNeed {
	out := make(map[string]MetricNeed, len(needs))
	for m, acc := range needs {
		n := MetricNeed{
			All:        acc.all,
			AllOpsSafe: acc.opsSafe,
			Blockers:   acc.blockers,
		}
		if !acc.all {
			for r := range acc.required {
				n.Required = append(n.Required, r)
			}
			sort.Strings(n.Required)
		}
		for op := range acc.ops {
			n.Ops = append(n.Ops, op)
		}
		sort.Strings(n.Ops)
		for fn := range acc.fns {
			n.Fns = append(n.Fns, fn)
		}
		sort.Strings(n.Fns)
		for w := range acc.windows {
			n.Windows = append(n.Windows, w)
		}
		sort.Strings(n.Windows)
		out[m] = n
	}
	return out
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
	describe func(q string, err error) string, unparsed *[]string, needs map[string]*needAccumulator) {
	for _, q := range qs {
		c.Queries++
		sub := Substitute(q)
		refs, err := Extract(sub)
		if err != nil {
			*unparsed = append(*unparsed, describe(q, err))
			for _, tok := range metricToken.FindAllString(q, -1) {
				if known[tok] {
					c.markUsed(tok, src)
					// jetsam cannot read this query at all, so it might need
					// any label of any metric it merely mentions.
					foldUnreadable(needs, tok, q)
				}
			}
			continue
		}
		metrics := refs.Resolve(allMetrics)
		for _, m := range metrics {
			c.markUsed(m, src)
		}
		// The SUBSTITUTED text is what Extract actually analysed -- folding
		// the raw q here would ask LabelsNeeded to re-derive labels from
		// $variables that were never resolved, and get a different, wrong
		// answer than the one Extract's refs are based on.
		foldNeeds(needs, sub, metrics)
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

	known := make(map[string]bool, len(allMetrics))
	for _, m := range allMetrics {
		known[m] = true
	}

	// needs accumulates the corpus-wide union as every source is read below,
	// and is converted to c.Needs once at the end -- see needsToCorpus.
	needs := map[string]*needAccumulator{}

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
			// An unparseable rule already forbids every drop by way of
			// Blocked -- but Needs must not depend on a reader following
			// that distant gate before trusting it, so the same metric-token
			// fallback readQueries uses for a contained failure applies here
			// too, purely for Needs' own consistency.
			for _, tok := range metricToken.FindAllString(r.Query, -1) {
				if known[tok] {
					foldUnreadable(needs, tok, r.Query)
				}
			}
			continue
		}
		metrics := refs.Resolve(allMetrics)
		for _, m := range metrics {
			c.markUsed(m, FromRule)
		}
		foldNeeds(needs, r.Query, metrics)
	}

	for _, d := range src.Dashboards {
		title, uid := d.Title, d.UID
		c.readQueries(d.Queries, FromDashboard, known, allMetrics,
			func(q string, err error) string {
				return fmt.Sprintf("dashboard %s (%s): %v", title, uid, err)
			}, &c.DashboardPanelsUnparsed, needs)

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
			}, &c.DashboardVariablesUnparsed, needs)
	}

	if src.QueryLog != nil {
		c.LogRead = true
		c.LogSpan = src.QueryLog.Span
		c.LogEnd = src.QueryLog.End
		c.LogQualifies = src.LogQualifies
		c.readQueries(src.QueryLog.Queries, FromLog, known, allMetrics,
			func(q string, err error) string {
				return fmt.Sprintf("query log: %v", err)
			}, &c.QueryLogUnparsed, needs)
	}

	c.Needs = needsToCorpus(needs)
	return c
}
