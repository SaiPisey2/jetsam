package emit

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/safe"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// addSeriesSaturating adds b to a, clamping to math.MaxInt instead of
// wrapping into a negative number when the true sum overflows. Series
// counts are validated non-negative where they enter jetsam (see
// promapi.Client.TSDBStatus), so the only way this overflows is a hostile
// or compromised Prometheus reporting enough individually-plausible counts
// that their sum is not representable. A clamped total is still wrong --
// it understates the true sum -- but it is visibly, absurdly wrong (an
// implausible number of series) rather than silently wrong (a negative
// count of series in a PR asking a human to approve deleting them).
func addSeriesSaturating(a, b int) int {
	sum := a + b
	if b > 0 && sum < a {
		return math.MaxInt
	}
	return sum
}

// mdText renders a remote-sourced string (a metric name, a job name, or a
// blocked-query description) for embedding in a pull request body that a
// Markdown renderer will parse. Every one of these strings is untrusted:
// anyone who can expose a metric to a scraped target, or name a rule that
// ends up in Blocked, controls what lands here, and this body's whole
// purpose is to be an approval surface a human reads before permanently
// deleting monitoring data. A metric name that renders as a live link or a
// tracking beacon in that surface is a phishing vector, not a cosmetic bug.
//
// safe.Text handles the first problem: any rune that is not printable is
// rendered as its visible escape sequence instead of being emitted. That
// covers control characters -- an ANSI escape, a bare CR/LF, a tab, which
// execute in a terminal or forge a line break inside what is meant to be
// one Markdown table row -- and, just as importantly here, the Unicode
// FORMAT characters that are not control characters at all: U+202E and the
// directional isolates reorder the glyphs after them, so a name can be made
// to DISPLAY as a different name than the one being dropped, and U+200B,
// U+00AD and U+FEFF are invisible outright, so a reviewer cannot see that
// the name in the table is not the name they know. On an approval surface
// for an irreversible deletion, a name that does not read as itself is the
// whole attack.
//
// Markdown and GitHub's raw-HTML pass-through add every other problem
// safe.Text does not touch, so mdText neutralises each construct that turns
// plain text into something that renders as structure rather than content:
//
//   - "|" is escaped because it is the table's own column separator.
//   - "`" is escaped defensively. This body never wraps a remote-sourced
//     string in backticks -- unlike a naive `%s` inline code span, plain
//     text cannot be broken out of, because there is no delimiter of ours
//     for an embedded backtick to pair with. But two backticks in the same
//     table row (this field's and some other field's) could still pair up
//     across cells and swallow whatever sits between them, "|" included,
//     once a Markdown table parser resolves code spans before splitting a
//     row on "|" -- so a literal backtick is escaped anyway.
//   - "[", "]", "(", ")" and "!" are escaped so link and image syntax --
//     "[text](url)" or "![alt](url)" -- can never form. An unescaped image
//     renders as a live, unprompted GET request (a tracking beacon); an
//     unescaped link renders as something a reviewer can click without
//     ever seeing the URL as text.
//   - "<" and ">" are rendered as the HTML entities "&lt;"/"&gt;" rather
//     than backslash-escaped. GitHub's Markdown renders raw HTML that
//     survives inline parsing, so a script tag, a forged `</td><td>`, or an
//     HTML comment hiding real content is a structural break, not just a
//     display glitch. A backslash escape ("\<") depends on every renderer
//     in this body's path doing CommonMark-correct backslash-escape
//     processing before HTML-tag recognition; an entity reference does not
//     depend on that at all; it is inert text at the point angle brackets
//     would otherwise start being read as a tag; that holds regardless of
//     what parses this Markdown next.
//   - "." and "@" are rendered as the HTML entities "&#46;"/"&#64;". None
//     of the delimiter escaping above stops GitHub Flavored Markdown's
//     *extended autolink*, which needs no delimiters at all: bare
//     "www.evil.example", "http://evil.example" and "me@evil.example"
//     become live links purely from their own characters. A remote-sourced
//     job label or (on a Prometheus using the UTF-8 metric-naming scheme)
//     metric name is exactly the kind of free-form string that can spell
//     one of these out, turning this approval surface into a phishing
//     link or a mailto: a reviewer can click without ever deciding to.
//     Entity references, like the "<"/"&gt;" ones above, render as the
//     original character but leave no literal "." or "@" for the autolink
//     scanner to anchor on.
//
// Order matters throughout: backslashes are escaped first, so a backslash
// already escaping one of these characters can never be produced by this
// function's own escaping and then re-interpreted as escaping something
// else. "&" is escaped immediately after, and before every entity this
// function introduces ("&lt;", "&gt;", "&#46;", "&#64;"): escaping it
// first, rather than not touching it at all, closes two problems with one
// rule. First, it stops this function's own output from being
// double-escaped -- if "&" escaping ran after the "<"/"." replacements
// below, "&lt;" would become "&amp;lt;", which renders as the literal
// text "&lt;" instead of "<". Second, it stops a remote string from
// smuggling a literal "." or "@" past the checks below by spelling out
// its own entity: an input already containing the literal text "&#46;"
// would, left untouched, still decode to "." in a Markdown/HTML renderer
// even though this function never wrote a "." itself. Escaping any
// pre-existing "&" to "&amp;" first makes that sequence render as the
// inert text "&#46;", not a decoded period.
func mdText(s string) string {
	s = safe.Text(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	s = strings.ReplaceAll(s, "(", `\(`)
	s = strings.ReplaceAll(s, ")", `\)`)
	s = strings.ReplaceAll(s, "!", `\!`)
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, ".", "&#46;")
	s = strings.ReplaceAll(s, "@", "&#64;")
	return s
}

