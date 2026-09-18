// Package report renders jetsam's findings for a terminal.
package report

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/safe"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// Scan writes the scan report: a header, then every metric graded, largest
// first.
func Scan(w io.Writer, inv inventory.Inventory, c corpus.Corpus, res verdict.Result) {
	if inv.Truncated {
		fmt.Fprintf(w, "WARNING: inventory truncated at metric_limit=%d of %d metric names; "+
			"metrics outside the top %d by series count were not graded. Raise prometheus.metric_limit "+
			"in your config to see them.\n\n", inv.MetricLimit, inv.NameCount, inv.MetricLimit)
	}
	fmt.Fprintf(w, "Metrics    %d\n", len(inv.Metrics))
	fmt.Fprintf(w, "Series     %d\n", inv.TotalSeries)
	fmt.Fprintf(w, "Queries    %d read from %s\n", c.Queries, c.SourceList())
	if res.DroppableSeries > 0 {
		fmt.Fprintf(w, "Droppable  %d series (%.1f%% of stored)\n",
			res.DroppableSeries, 100*inv.Share(res.DroppableSeries))
	} else {
		// "Droppable none" has more than one possible cause, and they are
		// not interchangeable: a blocked corpus withholds every drop
		// regardless of evidence, so blaming a missing query log there is
		// simply wrong -- it tells an operator to configure query_log.path
		// when doing so would change nothing until the blocked rule is
		// fixed. Only print the query-log explanation when it is actually
		// why nothing is droppable.
		switch {
		// Checked first: dashboard evidence configured but unfetchable
		// withholds every drop regardless of whether a rule or the query
		// log would otherwise have accounted for everything. "No dashboard
		// reads it" and "nobody looked" produce identical numbers, so this
		// must never fall through to a message that claims the second when
		// the truth is the first.
		case res.DashboardsMissing:
			fmt.Fprintln(w, "Droppable  none -- dashboard evidence was configured but could not be fetched, so every drop is withheld")
		case res.QueryLogMissing:
			fmt.Fprintln(w, "Droppable  none -- the query log was configured but could not be read, so every drop is withheld")
		case len(res.Blocked) > 0:
			fmt.Fprintf(w, "Droppable  none -- %d rule(s) could not be read; a blocked rule may reference "+
				"anything, so every drop is withheld until it is fixed\n", len(res.Blocked))
		case anyUnreferenced(res.Verdicts):
			// One owner for this clause -- see corpus.Corpus.LogShortfall.
			// This branch used to say "no query log configured" flatly,
			// two lines above a "Query log covers 3h -- not long enough"
			// line printed from the same corpus, which contradicted itself
			// on every install whose log was configured and short.
			fmt.Fprintf(w, "Droppable  none -- %s, so ad-hoc reads are invisible\n", c.LogShortfall())
		default:
			fmt.Fprintln(w, "Droppable  none -- every metric is referenced by a rule or dashboard, or was read within the query-log window")
		}
	}
	// Blocked entries quote a rule's group, name and parse error, all
	// sourced from the remote Prometheus: sanitize before printing.
	for _, b := range res.Blocked {
		fmt.Fprintf(w, "Blocked    %s\n", safe.Text(b))
	}
	if c.DashboardsConfigured {
		if c.DashboardsReachable {
			fmt.Fprintf(w, "Dashboards %d read from Grafana\n", c.Dashboards)
		} else {
			fmt.Fprintln(w, "Dashboards configured but could not be fetched -- every drop is withheld")
		}
	}
	if c.LogUnreadable {
		fmt.Fprintln(w, "Query log  configured but could not be read -- no negative evidence, so every drop is withheld")
	}
	if c.LogRead {
		// The end time is printed beside the span because the span alone
		// cannot distinguish a log that covers the last 30 days from one
		// that covers 30 days of last year. jetsam does not refuse on a
		// stale log; it must at least show one.
		if c.LogQualifies {
			fmt.Fprintf(w, "Query log  covers %s, ending %s\n", c.LogSpanText(), c.LogEndText())
		} else {
			fmt.Fprintf(w, "Query log  covers %s, ending %s -- not long enough to license a drop\n",
				c.LogSpanText(), c.LogEndText())
		}
	}
	for _, p := range c.DashboardPanelsUnparsed {
		fmt.Fprintf(w, "Unparsed   %s\n", safe.Text(p))
	}
	for _, q := range c.QueryLogUnparsed {
		// The query log is the only source whose SILENCE licenses a
		// deletion, so a query it could not read is the one parse failure
		// that must never be swallowed.
		fmt.Fprintf(w, "Unparsed   %s\n", safe.Text(q))
	}
	if n := len(c.DashboardVariablesUnparsed); n > 0 {
		// A COUNT, not the list. A dashboard's template variables are
		// normally Grafana functions like label_values(...), which is not
		// PromQL and never parses -- three of the four on a healthy
		// vendored dashboard land here -- so naming them would print three
		// broken-looking lines on every scan of a working install.
		fmt.Fprintf(w, "Variables  %d dashboard template variable(s) are not PromQL (normal: label_values() and "+
			"friends are Grafana functions); every metric name in them is treated as read\n", n)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIES\tSHARE\tGRADE\tDROP\tMETRIC\tWHY")
	for _, v := range res.Verdicts {
		drop := "no"
		if v.Droppable {
			drop = "yes"
		}
		// The metric name is remote-sourced; the reason may embed
		// remote-sourced detail too. Sanitize both -- an ANSI escape or a
		// newline here can clear/forge a terminal line or break a
		// Markdown table in a generated pull request.
		fmt.Fprintf(tw, "%d\t%.1f%%\t%s\t%s\t%s\t%s\n",
			v.Series, 100*inv.Share(v.Series), v.Grade, drop, safe.Text(v.Metric), safe.Text(v.Reason))
	}
	tw.Flush()
}

// anyUnreferenced reports whether any verdict rests on GradeUnreferenced --
// the grade that only exists because no query log LICENSES a drop, which
// covers three different states: none configured, one configured and too
// short, and one configured and unreadable. Its presence is what makes a
// query-log explanation the correct one for "Droppable none";
// corpus.Corpus.LogShortfall says which of the three it is. Its absence
// means every metric was either used or fully evaluated against an actual
// qualifying log, and nothing was missed for lack of one.
func anyUnreferenced(vs []verdict.Verdict) bool {
	for _, v := range vs {
		if v.Grade == verdict.GradeUnreferenced {
			return true
		}
	}
	return false
}
