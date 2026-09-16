// Command jetsam finds Prometheus metrics nothing reads and proposes
// dropping them.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/config"
	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/emit"
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

// runPropose repeats the scan pipeline, then prints the pull request it
// would open: the unified diff of the scrape config it would edit, and the
// PR body. Nothing is written to disk and nothing reaches GitHub -- that is
// a later command. This is a dry run, full stop.
func runPropose(args []string) {
	fs := flag.NewFlagSet("propose", flag.ExitOnError)
	path := fs.String("config", "jetsam.yaml", "config file")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		fail(err)
	}
	if cfg.PrometheusFile == "" {
		fail(fmt.Errorf("propose requires prometheus_file to be set in %s", *path))
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
			fail(fmt.Errorf("resolve job for %s: %w", v.Metric, err))
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
		fmt.Println("propose: nothing to propose.")
		switch {
		case len(res.Blocked) > 0:
			fmt.Printf("%d rule(s) could not be read; a blocked rule may reference anything, "+
				"so every drop is withheld until it is fixed. See `jetsam scan` for which.\n", len(res.Blocked))
		case len(unresolved) > 0:
			fmt.Printf("%d metric(s) graded droppable have no job currently exposing them, so there is no scrape config to edit; skipped.\n", len(unresolved))
		default:
			fmt.Println("no query log is configured, so jetsam cannot tell \"no rule mentions this\" apart from " +
				"\"nobody reads this\" -- set query_log.path to make any metric eligible for a drop.")
		}
		fmt.Println("dry run: nothing opened. Re-run with -apply to open this PR.")
		return
	}

	promYAML, err := os.ReadFile(cfg.PrometheusFile)
	if err != nil {
		fail(fmt.Errorf("read %s: %w", cfg.PrometheusFile, err))
	}
	newYAML, err := emit.Render(string(promYAML), drops)
	if err != nil {
		fail(err)
	}

	if diff := unifiedDiff(cfg.PrometheusFile, string(promYAML), newYAML); diff != "" {
		fmt.Print(diff)
	} else {
		fmt.Printf("(no change: %s already carries every drop rule below)\n", cfg.PrometheusFile)
	}
	fmt.Println()

	title, body := emit.Body(drops, res, cor.Queries, inv.TotalSeries, cfg.Pricing.PerSeriesMonth)
	fmt.Printf("%s\n\n%s\n\n", title, body)
	fmt.Println("dry run: nothing opened. Re-run with -apply to open this PR.")
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
// description of the change.
func unifiedDiff(path, oldText, newText string) string {
	ops := diffLCS(splitLines(oldText), splitLines(newText))
	return formatHunks(path, ops)
}

// splitLines splits on "\n" without discarding a final line that has no
// trailing newline, so a file missing one still diffs correctly.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
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
func formatHunks(path string, ops []diffLine) string {
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
		for _, op := range ops[lo:hi] {
			fmt.Fprintf(&b, "%c%s\n", op.op, op.text)
		}
	}
	return b.String()
}
