// Command jetsam finds Prometheus metrics nothing reads and proposes
// dropping them.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/config"
	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/emit"
	"github.com/SaiPisey2/jetsam/internal/forge"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/report"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

const usage = `jetsam finds Prometheus metrics nothing reads.

usage:
  jetsam init     [-config jetsam.yaml]
  jetsam scan     [-config jetsam.yaml]
  jetsam propose  [-config jetsam.yaml]
  jetsam propose  -apply -owner OWNER -repo REPO [-base main] [-config jetsam.yaml]

  -apply requires $GITHUB_TOKEN in the environment. It is never accepted as
  a flag: a flag value lands in argv, readable by every other user on the
  machine via ps, and in shell history.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "scan":
		runScan(os.Args[2:])
	case "propose":
		runPropose(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "jetsam.yaml", "config file")
	fs.Parse(args)
	if err := config.WriteDefault(*path); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s\n", *path)
}

func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	path := fs.String("config", "jetsam.yaml", "config file")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Prometheus.Timeout)
	defer cancel()

	cl := promapi.New(cfg.Prometheus.URL, cfg.Prometheus.Timeout)
	counts, err := cl.TSDBStatus(ctx, cfg.Prometheus.MetricLimit)
	if err != nil {
		fail(err)
	}
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		fail(err)
	}

	inv := inventory.Build(counts)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	cor := corpus.Build(rules, names)
	// v0.1 never reads a query log, so the evidence for "nobody queried
	// this" does not exist yet; cfg.HaveQueryLog() only reports that a
	// path is configured in YAML, not that anything was read from it.
	// Wiring the configured path to Compute here would let one line of
	// YAML mark metrics droppable on rule evidence alone. This parameter
	// becomes real in v0.3 when log ingestion lands.
	report.Scan(os.Stdout, inv, cor, verdict.Compute(inv, cor, false))
}

// runPropose is the production entry point for `jetsam propose`. It wires
// proposeCmd to the real world: real stdio, the real environment (so
// $GITHUB_TOKEN is read from there and nowhere else), and a real
// forge.GitHubProvider when -apply is given.
func runPropose(args []string) {
	os.Exit(proposeCmd(args, os.Stdout, os.Stderr, os.Getenv, func(token string) forge.Provider {
		return forge.NewGitHubProvider(token)
	}))
}

// proposeCmd implements `jetsam propose`, with every side channel that
// matters for testing passed in explicitly: where output goes, where the
// environment is read from, and how a forge.Provider is built. That last
// seam is what lets -apply be exercised end to end against
// forge.NewFakeProvider() in a test, without the test ever constructing a
// real GitHubProvider or touching a real repository.
//
// It repeats the scan pipeline, then either prints the pull request it
// would open (a dry run: the unified diff of the scrape config it would
// edit, and the PR body -- nothing written, nothing reaches a forge) or,
// with -apply, actually opens it via newProvider(token).
func proposeCmd(args []string, stdout, stderr io.Writer, getenv func(string) string, newProvider func(token string) forge.Provider) int {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "jetsam.yaml", "config file")
	apply := fs.Bool("apply", false, "open the pull request on GitHub instead of printing a dry run")
	owner := fs.String("owner", "", "GitHub repository owner (required with -apply)")
	repo := fs.String("repo", "", "GitHub repository name (required with -apply)")
	base := fs.String("base", "main", "base branch to open the pull request against")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logErr := func(err error) { fmt.Fprintf(stderr, "error: %v\n", err) }

	// -apply is the only path that touches someone else's repository and
	// the only one that needs a credential. Both are checked here, before
	// the config is loaded, before Prometheus is queried, before any file
	// is read: a run that does half the work and then fails partway is
	// worse than one that refuses immediately.
	//
	// The token is read from the environment only, via getenv
	// ($GITHUB_TOKEN in production). It is never accepted as a flag: a
	// flag value lands in argv, readable by every other user on the
	// machine via `ps`, and in shell history.
	token := getenv("GITHUB_TOKEN")
	if *apply {
		var missing []string
		if *owner == "" {
			missing = append(missing, "-owner")
		}
		if *repo == "" {
			missing = append(missing, "-repo")
		}
		if token == "" {
			missing = append(missing, "$GITHUB_TOKEN")
		}
		if len(missing) > 0 {
			logErr(fmt.Errorf("-apply requires %s", strings.Join(missing, ", ")))
			return 1
		}
	}

	cfg, err := config.Load(*path)
	if err != nil {
		logErr(err)
		return 1
	}
	if cfg.PrometheusFile == "" {
		logErr(fmt.Errorf("propose requires prometheus_file to be set in %s", *path))
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Prometheus.Timeout)
	defer cancel()

	cl := promapi.New(cfg.Prometheus.URL, cfg.Prometheus.Timeout)
	counts, err := cl.TSDBStatus(ctx, cfg.Prometheus.MetricLimit)
	if err != nil {
		logErr(err)
		return 1
	}
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		logErr(err)
		return 1
	}

	inv := inventory.Build(counts)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	cor := corpus.Build(rules, names)
	// Same reasoning as runScan, and just as deliberate here: v0.1 has no
	// query log evidence, so passing cfg.HaveQueryLog() would let a line of
	// YAML mark metrics droppable on rule evidence alone. On a default
	// install this means propose finds nothing droppable and says so below,
	// rather than silently deleting data on rule evidence.
	res := verdict.Compute(inv, cor, false)

	var drops []emit.Drop
	var unresolved []string
	for _, v := range res.Verdicts {
		if !v.Droppable {
			continue
		}
		jobs, err := cl.QueryJobsFor(ctx, v.Metric)
		if err != nil {
			logErr(fmt.Errorf("resolve job for %s: %w", v.Metric, err))
			return 1
		}
		if len(jobs) == 0 {
			// Nothing is currently exposing this metric under any job
			// label, so there is no scrape config to add a drop rule to.
			// Skipped, not guessed at.
			unresolved = append(unresolved, v.Metric)
			continue
		}
		for _, j := range jobs {
			drops = append(drops, emit.Drop{Metric: v.Metric, Series: v.Series, Job: j})
		}
	}

	if len(drops) == 0 {
		fmt.Fprintln(stdout, "propose: nothing to propose.")
		switch {
		case len(res.Blocked) > 0:
			fmt.Fprintf(stdout, "%d rule(s) could not be read; a blocked rule may reference anything, "+
				"so every drop is withheld until it is fixed. See `jetsam scan` for which.\n", len(res.Blocked))
		case len(unresolved) > 0:
			fmt.Fprintf(stdout, "%d metric(s) graded droppable have no job currently exposing them, so there is no scrape config to edit; skipped.\n", len(unresolved))
		default:
			fmt.Fprintln(stdout, "no query log is configured, so jetsam cannot tell \"no rule mentions this\" apart from "+
				"\"nobody reads this\" -- set query_log.path to make any metric eligible for a drop.")
		}
		// Nothing droppable means nothing to commit or open, -apply
		// included: no branch is created and no empty PR is opened.
		fmt.Fprintln(stdout, "dry run: nothing opened. Re-run with -apply to open this PR.")
		return 0
	}

	promYAML, err := os.ReadFile(cfg.PrometheusFile)
	if err != nil {
		logErr(fmt.Errorf("read %s: %w", cfg.PrometheusFile, err))
		return 1
	}
	newYAML, err := emit.Render(string(promYAML), drops)
	if err != nil {
		logErr(err)
		return 1
	}

	if diff := unifiedDiff(cfg.PrometheusFile, string(promYAML), newYAML); diff != "" {
		fmt.Fprint(stdout, diff)
	} else {
		fmt.Fprintf(stdout, "(no change: %s already carries every drop rule below)\n", cfg.PrometheusFile)
	}
	fmt.Fprintln(stdout)

	title, body := emit.Body(drops, res, cor.Queries, inv.TotalSeries, cfg.Pricing.PerSeriesMonth)
	fmt.Fprintf(stdout, "%s\n\n%s\n\n", title, body)

	if !*apply {
		fmt.Fprintln(stdout, "dry run: nothing opened. Re-run with -apply to open this PR.")
		return 0
	}

	return applyAndReport(ctx, stdout, stderr, newProvider(token), *owner, *repo, *base, cfg.PrometheusFile, string(promYAML), newYAML, title, body)
}

// applyAndReport calls emit.Apply through p and reports the outcome: the
// opened (or found) PR's state and URL on stdout, or the error on stderr.
// It is everything -apply does once there is something to propose,
// factored out so it can be driven straight against
// forge.NewFakeProvider() in a test without a real scan first -- and
// notably, it never takes a token: by the time p exists, authentication is
// already p's own business (see the comment on emit.Apply).
func applyAndReport(ctx context.Context, stdout, stderr io.Writer, p forge.Provider, owner, repo, base, path, oldYAML, newYAML, title, body string) int {
	pr, err := emit.Apply(ctx, p, owner, repo, base, path, oldYAML, newYAML, title, body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %s\n", pr.State, pr.URL)
	return 0
}

// diffContext is how many unchanged lines surround a change in the
// unified diff propose prints, matching the context width forge/edit.go
// uses for the same kind of human-readable patch.
const diffContext = 3

// diffLine is one line of an old/new comparison, tagged with how it
// changed: ' ' unchanged, '-' removed, '+' added.
type diffLine struct {
	op   byte
	text string
}

// unifiedDiff renders old -> new in the style of `diff -u`, computed from a
// longest common subsequence of lines. It exists so a reviewer can check
// exactly what jetsam would write to path rather than trusting jetsam's own
// description of the change -- which only holds if the diff is one `patch`
// can actually apply, trailing newline included.
func unifiedDiff(path, oldText, newText string) string {
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	ops := diffLCS(oldLines, newLines)
	ops, oldMarkIdx, newMarkIdx := markMissingTrailingNewlines(ops, oldLines, newLines,
		hasTrailingNewline(oldText), hasTrailingNewline(newText))
	return formatHunks(path, ops, oldMarkIdx, newMarkIdx)
}

// splitLines splits on "\n" without discarding a final line that has no
// trailing newline, so a file missing one still diffs correctly.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// hasTrailingNewline reports whether s, taken as a whole file, ends with a
// newline. An empty file is not considered to be missing one -- there is no
// final line to annotate either way.
func hasTrailingNewline(s string) bool {
	return s == "" || strings.HasSuffix(s, "\n")
}

// noNewlineMarker is the standard unified-diff annotation for a line that,
// in the file it came from, is not actually terminated by a newline.
// Without it, two problems follow: a diff between two files that differ
// ONLY in whether the file ends with a newline looks like "no change" (a
// real byte-level difference reported as none), and a diff that legitimately
// changes content right up to such a line fails to apply via `patch` --
// which relies on this exact marker to know whether to glue what it writes
// next onto the same line or start a new one.
const noNewlineMarker = `\ No newline at end of file`

// markMissingTrailingNewlines adjusts ops so that either file's actual
// final line, if it lacks a trailing newline, can carry that fact -- even
// when line-text comparison alone would have folded it into an
// unremarkable, shared context line.
//
// diffLCS compares line TEXT only; whether a file's very last line ends in
// "\n" is tracked separately here, as one bit per file, which is how a real
// diff/patch toolchain models it too. Two problems follow from keeping that
// bit separate from line comparison:
//
//  1. Two files whose lines are textually identical but which disagree on
//     the final newline ("a\nb\n" vs "a\nb") diff to an empty op list --
//     "no change" -- even though the bytes differ.
//  2. When a file's true final line survives into the diff as a plain
//     context line (' ', shared with the other file at that position),
//     attaching a "no trailing newline" fact to it would silently claim it
//     for the OTHER file too, which may be wrong: the other file's line at
//     that position is not necessarily its own final line -- more content
//     can follow it there.
//
// Both are fixed the same way: whichever side needs to report "my last
// line has no trailing newline" gets its own copy of that line. A shared
// context op is split into a matching removal and addition so each side's
// copy can carry its own marker independently; an op that is already a
// distinct '-' or '+' entry needs no split.
func markMissingTrailingNewlines(ops []diffLine, oldLines, newLines []string, oldNL, newNL bool) (out []diffLine, oldMarkIdx, newMarkIdx int) {
	out = ops
	oldMarkIdx, newMarkIdx = -1, -1

	// lastConsuming returns the highest index of an op that consumes a line
	// from the side notOp does NOT represent -- i.e. the last op that is
	// not notOp. Old-consuming ops are '-' and ' '; new-consuming ops are
	// '+' and ' '. Because each side's lines are consumed strictly in the
	// order they appear, this index always identifies the op that consumed
	// that side's true final line, regardless of what the other side does
	// around it.
	lastConsuming := func(notOp byte) int {
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].op != notOp {
				return i
			}
		}
		return -1
	}

	if len(oldLines) > 0 && !oldNL {
		if i := lastConsuming('+'); i >= 0 {
			if out[i].op == ' ' {
				out = spliceReplace(out, i, diffLine{'-', out[i].text}, diffLine{'+', out[i].text})
			}
			oldMarkIdx = i
		}
	}
	if len(newLines) > 0 && !newNL {
		if i := lastConsuming('-'); i >= 0 {
			if out[i].op == ' ' {
				out = spliceReplace(out, i, diffLine{'-', out[i].text}, diffLine{'+', out[i].text})
				i++ // the '+' half is new's own copy of the line.
			}
			newMarkIdx = i
		}
	}
	return out, oldMarkIdx, newMarkIdx
}

// spliceReplace returns ops with the single entry at i replaced by a and b,
// in that order.
func spliceReplace(ops []diffLine, i int, a, b diffLine) []diffLine {
	out := make([]diffLine, 0, len(ops)+1)
	out = append(out, ops[:i]...)
	out = append(out, a, b)
	out = append(out, ops[i+1:]...)
	return out
}

// diffLCS returns old and new merged into one ordered list of kept, removed
// and added lines, via the longest common subsequence of the two. old and
// new are configuration files -- at most a few thousand lines -- so the
// O(len(old)*len(new)) table this builds is not a concern.
func diffLCS(old, new []string) []diffLine {
	n, m := len(old), len(new)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			switch {
			case old[i-1] == new[j-1]:
				dp[i][j] = dp[i-1][j-1] + 1
			case dp[i-1][j] >= dp[i][j-1]:
				dp[i][j] = dp[i-1][j]
			default:
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	ops := make([]diffLine, 0, n+m)
	i, j := n, m
	for i > 0 && j > 0 {
		switch {
		case old[i-1] == new[j-1]:
			ops = append(ops, diffLine{' ', old[i-1]})
			i--
			j--
		case dp[i-1][j] >= dp[i][j-1]:
			ops = append(ops, diffLine{'-', old[i-1]})
			i--
		default:
			ops = append(ops, diffLine{'+', new[j-1]})
			j--
		}
	}
	for i > 0 {
		ops = append(ops, diffLine{'-', old[i-1]})
		i--
	}
	for j > 0 {
		ops = append(ops, diffLine{'+', new[j-1]})
		j--
	}
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

// formatHunks groups ops into unified-diff hunks, each padded with up to
// diffContext lines of unchanged context on either side, merging two change
// regions whose padding would otherwise overlap into a single hunk.
// oldMarkIdx/newMarkIdx (-1 if not applicable) name the op, if any, that is
// old's or new's true final line while that side lacks a trailing newline;
// noNewlineMarker is printed immediately after whichever op line that is.
func formatHunks(path string, ops []diffLine, oldMarkIdx, newMarkIdx int) string {
	var regions [][2]int
	for i := 0; i < len(ops); {
		if ops[i].op == ' ' {
			i++
			continue
		}
		start := i
		end := i
		for end < len(ops) && ops[end].op != ' ' {
			end++
		}
		for {
			j := end
			for j < len(ops) && ops[j].op == ' ' {
				j++
			}
			if j == len(ops) || j-end > 2*diffContext {
				break
			}
			end = j
			for end < len(ops) && ops[end].op != ' ' {
				end++
			}
		}
		regions = append(regions, [2]int{start, end})
		i = end
	}
	if len(regions) == 0 {
		return ""
	}

	var b strings.Builder
	for _, r := range regions {
		lo, hi := r[0]-diffContext, r[1]+diffContext
		if lo < 0 {
			lo = 0
		}
		if hi > len(ops) {
			hi = len(ops)
		}

		oldStart, newStart := 1, 1
		for _, op := range ops[:lo] {
			switch op.op {
			case ' ':
				oldStart++
				newStart++
			case '-':
				oldStart++
			case '+':
				newStart++
			}
		}
		var oldCount, newCount int
		for _, op := range ops[lo:hi] {
			switch op.op {
			case ' ':
				oldCount++
				newCount++
			case '-':
				oldCount++
			case '+':
				newCount++
			}
		}

		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", path, path, oldStart, oldCount, newStart, newCount)
		for idx := lo; idx < hi; idx++ {
			op := ops[idx]
			fmt.Fprintf(&b, "%c%s\n", op.op, op.text)
			// A context line (' ') can carry at most one of these -- see
			// markMissingTrailingNewlines -- but a '-' only ever carries
			// old's marker and a '+' only ever carries new's, so the two
			// checks below can never both fire for the same line.
			if idx == oldMarkIdx && op.op != '+' {
				b.WriteString(noNewlineMarker + "\n")
			}
			if idx == newMarkIdx && op.op != '-' {
				b.WriteString(noNewlineMarker + "\n")
			}
		}
	}
	return b.String()
}
