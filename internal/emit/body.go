package emit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/safe"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// mdText renders a remote-sourced string (a metric name, a job name, or a
// blocked-query description) for embedding in a pull request body that a
// Markdown renderer will parse.
//
// safe.Text handles the first problem: a control character -- an ANSI
// escape, a bare CR/LF, a tab -- is rendered as its visible escape sequence
// instead of executing in a terminal or, here, forging a line break inside
// what is meant to be one Markdown table row.
//
// Markdown adds a second problem safe.Text does not touch. This body never
// wraps a remote-sourced string in backticks -- unlike a naive `%s` inline
// code span, plain text cannot be broken out of, because there is no
// delimiter of ours for an embedded backtick to pair with. A literal
// backtick in the input is still backslash-escaped anyway, defensively: two
// backticks in the same table row (this metric's and some other field's)
// could otherwise pair up across cells and swallow whatever sits between
// them, "|" included, once a Markdown table parser resolves code spans
// before splitting a row on "|". A literal pipe is escaped for the more
// direct reason that it is the table's own column separator.
//
// Order matters: backslashes are escaped first, so a backslash already
// escaping a "|" or a backtick can never be produced by this function's own
// escaping and then re-interpreted as escaping something else.
func mdText(s string) string {
	s = safe.Text(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "`", "\\`")
	return s
}

// Body renders the pull request's title and body. perSeriesMonth of 0 means
// no rate was configured, and no currency figure is printed: there is no
// honest universal price for a series -- Grafana Cloud bills per active
// series, self-hosted Mimir costs disk and RAM, VictoriaMetrics differs
// again -- and inventing one would be the least checkable number in the
// whole report.
func Body(drops []Drop, res verdict.Result, queries, totalSeries int, perSeriesMonth float64) (string, string) {
	total := 0
	for _, d := range drops {
		total += d.Series
	}
	title := fmt.Sprintf("jetsam: stop storing %d unread series", total)

	var b strings.Builder
	fmt.Fprintf(&b, "## jetsam: %d series nothing reads\n\n", total)
	fmt.Fprintf(&b, "Checked against %d queries read from this Prometheus's own rules. "+
		"Every metric below is named by none of them.\n\n", queries)

	// The irreversibility warning: near the top, ahead of any numbers, ahead
	// of the table. Reverting this PR restores collection; it does not
	// restore the history that went unwritten while the drop was live, and
	// that is the one mistake here a reviewer cannot undo.
	b.WriteString("> [!WARNING]\n")
	b.WriteString("> **Reverting this PR restores collection, not history.** Series not written " +
		"while these rules are live cannot be recovered afterwards. Everything else here is reversible; this is not.\n\n")

	if total > 0 && totalSeries > 0 {
		fmt.Fprintf(&b, "Drops %d series -- %.1f%% of what this Prometheus stores.\n", total, 100*float64(total)/float64(totalSeries))
		if perSeriesMonth > 0 {
			// State the arithmetic, not just the answer: the configured
			// rate and the multiplication, so a reader can check it rather
			// than trust it.
			fmt.Fprintf(&b, "At `pricing.per_series_month = %g` (%d series x %g/series/month) that is %.2f per month.\n",
				perSeriesMonth, total, perSeriesMonth, float64(total)*perSeriesMonth)
		}
		b.WriteString("\n")
	}

	byJob := map[string][]Drop{}
	for _, d := range drops {
		byJob[d.Job] = append(byJob[d.Job], d)
	}
	jobs := make([]string, 0, len(byJob))
	for j := range byJob {
		jobs = append(jobs, j)
	}
	sort.Strings(jobs)
	for _, j := range jobs {
		fmt.Fprintf(&b, "### job \"%s\"\n\n| metric | series |\n| --- | --- |\n", mdText(j))
		ds := byJob[j]
		sort.Slice(ds, func(a, c int) bool { return ds[a].Series > ds[c].Series })
		for _, d := range ds {
			fmt.Fprintf(&b, "| %s | %d |\n", mdText(d.Metric), d.Series)
		}
		b.WriteString("\n")
	}

	// Count what was declined, not just what was dropped: every blocked
	// input, because the number that matters is how many things jetsam
	// refused to touch.
	if len(res.Blocked) > 0 {
		fmt.Fprintf(&b, "### Declined\n\njetsam refused to propose anything for %d input(s) it could not read:\n\n", len(res.Blocked))
		for _, x := range res.Blocked {
			fmt.Fprintf(&b, "- %s\n", mdText(x))
		}
		b.WriteString("\n")
	}

	b.WriteString("**How to check this**\n\nFor any metric above, search your rules for its name. " +
		"jetsam proposes a drop only when that search returns nothing and a query log confirms nothing read it.\n")
	return title, b.String()
}