// Body renders the pull request's title and body. perSeriesMonth of 0 means
// no rate was configured, and no currency figure is printed: there is no
// honest universal price for a series -- Grafana Cloud bills per active
// series, self-hosted Mimir costs disk and RAM, VictoriaMetrics differs
// again -- and inventing one would be the least checkable number in the
// whole report.
//
// res.Verdicts is also where each drop's evidence grade comes from: the
// spec requires the body to state which grade every drop rests on, not
// just that it is droppable, because "unreferenced" (no rule names it, but
// jetsam cannot see ad-hoc or Explore queries) and "unqueried" (a query log
// covered the window too) carry very different confidence, and a drop
// proposed on the weaker grade needs a warning of its own, alongside -- not
// instead of -- the irreversibility one.
//
// inv, not a bare total, so this function can warn when the inventory
// itself was truncated by metric_limit: inv.TotalSeries is the Prometheus's
// true series count (see inventory.Inventory.TotalSeries), and the
// percentage below is stated against that, never against a sum of only the
// metrics metric_limit happened to return.
func Body(drops []Drop, res verdict.Result, c corpus.Corpus, inv inventory.Inventory, perSeriesMonth float64) (string, string) {
	// The headline total counts each METRIC once, not each Drop once. A
	// metric produced by several jobs (a shared exporter scraped under
	// more than one job label, most commonly) gets one emit.Drop per job
	// it is dropped from, each carrying that metric's FULL series count
	// from the inventory -- cmd/jetsam does this deliberately, because a
	// relabel rule genuinely is added to every one of those jobs. Summing
	// Series across every Drop would then count that metric's series once
	// per job it happens to be produced by, which is how a real run
	// against the public demo reported dropping more series than the
	// instance held: a percentage over 100%. That number is the one this
	// whole tool asks a human to trust before approving an irreversible
	// deletion, so it must never be able to say something that cannot be
	// true. multiJobMetrics counts how many metrics this happened for, so
	// the per-job tables below can say plainly that they list a shared
	// metric more than once while the total above does not.
	dropCount := map[string]int{}
	for _, d := range drops {
		dropCount[d.Metric]++
	}
	seenMetric := map[string]bool{}
	total := 0
	multiJobMetrics := 0
	for _, d := range drops {
		if seenMetric[d.Metric] {
			continue
		}
		seenMetric[d.Metric] = true
		total = addSeriesSaturating(total, d.Series)
		if dropCount[d.Metric] > 1 {
			multiJobMetrics++
		}
	}
	title := fmt.Sprintf("jetsam: stop storing %d unread series", total)

	gradeOf := make(map[string]verdict.Grade, len(res.Verdicts))
	for _, v := range res.Verdicts {
		gradeOf[v.Metric] = v.Grade
	}
	unreferenced := map[string]bool{}
	unqueried := map[string]bool{}
	for _, d := range drops {
		switch gradeOf[d.Metric] {
		case verdict.GradeUnreferenced:
			unreferenced[d.Metric] = true
		case verdict.GradeUnqueried:
			unqueried[d.Metric] = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## jetsam: %d series nothing reads\n\n", total)
	fmt.Fprintf(&b, "Checked against %d queries read from this Prometheus's own %s. "+
		"Every metric below is named by none of them.\n\n", c.Queries, c.SourceList())

	// The irreversibility warning: near the top, ahead of any numbers, ahead
	// of the table. Reverting this PR restores collection; it does not
	// restore the history that went unwritten while the drop was live, and
	// that is the one mistake here a reviewer cannot undo.
	b.WriteString("> [!WARNING]\n")
	b.WriteString("> **Reverting this PR restores collection, not history.** Series not written " +
		"while these rules are live cannot be recovered afterwards. Everything else here is reversible; this is not.\n\n")

	// How much evidence there was, the same three facts report.Scan states
	// on the terminal: dashboards read (or unreachable), the query log's
	// span against its minimum, and any dashboard panel jetsam could not
	// parse. A reviewer approving an irreversible deletion should not have
	// to take jetsam's grading on faith without knowing what it actually
	// looked at.
	if c.DashboardsConfigured {
		if c.DashboardsReachable {
			fmt.Fprintf(&b, "Dashboards: %d read from Grafana.\n\n", c.Dashboards)
		} else {
			fmt.Fprint(&b, "> [!WARNING]\n> **Grafana is configured but its dashboards could not be fetched.** "+
				"\"No dashboard reads it\" and \"nobody looked\" are opposite claims with identical numbers, so "+
				"every drop below is withheld until Grafana is reachable again.\n\n")
		}
	}
	if c.LogUnreadable {
		fmt.Fprint(&b, "> [!WARNING]\n> **A query log is configured and could not be read.** It is the only source whose "+
			"silence is evidence, so jetsam has no negative evidence at all on this run and every drop below is "+
			"withheld until the log is readable again.\n\n")
	}
	if c.LogRead {
		// The end time goes beside the span deliberately. A span alone
		// cannot tell a log covering the last 30 days from one covering 30
		// days of last year, and a reviewer approving a deletion on the
		// strength of "nothing queried it" should be able to see when the
		// log stopped looking.
		if c.LogQualifies {
			fmt.Fprintf(&b, "Query log: covers %s, ending %s.\n\n", c.LogSpanText(), c.LogEndText())
		} else {
			fmt.Fprintf(&b, "Query log: covers %s, ending %s -- not long enough to license a drop.\n\n",
				c.LogSpanText(), c.LogEndText())
		}
	}
	if len(c.DashboardPanelsUnparsed) > 0 {
		fmt.Fprintf(&b, "%d dashboard panel(s) could not be parsed and were conservatively marked as reading "+
			"every metric-shaped token they mention:\n\n", len(c.DashboardPanelsUnparsed))
		for _, p := range c.DashboardPanelsUnparsed {
			fmt.Fprintf(&b, "- %s\n", mdText(p))
		}
		b.WriteString("\n")
	}
	if len(c.QueryLogUnparsed) > 0 {
		// Named, not counted, unlike the template variables below: the
		// query log is the one source whose silence licenses a deletion,
		// so a query it could not read is a gap in the evidence the drops
		// below rest on rather than an ordinary shape of the input.
		fmt.Fprintf(&b, "%d logged quer(ies) could not be parsed and were conservatively marked as reading "+
			"every metric-shaped token they mention:\n\n", len(c.QueryLogUnparsed))
		for _, q := range c.QueryLogUnparsed {
			fmt.Fprintf(&b, "- %s\n", mdText(q))
		}
		b.WriteString("\n")
	}
	if n := len(c.DashboardVariablesUnparsed); n > 0 {
		// A count with its explanation, never the list: a dashboard's
		// template variables are normally Grafana functions such as
		// label_values(...), which is not PromQL and never parses, so
		// naming them would fill this section with broken-looking entries
		// on every healthy install.
		fmt.Fprintf(&b, "%d dashboard template variable(s) are not PromQL -- normal, since `label_values()` and "+
			"friends are Grafana functions rather than queries Prometheus parses. Every metric name in them is "+
			"treated as read.\n\n", n)
	}

	if len(unreferenced) > 0 {
		fmt.Fprintf(&b, "> [!WARNING]\n> **%d metric(s) below rest on \"unreferenced\" grade, not \"unqueried\".** "+
			"They are not referenced by any %s, but %s, so jetsam cannot see ad-hoc or Grafana "+
			"Explore queries against them. `-include-unreferenced` was passed, and the operator who ran it has "+
			"chosen to accept that risk.\n\n", len(unreferenced), c.Readers(), c.LogShortfall())
	}

	if len(unqueried) > 0 {
		// "unqueried" is the fully-evidenced grade, but the evidence is
		// still bounded: it only covers the window the log actually spans.
		// A read that happened before that window, or recurs less often
		// than it, would not appear in it -- so name the span here rather
		// than let "unqueried" read as an unconditional guarantee.
		fmt.Fprintf(&b, "%d metric(s) below rest on \"unqueried\" grade: no PromQL query ran against them in the "+
			"**%s** the query log covers, ending %s. Prometheus' query log records PromQL engine queries only, "+
			"so a read through the label-values or series endpoints -- which is how Grafana resolves a "+
			"dashboard's variables -- never appears in it at all, and a query that ran before that window, or "+
			"recurs less often than it, would not either.\n\n",
			len(unqueried), c.LogSpanText(), c.LogEndText())
	}

	if inv.Truncated {
		fmt.Fprintf(&b, "> [!WARNING]\n> **This inventory was truncated at metric_limit=%d of %d metric names.** "+
			"Metrics outside the top %d by series count were not graded and could not be proposed for dropping. "+
			"The percentage below understates what this Prometheus actually stores only in the sense that it "+
			"cannot account for those ungraded metrics at all; raise `prometheus.metric_limit` to see them.\n\n",
			inv.MetricLimit, inv.NameCount, inv.MetricLimit)
	}

	if total > 0 && inv.TotalSeries > 0 {
		fmt.Fprintf(&b, "Drops %d series -- %.1f%% of what this Prometheus stores.\n", total, 100*float64(total)/float64(inv.TotalSeries))
		if perSeriesMonth > 0 {
			// State the arithmetic, not just the answer: the configured
			// rate and the multiplication, so a reader can check it rather
			// than trust it.
			fmt.Fprintf(&b, "At `pricing.per_series_month = %g` (%d series x %g/series/month) that is %.2f per month.\n",
				perSeriesMonth, total, perSeriesMonth, float64(total)*perSeriesMonth)
		}
		b.WriteString("\n")
	}

	if multiJobMetrics > 0 {
		// Chose to keep the per-row series count in the tables below,
		// rather than omit it for a shared metric: it is the same real
		// number either way (this metric's series count, from the
		// inventory), and a relabel rule genuinely is being added to every
		// job listed, which is worth stating plainly. What must not happen
		// is a reader adding the rows up and reasonably expecting to reach
		// the headline total -- so say outright that they won't.
		fmt.Fprintf(&b, "%d metric(s) below are produced by more than one job and are listed once under "+
			"each -- the totals above count each metric once, not once per job.\n\n", multiJobMetrics)
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
		fmt.Fprintf(&b, "### job \"%s\"\n\n| metric | series | grade |\n| --- | --- | --- |\n", mdText(j))
		ds := byJob[j]
		sort.Slice(ds, func(a, c int) bool { return ds[a].Series > ds[c].Series })
		for _, d := range ds {
			// The grade string itself ("used"/"unreferenced"/"unqueried") is
			// one of verdict's own constants, not remote-sourced, so it does
			// not need mdText -- unlike the metric name next to it.
			fmt.Fprintf(&b, "| %s | %d | %s |\n", mdText(d.Metric), d.Series, gradeOf[d.Metric])
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

	b.WriteString("**How to check this**\n\nFor any metric above, search your rules and your Grafana dashboards " +
		"for its name. A metric graded `unqueried` reached this list because that search came up empty AND a " +
		"query log covering the window above recorded no PromQL query against it. A metric graded " +
		"`unreferenced` reached it because the search came up empty and an operator passed " +
		"`-include-unreferenced` to accept it without query-log evidence -- the grade column above says which.\n")
	return title, b.String()
}
