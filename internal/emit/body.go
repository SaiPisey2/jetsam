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
// Markdown renderer will parse. Every one of these strings is untrusted:
// anyone who can expose a metric to a scraped target, or name a rule that
// ends up in Blocked, controls what lands here, and this body's whole
// purpose is to be an approval surface a human reads before permanently
// deleting monitoring data. A metric name that renders as a live link or a
// tracking beacon in that surface is a phishing vector, not a cosmetic bug.
//
// safe.Text handles the first problem: a control character -- an ANSI
// escape, a bare CR/LF, a tab -- is rendered as its visible escape sequence
// instead of executing in a terminal or forging a line break inside what is
// meant to be one Markdown table row.
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
//
// Order matters throughout: backslashes are escaped first, so a backslash
// already escaping one of these characters can never be produced by this
// function's own escaping and then re-interpreted as escaping something
// else.
func mdText(s string) string {
	s = safe.Text(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	s = strings.ReplaceAll(s, "(", `\(`)
	s = strings.ReplaceAll(s, ")", `\)`)
	s = strings.ReplaceAll(s, "!", `\!`)
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
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
func Body(drops []Drop, res verdict.Result, queries, totalSeries int, perSeriesMonth float64) (string, string) {
	total := 0
	for _, d := range drops {
		total += d.Series
	}
	title := fmt.Sprintf("jetsam: stop storing %d unread series", total)

	gradeOf := make(map[string]verdict.Grade, len(res.Verdicts))
	for _, v := range res.Verdicts {
		gradeOf[v.Metric] = v.Grade
	}
	unreferenced := map[string]bool{}
	for _, d := range drops {
		if gradeOf[d.Metric] == verdict.GradeUnreferenced {
			unreferenced[d.Metric] = true
		}
	}

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

	if len(unreferenced) > 0 {
		fmt.Fprintf(&b, "> [!WARNING]\n> **%d metric(s) below rest on \"unreferenced\" grade, not \"unqueried\".** "+
			"They are not referenced by any rule, but jetsam has no query log configured and therefore cannot see "+
			"ad-hoc or Grafana Explore queries against them. `-include-unreferenced` was passed, and the operator "+
			"who ran it has chosen to accept that risk.\n\n", len(unreferenced))
	}

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

	b.WriteString("**How to check this**\n\nFor any metric above, search your rules for its name. " +
		"jetsam proposes a drop only when that search returns nothing and a query log confirms nothing read it.\n")
	return title, b.String()
}
