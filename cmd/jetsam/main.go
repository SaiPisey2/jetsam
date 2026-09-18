// Command jetsam finds Prometheus metrics nothing reads and proposes
// dropping them.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/SaiPisey2/jetsam/internal/config"
	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/emit"
	"github.com/SaiPisey2/jetsam/internal/forge"
	"github.com/SaiPisey2/jetsam/internal/grafana"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/querylog"
	"github.com/SaiPisey2/jetsam/internal/report"
	"github.com/SaiPisey2/jetsam/internal/safe"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// gather collects every evidence source the config enables. The Grafana
// token comes from the environment only -- a flag lands in ps and shell
// history, and a config file gets committed.
func gather(ctx context.Context, cfg config.Config, cl *promapi.Client, getenv func(string) string, allMetrics []string) (corpus.Sources, error) {
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		return corpus.Sources{}, err
	}
	src := corpus.Sources{Rules: rules}

	if cfg.Grafana.URL != "" {
		src.DashboardsConfigured = true
		g := grafana.New(cfg.Grafana.URL, getenv("GRAFANA_TOKEN"), cfg.Prometheus.Timeout)
		dash, derr := g.Dashboards(ctx)
		if derr != nil {
			// Not fatal: scan is still useful. But every drop is withheld,
			// because "no dashboard reads it" and "nobody looked" are
			// opposite claims with identical numbers.
			// safe.Text: a proxy in front of Grafana can put anything it
			// likes in a response body, and that body reaches this
			// string. An ANSI escape here rewrites the terminal line a
			// human is reading the warning from.
			fmt.Fprintf(os.Stderr, "warning: grafana configured but unreadable, every drop withheld: %s\n",
				safe.Text(derr.Error()))
		} else {
			src.Dashboards = dash
			src.DashboardsReachable = true
		}
	}

	if cfg.QueryLog.Path != "" {
		reading, rerr := querylog.Read(cfg.QueryLog.Path)
		if rerr != nil {
			// Not fatal, and deliberately not an empty log either. One
			// truncated line in a multi-GB rotated set used to exit scan
			// non-zero and do nothing at all; treating it as an empty log
			// instead would be far worse, since an empty log says nobody
			// queried anything. This degrades exactly as an unreachable
			// Grafana does: said loudly, graded around, every drop
			// withheld. safe.Text because the error embeds log content.
			fmt.Fprintf(os.Stderr, "warning: query log configured but unreadable, every drop withheld: %s\n",
				safe.Text(rerr.Error()))
			src.QueryLogUnreadable = true
		} else {
			src.QueryLog = reading
			if reading != nil {
				src.LogQualifies = reading.Span >= cfg.QueryLog.MinWindow
			}
		}

		// The one configuration where the query log's blind spot bites.
		// Prometheus' global.query_log_file records PromQL engine
		// evaluations only: a metric read through /api/v1/label/*/values
		// or /api/v1/series -- which is how Grafana resolves a dashboard's
		// template variables, and what Explore's metric browser uses --
		// never appears in it. With a qualifying log and no Grafana, such
		// a metric is absent from the corpus, grades unqueried, and plain
		// "jetsam propose" with no flag proposes deleting it.
		if cfg.Grafana.URL == "" {
			fmt.Fprintln(os.Stderr, "warning: query_log.path is set and grafana.url is not. Prometheus' query log "+
				"records PromQL queries only, so a metric read only through label-values or series lookups -- "+
				"how Grafana resolves dashboard variables -- is invisible to it and can grade unqueried. "+
				"Set grafana.url to cover that case.")
		}
	}
	return src, nil
}

// jobResolveWorkers bounds how many QueryJobsFor calls resolveJobs runs at
// once. promapi.Client holds only a base URL string and an *http.Client,
// both safe for concurrent use, and every call it makes is a GET -- there
// is nothing here that needs more synchronization than "don't open an
// unbounded number of sockets at once". 8 was chosen as a modest, fixed
// concurrency: enough that ~1300 candidates (the public demo's full
// -include-unreferenced run) resolve in well under a minute at ~0.17s per
// query, without turning a single propose run into a thundering herd
// against someone's Prometheus.
const jobResolveWorkers = 8

// resolveJobs resolves the job labels currently exposing each of metrics,
// one cl.QueryJobsFor call per metric, run through a bounded pool of
// jobResolveWorkers goroutines instead of one call at a time.
//
// A serial loop cannot fit inside any reasonable deadline once there are
// more than a few hundred candidates: measured against the public demo
// (smaller than any production Prometheus), ~0.17s per query times ~1300
// candidates is ~220s against a 120s default timeout -- propose -include-
// unreferenced could not complete AT ALL on the config `jetsam init`
// writes. Eight workers bring that down to ~28s.
//
// Results are collected into jobs, an indexed slice with one slot per
// metric -- never a shared map filled in from multiple goroutines. Each
// worker only ever writes the slot matching the index it was handed, so no
// synchronization is needed on the writes themselves, and the caller can
// always read jobs[i] as "the result for metrics[i]" regardless of which
// query happened to finish first. That is what keeps propose's output
// deterministic: an unstable order would make every PR diff look
// different between runs for no reason, even though nothing changed.
//
// The first error, in metrics' own index order (not completion order), is
// returned; every other query still runs to completion (there is no early
// cancellation on error), but a non-nil error means jobs must not be used
// at all.
func resolveJobs(ctx context.Context, cl *promapi.Client, metrics []string) ([][]string, error) {
	jobs := make([][]string, len(metrics))
	errs := make([]error, len(metrics))

	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobResolveWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				jobs[i], errs[i] = cl.QueryJobsFor(ctx, metrics[i])
			}
		}()
	}
	for i := range metrics {
		work <- i
	}
	close(work)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("resolve job for %s: %w", metrics[i], err)
		}
	}
	return jobs, nil
}

const usage = `jetsam finds Prometheus metrics nothing reads.

usage:
  jetsam init      [-config jetsam.yaml]
  jetsam scan      [-config jetsam.yaml]
  jetsam propose   [-include-unreferenced] [-config jetsam.yaml]
  jetsam propose   -apply -owner OWNER -repo REPO [-base main] [-include-unreferenced] [-config jetsam.yaml]
  jetsam aggregate [-config jetsam.yaml]

  -apply requires $GITHUB_TOKEN in the environment. It is never accepted as
  a flag: a flag value lands in argv, readable by every other user on the
  machine via ps, and in shell history.

  -include-unreferenced proposes dropping metrics no rule references even
  though jetsam has no query log confirming nobody reads them. Without it
  (the default), an install with no query log configured proposes nothing.

  aggregate reports metrics whose consumers only ever read a few of their
  labels, and prints the recording rule and consumer rewrites that would
  collapse them. It never writes anything -- no -apply, no rule file, no
  pull request -- see its own output for why.
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
	case "aggregate":
		runAggregate(os.Args[2:])
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
	status, err := cl.TSDBStatus(ctx, cfg.Prometheus.MetricLimit)
	if err != nil {
		fail(err)
	}

	inv := inventory.Build(status)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	src, err := gather(ctx, cfg, cl, os.Getenv, names)
	if err != nil {
		fail(err)
	}
	cor := corpus.Build(src, names)
	report.Scan(os.Stdout, inv, cor, verdict.Compute(inv, cor))
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
	// includeUnreferenced widens what propose treats as droppable beyond
	// what verdict.Compute itself decided. verdict.Compute never sets this
	// on its own -- see the spec's evidence-grade split -- because "no rule
	// reads it" and "nobody reads it" are different claims, and only the
	// second is backed by evidence in v0.1 (no query log). Left at its
	// default of false, an install with no query log configured proposes
	// nothing at all through the real CLI, by design.
	includeUnreferenced := fs.Bool("include-unreferenced", false,
		"propose dropping metrics no rule references, even though jetsam has no query log confirming nobody reads them (the operator accepts that risk)")
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
	status, err := cl.TSDBStatus(ctx, cfg.Prometheus.MetricLimit)
	if err != nil {
		logErr(err)
		return 1
	}

	inv := inventory.Build(status)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	src, err := gather(ctx, cfg, cl, getenv, names)
	if err != nil {
		logErr(err)
		return 1
	}
	cor := corpus.Build(src, names)
	res := verdict.Compute(inv, cor)
	// DashboardsMissing and QueryLogMissing must widen this the same as a
	// blocked rule does: -include-unreferenced only accepts the gap in
	// evidence that comes from never having asked for a query log, never
	// the gap that comes from evidence being configured and unavailable.
	// Checking only res.Blocked here would let that flag override
	// withholding that verdict.Compute already enforced on every verdict's
	// Droppable field.
	blocked := len(res.Blocked) > 0 || res.DashboardsMissing || res.QueryLogMissing

	var eligible []verdict.Verdict
	for _, v := range res.Verdicts {
		if eligibleForDrop(v, blocked, *includeUnreferenced) {
			eligible = append(eligible, v)
		}
	}

	// The scrape config is only read once there is at least one candidate
	// worth checking it against: a run that would propose nothing anyway
	// (the default install, or an all-blocked corpus) never needs
	// prometheus_file to exist at all.
	var promYAML []byte
	definedJobs := map[string]bool{}
	if len(eligible) > 0 {
		var err error
		promYAML, err = os.ReadFile(cfg.PrometheusFile)
		if err != nil {
			logErr(fmt.Errorf("read %s: %w", cfg.PrometheusFile, err))
			return 1
		}
		definedJobs, err = emit.JobNames(string(promYAML))
		if err != nil {
			logErr(err)
			return 1
		}
	}

	// A candidate's own grade says whether jetsam can see it is unread at
	// all; definedJobs says whether jetsam can actually WRITE that drop
	// anywhere. The two are independent, and a metric produced by several
	// jobs can be eligible on the first and only partly satisfy the
	// second: propose it for whichever jobs a scrape_config actually
	// exists for, and decline it, one job at a time, for the rest --
	// never decline the whole metric because one of several jobs is
	// missing, and never abort the whole run because Render would refuse
	// one job it was never going to be asked about.
	// Job resolution gets its own deadline, freshly started here rather
	// than continuing to draw down cfg.Prometheus.Timeout: by this point
	// that budget has already paid for TSDBStatus and
	// AlertingAndRecordingRules, and a serial QueryJobsFor per candidate
	// cannot fit what remains of it once there are more than a few hundred
	// candidates (see resolveJobs).
	jobCtx, jobCancel := context.WithTimeout(context.Background(), cfg.Prometheus.Timeout)
	defer jobCancel()

	metricNames := make([]string, len(eligible))
	for i, v := range eligible {
		metricNames[i] = v.Metric
	}
	jobsByCandidate, err := resolveJobs(jobCtx, cl, metricNames)
	if err != nil {
		logErr(err)
		return 1
	}

	var drops []emit.Drop
	var unresolved []string
	var declined []string
	unreferenced := map[string]bool{}
	for i, v := range eligible {
		jobs := jobsByCandidate[i]
		if len(jobs) == 0 {
			// Nothing is currently exposing this metric under any job
			// label, so there is no scrape config to add a drop rule to.
			// Skipped, not guessed at.
			unresolved = append(unresolved, v.Metric)
			continue
		}
		matched := false
		for _, j := range jobs {
			if !definedJobs[j] {
				declined = append(declined, fmt.Sprintf(
					"metric %s: produced by job %q, which has no scrape_config in %s",
					v.Metric, j, cfg.PrometheusFile))
				continue
			}
			matched = true
			drops = append(drops, emit.Drop{Metric: v.Metric, Series: v.Series, Job: j})
		}
		if matched && v.Grade == verdict.GradeUnreferenced {
			unreferenced[v.Metric] = true
		}
	}

	// printDeclined lists every candidate withheld because its job has no
	// scrape_config in this file -- the same information, in the same
	// style as a blocked rule, just named individually here because
	// "which job, in which file" is exactly what an operator running
	// against one file among several needs to see without reading the PR
	// body in full.
	printDeclined := func() {
		fmt.Fprintf(stdout, "declined %d metric(s): their job has no scrape_config in %s:\n", len(declined), cfg.PrometheusFile)
		for _, d := range declined {
			fmt.Fprintf(stdout, "  - %s\n", safe.Text(d))
		}
	}

	// unresolvedMsgs mirrors declined's shape -- one formatted line per
	// candidate -- so it can be printed the same way and appended to the
	// PR body's blocked list the same way. These are metrics graded
	// droppable with zero jobs currently exposing them: a target that
	// stopped being scraped, or a metric a deploy removed. They used to be
	// reported ONLY when propose found nothing else to drop at all; with
	// one or more real drops in the same run they simply vanished from
	// both the terminal and the PR body, even though a metric with no
	// current producer is among the best drop candidates there is.
	unresolvedMsgs := make([]string, len(unresolved))
	for i, m := range unresolved {
		unresolvedMsgs[i] = fmt.Sprintf(
			"metric %s: graded droppable, but no job currently exposes it, so there is no scrape config to edit",
			m)
	}
	printUnresolved := func() {
		fmt.Fprintf(stdout, "%d metric(s) graded droppable have no job currently exposing them, so there is no scrape config to edit:\n", len(unresolved))
		for _, m := range unresolved {
			fmt.Fprintf(stdout, "  - %s\n", safe.Text(m))
		}
	}

	if len(drops) == 0 {
		fmt.Fprintln(stdout, "propose: nothing to propose.")
		switch {
		case res.DashboardsMissing:
			fmt.Fprintln(stdout, "dashboard evidence was configured but could not be fetched, so every drop is "+
				"withheld until Grafana is reachable again. See `jetsam scan` for detail.")
		case res.QueryLogMissing:
			// The query log is the only source whose SILENCE is evidence,
			// so one that could not be read leaves jetsam with no negative
			// evidence at all. Withheld, not graded around -- and said
			// here rather than left to the generic "nothing unread"
			// message, which would read as a clean bill of health.
			fmt.Fprintln(stdout, "the query log was configured but could not be read, so jetsam has no evidence that "+
				"anything went unqueried and every drop is withheld. See `jetsam scan` for detail.")
		case len(res.Blocked) > 0:
			fmt.Fprintf(stdout, "%d rule(s) could not be read; a blocked rule may reference anything, "+
				"so every drop is withheld until it is fixed. See `jetsam scan` for which.\n", len(res.Blocked))
		default:
			if len(declined) > 0 {
				printDeclined()
			}
			if len(unresolved) > 0 {
				printUnresolved()
			}
			if len(declined) == 0 && len(unresolved) == 0 {
				switch {
				case *includeUnreferenced:
					fmt.Fprintln(stdout, "-include-unreferenced is set, but no rule-unreferenced metric was found either -- "+
						"there is nothing to widen the proposal with.")
				case cor.LogRead && cor.LogQualifies:
					// A qualifying query log IS configured here -- unlike the
					// case below -- so the honest reason nothing is eligible
					// is that every metric is already accounted for by a
					// rule, a dashboard, or the log itself, not that evidence
					// is missing.
					fmt.Fprintf(stdout, "every metric is already referenced by a %s, or was read "+
						"within the query log's window -- there is nothing unread to propose dropping.\n", cor.Readers())
				case cor.LogRead && !cor.LogQualifies:
					fmt.Fprintf(stdout, "query_log.path is configured, but its log only covers %s -- not long enough "+
						"to license a drop -- so jetsam cannot tell \"no rule mentions this\" apart from \"nobody reads "+
						"this\". Let it cover more of query_log.min_window, or pass -include-unreferenced to propose "+
						"dropping rule-unreferenced metrics on faith instead.\n", cor.LogSpanText())
				default:
					fmt.Fprintln(stdout, "no query log is configured, so jetsam cannot tell \"no rule mentions this\" apart from "+
						"\"nobody reads this\" -- set query_log.path to make a metric eligible on evidence, or pass "+
						"-include-unreferenced to propose dropping rule-unreferenced metrics on faith instead.")
				}
			}
		}
		// Nothing droppable means nothing to commit or open, -apply
		// included: no branch is created and no empty PR is opened. This is
		// success, not failure: a metric declined because its job lives in
		// a different file is jetsam correctly refusing to guess, exactly
		// like a blocked rule -- not a reason to exit non-zero.
		//
		// The message must say what actually happened. Printing the dry-run
		// "re-run with -apply" line unconditionally here was wrong on two
		// counts: it told an operator who already passed -apply to do
		// something they had just done, and it implied a PR would have
		// opened if only -apply had been given, when in fact there was
		// nothing to open either way.
		if *apply {
			fmt.Fprintln(stdout, "apply: nothing to propose, so nothing was opened.")
		} else {
			fmt.Fprintln(stdout, "dry run: nothing to propose, so there is nothing -apply would open either.")
		}
		return 0
	}

	if len(declined) > 0 {
		printDeclined()
		fmt.Fprintln(stdout)
	}
	if len(unresolved) > 0 {
		printUnresolved()
		fmt.Fprintln(stdout)
	}
	if len(unreferenced) > 0 {
		// The same fact the PR body states, said here too: a dry run must
		// not need the body read in full to know a drop rests on faith
		// rather than evidence.
		// The same three-state clause report.Scan and the PR body use --
		// see corpus.Corpus.LogShortfall. This said "jetsam has no query
		// log" flatly, which was wrong on every install whose log was
		// configured and merely short of the window.
		fmt.Fprintf(stdout, "-include-unreferenced is set: %d metric(s) below are not referenced by any %s, "+
			"but %s, so jetsam cannot see ad-hoc or Grafana Explore queries against them. "+
			"You have accepted that risk.\n\n", len(unreferenced), cor.Readers(), cor.LogShortfall())
	}

	newYAML, err := emit.Render(string(promYAML), drops)
	if err != nil {
		logErr(err)
		return 1
	}

	diff := unifiedDiff(cfg.PrometheusFile, string(promYAML), newYAML)
	if diff == "" {
		// The target file already carries every drop rule this run would
		// propose -- the ordinary case on a second run after the last
		// proposal merged. There is nothing to commit: falling through to
		// applyAndReport here would create a branch, commit byte-identical
		// content onto it, and then have GitHub reject OpenPR with 422 "No
		// commits between", every single run, leaving a stray branch behind
		// each time. Return before touching the forge at all, with -apply
		// or without it -- neither has anything to do.
		fmt.Fprintf(stdout, "%s already carries every drop rule below; nothing to open.\n", cfg.PrometheusFile)
		return 0
	}
	fmt.Fprint(stdout, diff)
	fmt.Fprintln(stdout)

	// Every declined-by-missing-job candidate, and every unresolved one, is
	// reported in the PR body exactly like a blocked rule: same section,
	// same count, so "how many things jetsam refused to touch" stays the
	// one number that matters, not "how many metrics were dropped".
	bodyRes := res
	bodyRes.Blocked = append(append(append([]string(nil), res.Blocked...), declined...), unresolvedMsgs...)
	title, body := emit.Body(drops, bodyRes, cor, inv, cfg.Pricing.PerSeriesMonth)
	fmt.Fprintf(stdout, "%s\n\n%s\n\n", title, body)

	if !*apply {
		fmt.Fprintln(stdout, "dry run: nothing opened. Re-run with -apply to open this PR.")
		return 0
	}

	// The forge phase (EnsureBranch, CommitFiles, OpenPR) gets its own
	// context, sized for a GitHub write rather than a Prometheus read, and
	// started fresh here rather than reusing ctx -- which was sized by
	// cfg.Prometheus.Timeout and has already been spent on TSDBStatus and
	// AlertingAndRecordingRules. Sharing it would mean a Prometheus-sized
	// deadline could expire between CommitFiles and OpenPR: the branch
	// would then hold jetsam's commit but no PR would exist, and the next
	// run's FindPR returns nil (nothing to reuse), EnsureBranch correctly
	// refuses to reset a branch that might carry a human's own push, and
	// CommitFiles refuses forever because the branch blob no longer
	// matches BaseContent -- with an error telling the operator to
	// "re-run against a fresh checkout", which cannot actually help.
	// GitHubProvider already applies its own 30s-per-request timeout on
	// top of this; 5 minutes is comfortably above what EnsureBranch,
	// CommitFiles and OpenPR need together even under retries.
	forgeCtx, forgeCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer forgeCancel()

	return applyAndReport(forgeCtx, stdout, stderr, newProvider(token), *owner, *repo, *base, cfg.PrometheusFile, string(promYAML), newYAML, title, body)
}

// eligibleForDrop reports whether v should be treated as droppable for this
// run of propose. verdict.Compute already decided v.Droppable under
// whatever evidence it had -- rules, dashboards and a qualifying query log,
// when configured. This function only ever WIDENS that decision, and only
// for the one grade an operator can explicitly choose to accept without
// full evidence: GradeUnreferenced, when includeUnreferenced was passed.
//
// It never widens a GradeUsed verdict -- a rule reads the metric, or a
// recording rule writes it -- because that grade means something read it,
// not merely that jetsam lacks proof nothing did; -include-unreferenced is
// about accepting a gap in evidence, not overriding evidence that exists.
// And it never widens anything at all once blocked is true: an unreadable
// query might reference anything, the same reason verdict.Compute itself
// withholds every drop in that case, and this flag does not get to
// override it.
func eligibleForDrop(v verdict.Verdict, blocked, includeUnreferenced bool) bool {
	if v.Droppable {
		return true
	}
	return includeUnreferenced && !blocked && v.Grade == verdict.GradeUnreferenced
}

// applyAndReport calls emit.Apply through p and reports the outcome on
// stdout, or the error on stderr. It is everything -apply does once there
// is something to propose, factored out so it can be driven straight
// against forge.NewFakeProvider() in a test without a real scan first --
// and notably, it never takes a token: by the time p exists,
// authentication is already p's own business (see the comment on
// emit.Apply).
//
// pr.State distinguishes three outcomes an operator needs to be able to
// tell apart, not just "success": already open (nothing new happened),
// already merged (the change landed some other way), and closed (a human
// already declined this exact proposal -- emit.Apply does not reopen it,
// and this must not report that as if a PR had just been opened).
func applyAndReport(ctx context.Context, stdout, stderr io.Writer, p forge.Provider, owner, repo, base, path, oldYAML, newYAML, title, body string) int {
	pr, err := emit.Apply(ctx, p, owner, repo, base, path, oldYAML, newYAML, title, body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch pr.State {
	case "closed":
		fmt.Fprintf(stdout, "a previous proposal on branch %s was closed (#%d, %s); not reopening it. "+
			"Change what jetsam would propose to get a new pull request.\n", pr.Branch, pr.Number, pr.URL)
	case "merged":
		fmt.Fprintf(stdout, "a previous proposal on branch %s was already merged (#%d, %s); nothing to do.\n", pr.Branch, pr.Number, pr.URL)
	default: // "open": newly opened, or an already-open PR found and reused.
		fmt.Fprintf(stdout, "%s: %s\n", pr.State, pr.URL)
	}
	return 0
}

// runAggregate is the production entry point for `jetsam aggregate`. Real
// stdio and the real environment, the same seam runPropose uses -- and
// nothing else: aggregate opens no pull request, so it needs no
// forge.Provider and no credential of any kind.
func runAggregate(args []string) {
	os.Exit(aggregateCmd(args, os.Stdout, os.Stderr, os.Getenv))
}

// aggregateCmd implements `jetsam aggregate`: a report, not an edit. It
// builds the inventory and corpus through exactly gather() and corpus.Build
// -- the same path proposeCmd uses -- because a second, divergent way of
// answering "what does the corpus say" would mean the two subcommands could
// disagree about the same install, and only one of them could be right.
//
// From there it is arithmetic and rendering, never a write: it measures
// each of aggregate.Decide's proposals against the real series count,
// withholds whatever that measurement rules out, fills in the consumers and
// the rendered rule and rewrites, and prints report.Aggregate -- which
// states, on every call, that none of this has been applied. See that
// package's own doc comment for why: a recording rule cannot read a metric
// dropped at ingest, so the rule and rewrite this prints are only safe to
// apply by hand alongside a drop that never happens.
func aggregateCmd(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "jetsam.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logErr := func(err error) { fmt.Fprintf(stderr, "error: %v\n", err) }

	cfg, err := config.Load(*path)
	if err != nil {
		logErr(err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Prometheus.Timeout)
	defer cancel()

	cl := promapi.New(cfg.Prometheus.URL, cfg.Prometheus.Timeout)
	status, err := cl.TSDBStatus(ctx, cfg.Prometheus.MetricLimit)
	if err != nil {
		logErr(err)
		return 1
	}

	inv := inventory.Build(status)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}

	src, err := gather(ctx, cfg, cl, getenv, names)
	if err != nil {
		logErr(err)
		return 1
	}
	cor := corpus.Build(src, names)

	proposals, refusals := aggregate.Decide(inv, cor, dashboardConsumers(src.Dashboards, names))

	// Measuring against the real Prometheus is its own budget, separate
	// from cfg.Prometheus.Timeout above: by this point that deadline has
	// already paid for TSDBStatus, AlertingAndRecordingRules and whatever
	// gather() spent on Grafana or the query log. One instant query per
	// proposal is cheap next to resolveJobs' per-candidate cost in
	// propose, but a large corpus can still hold dozens of proposals, and
	// a fresh deadline sized for that is more honest than drawing down
	// whatever happens to remain.
	measureCtx, measureCancel := context.WithTimeout(context.Background(), cfg.Prometheus.Timeout)
	defer measureCancel()
	hc := &http.Client{Timeout: cfg.Prometheus.Timeout}

	var findings []report.AggregateFinding
	var withheld []string
	for _, p := range proposals {
		kept, err := measureKeptSeries(measureCtx, cfg.Prometheus.URL, hc, p.Metric, p.Keep)
		if err != nil {
			// A proposal whose measurement query fails is withheld, not
			// reported with a missing or zero number: see the brief this
			// command implements. Said on stderr, the same way gather()
			// reports a degraded source, rather than silently dropped.
			withheld = append(withheld, fmt.Sprintf("%s: could not measure the aggregated series count: %v", p.Metric, err))
			continue
		}
		p.KeptSeries = kept

		if reason := withholdReason(p, cfg.Aggregate.MinSeriesSaved); reason != "" {
			withheld = append(withheld, fmt.Sprintf("%s: %s", p.Metric, reason))
			continue
		}

		p.Consumers = rulesReading(p.Metric, src.Rules, names)

		ruleYAML, err := emit.RenderRule("", p)
		if err != nil {
			// RenderRule refusing here means p disagrees with itself --
			// Decide's own output should never do that -- so this is
			// withheld exactly like an unmeasured proposal rather than
			// printed with a rule that failed to render.
			withheld = append(withheld, fmt.Sprintf("%s: could not render its recording rule: %v", p.Metric, err))
			continue
		}

		consumers := make([]report.ConsumerFinding, 0, len(p.Consumers))
		for _, c := range p.Consumers {
			consumers = append(consumers, buildConsumerFinding(c, p))
		}

		findings = append(findings, report.AggregateFinding{Proposal: p, Rule: ruleYAML, Consumers: consumers})
	}

	if len(withheld) > 0 {
		fmt.Fprintf(stderr, "warning: %d proposal(s) withheld:\n", len(withheld))
		for _, w := range withheld {
			fmt.Fprintln(stderr, "  - "+safe.Text(w))
		}
	}

	report.Aggregate(stdout, findings, refusals)
	return 0
}

// withholdReason reports why a measured proposal must not be printed, or ""
// when it should be. It holds two checks that look redundant today and must
// not be collapsed into one:
//
//   - KeptSeries >= RawSeries is a CORRECTNESS floor, the spec's own
//     "required(M) must be a strict subset of M's observed labels":
//     collapsing to the kept labels still yields as many series as the
//     metric has today, so those labels already identify every series and
//     the rule would save nothing at all. This can never be configured
//     away -- there is no threshold at which "saves nothing" becomes worth
//     naming.
//   - a saving below minSaved is a PREFERENCE: real, but not worth asking a
//     person to add a recording rule and rewrite every consumer by hand
//     for. See aggregate.min_series_saved's own doc comment in
//     internal/config. Unlike the floor above, an operator can reasonably
//     want this at zero one day -- name every real saving, however small.
//
// The two happen to overlap completely under any config config.Load can
// produce TODAY, because Load treats min_series_saved: 0 as "unset" and
// replaces it with a positive default (100): whenever KeptSeries >=
// RawSeries the saving is zero or negative, which already fails a positive
// minSaved on its own. That overlap is an artifact of Load's current
// default, not a reason to merge the checks -- the day min_series_saved can
// genuinely be zero, only the first check still catches "saves nothing".
// minSaved is passed as a plain int rather than read from cfg for exactly
// this reason: it lets a test pin minSaved at 0, a value Load itself can
// never produce, and observe the first check on its own.
func withholdReason(p aggregate.Proposal, minSaved int) string {
	if p.KeptSeries >= p.RawSeries {
		return fmt.Sprintf("collapsing to (%s) still measures %d series, as many as the %d it has today -- nothing would be saved",
			strings.Join(p.Keep, ", "), p.KeptSeries, p.RawSeries)
	}
	if saved := p.RawSeries - p.KeptSeries; saved < minSaved {
		return fmt.Sprintf("saves only %d series, below the configured minimum of %d", saved, minSaved)
	}
	return ""
}

// dashboardConsumers reports, for each metric, the title of every dashboard
// that reads it -- the per-dashboard breakdown aggregate.Decide's
// dashboardConsumers parameter needs to name a blocking dashboard, which
// Corpus itself does not keep (see Corpus.Used/UsedBy, which only remember
// THAT a dashboard read something, not which one). It is recomputed here
// from the same dashboards gather() already fetched, using the same
// Extract+Resolve corpus.Build uses internally to decide FromDashboard,
// rather than only from panel queries: a template variable
// (label_values(metric, ...)) is exactly as real a read as a panel, and a
// dashboard's own Grafana loads it every time the dashboard opens.
func dashboardConsumers(dashboards []grafana.Dashboard, allMetrics []string) map[string][]string {
	out := map[string][]string{}
	name := func(title string, queries []string) {
		for _, q := range queries {
			refs, err := corpus.Extract(corpus.Substitute(q))
			if err != nil {
				// Contained the same way corpus.Build itself contains it:
				// an unparseable panel already sets the FromDashboard bit
				// through Corpus.Build's own fallback, which Decide checks
				// independently of this map (see decide.go's comment on
				// why both are checked). This map only ever adds a NAME
				// when one is actually known.
				continue
			}
			for _, m := range refs.Resolve(allMetrics) {
				if !containsString(out[m], title) {
					out[m] = append(out[m], title)
				}
			}
		}
	}
	for _, d := range dashboards {
		name(d.Title, d.Queries)
		name(d.Title, d.VariableQueries)
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// rulesReading returns, as aggregate.Consumer values, every rule in rules
// whose query touches metric -- what fills aggregate.Proposal.Consumers,
// which Decide itself leaves nil because Corpus carries no rule list (see
// Proposal's own doc comment). It uses the same corpus.Extract().Resolve()
// pair corpus.Build folds every rule through, so a rule counted here is
// exactly a rule corpus.Build would have counted as a FromRule reader of
// metric -- and a rule that fails to parse is skipped, not guessed at: an
// unparseable rule already withholds every proposal by way of c.Blocked
// (checked in aggregate.Decide), so no proposal for metric can exist for
// this loop to be wrong about in the first place.
func rulesReading(metric string, rules []promapi.Rule, allMetrics []string) []aggregate.Consumer {
	var out []aggregate.Consumer
	for _, r := range rules {
		refs, err := corpus.Extract(r.Query)
		if err != nil {
			continue
		}
		for _, m := range refs.Resolve(allMetrics) {
			if m == metric {
				out = append(out, aggregate.Consumer{Kind: "rule", Group: r.Group, Name: r.Name, Query: r.Query})
				break
			}
		}
	}
	return out
}

// buildConsumerFinding calls emit.RewriteConsumer for one consumer of p's
// metric and turns its answer into a report.ConsumerFinding.
//
// RewriteConsumer's own contract allows a rewrite and a decline together:
// one aggregate in c.Query can match p and get replaced while a second,
// different aggregate over the same metric elsewhere in the same
// expression does not and is left reading the raw series (see
// RewriteConsumer's doc comment, and report.ConsumerFinding's). Both return
// values are carried straight through unconditionally for exactly that
// reason -- collapsing them into "rewritten, so ignore Declined" would
// state something false about a consumer that still partly reads the raw
// metric.
func buildConsumerFinding(c aggregate.Consumer, p aggregate.Proposal) report.ConsumerFinding {
	rewritten, declined, err := emit.RewriteConsumer(c.Query, p)
	if err != nil {
		// RewriteConsumer only errors when p itself is not internally
		// consistent, or when the rewrite it produced fails to re-parse --
		// both a defect in jetsam's own pipeline, never a fact this
		// specific consumer's query stated. Reported as not rewritable,
		// with the query left unchanged, rather than silently dropped: see
		// report.ConsumerFinding's doc comment on why no consumer is ever
		// left out of this list.
		return report.ConsumerFinding{Consumer: c, Rewritten: c.Query, Declined: err.Error()}
	}
	return report.ConsumerFinding{Consumer: c, Rewritten: rewritten, Declined: declined}
}

// aggregateQueryMaxBytes bounds the instant-query response
// measureKeptSeries reads, mirroring promapi's own maxResponseBytes: a
// malformed or hostile response must produce a clear error, never a
// silently truncated decode that reads as "this aggregate has fewer series
// than it actually does".
const aggregateQueryMaxBytes = 64 << 20

// measureKeptSeries issues one instant query counting how many series
// aggregating metric down to keep would produce:
// count(count by (<keep>) ({__name__="<metric>"})). It cannot reuse
// promapi.Client for this -- QueryJobsFor answers a different question and
// its request-building is unexported -- so it mirrors that idiom directly:
// the metric name is passed through %q, never concatenated bare, because a
// metric name is untrusted input (Prometheus 3 permits UTF-8 names) and
// this repo has already shipped one injection through exactly this kind of
// interpolation. When keep is empty the clause is "by ()", which collapses
// every series to one -- correct for a consumer that aggregates the metric
// down to a single number.
func measureKeptSeries(ctx context.Context, baseURL string, hc *http.Client, metric string, keep []string) (int, error) {
	by := "()"
	if len(keep) > 0 {
		by = "(" + strings.Join(keep, ", ") + ")"
	}
	query := fmt.Sprintf("count(count by %s ({__name__=%q}))", by, metric)

	u := strings.TrimRight(baseURL, "/") + "/api/v1/query?" + (url.Values{"query": {query}}).Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("build measurement query for %s: %w", metric, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", metric, err)
	}
	defer resp.Body.Close()

	lr := &io.LimitedReader{R: resp.Body, N: aggregateQueryMaxBytes + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		return 0, fmt.Errorf("read measurement response for %s: %w", metric, err)
	}
	if lr.N == 0 {
		return 0, fmt.Errorf("measurement response for %s exceeds %d byte limit", metric, aggregateQueryMaxBytes)
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, fmt.Errorf("decode measurement response for %s: %w", metric, err)
	}
	if env.Status != "success" {
		return 0, fmt.Errorf("measure %s: %s: %s", metric, env.ErrorType, env.Error)
	}
	if len(env.Data.Result) != 1 {
		return 0, fmt.Errorf("measure %s: instant query returned %d result(s), want exactly 1", metric, len(env.Data.Result))
	}
	// Prometheus's instant-query API encodes a sample's value as
	// [timestamp, "value string"] -- the value itself is a JSON string, not
	// a bare number, so it is decoded as one and parsed rather than
	// unmarshalled directly into a float64.
	var s string
	if err := json.Unmarshal(env.Data.Result[0].Value[1], &s); err != nil {
		return 0, fmt.Errorf("decode measurement value for %s: %w", metric, err)
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse measurement value %q for %s: %w", s, metric, err)
	}
	return int(n), nil
}

// diffContext is how many unchanged lines surround a change in the
// unified diff propose prints, matching the context width forge/edit.go
// uses for the same kind of human-readable patch.
const diffContext = 3

// diffLine is one line of an old/new comparison, tagged with how it
// changed: ' ' unchanged, '-' removed, '+' added. noEOL carries forward
// eofLine's flag -- see eofLine -- so formatHunks knows, for this exact
// printed line, whether it is a file's true final line with no trailing
// newline.
type diffLine struct {
	op    byte
	text  string
	noEOL bool
}

// unifiedDiff renders old -> new in the style of `diff -u`, computed from a
// longest common subsequence of lines. It exists so a reviewer can check
// exactly what jetsam would write to path rather than trusting jetsam's own
// description of the change -- which only holds if the diff is one `patch`
// can actually apply, trailing newline included.
func unifiedDiff(path, oldText, newText string) string {
	oldLines := eofLines(splitLines(oldText), hasTrailingNewline(oldText))
	newLines := eofLines(splitLines(newText), hasTrailingNewline(newText))
	ops := normalizeChangeOrder(diffLCS(oldLines, newLines))
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

// eofLine is one line of a file, together with whether it is that file's
// own true final line while the file itself has no trailing newline.
//
// diffLCS compares eofLine values for equality, not bare text, and that is
// the whole point of this type: a line's text being identical to a line in
// the other file is not the same fact as the two of them agreeing about
// where their file ends. Comparing on text alone lets a file's true final
// line be silently treated as an ordinary shared context line matching
// some line elsewhere that is emphatically not that file's end -- which is
// exactly how a "no trailing newline" fact for one file gets attached to a
// position that, for the OTHER file, has more content after it. Requiring
// noEOL to match too forces diffLCS itself to treat such a line as a real
// change (an explicit removal and/or addition) whenever the two files
// disagree about it, which is what real diff/patch tooling does, and which
// is what keeps every later "\ No newline" marker attached to a line that
// is provably that file's actual last line with nothing of that file's own
// content after it anywhere in the diff.
type eofLine struct {
	text  string
	noEOL bool
}

// eofLines tags lines with eofLine.noEOL: true for the last element only,
// and only when hasNL is false.
func eofLines(lines []string, hasNL bool) []eofLine {
	out := make([]eofLine, len(lines))
	for i, t := range lines {
		out[i] = eofLine{text: t}
	}
	if n := len(out); n > 0 && !hasNL {
		out[n-1].noEOL = true
	}
	return out
}

// diffLCS returns old and new merged into one ordered list of kept, removed
// and added lines, via the longest common subsequence of the two. old and
// new are configuration files -- at most a few thousand lines -- so the
// O(len(old)*len(new)) table this builds is not a concern.
//
// Equality is eofLine equality (text AND noEOL), not text alone -- see
// eofLine. This is what makes diffLCS itself choose the same alignment a
// real diff tool would when either file's true final line is involved:
// old="a\nb\n" against new="a" cannot line up old's "a" with new's "a" as
// shared context, because new's "a" is its file's own (newline-less) end
// and old's is not, so both of old's lines end up explicit removals and
// new's "a" an explicit addition -- never a mismatched shared line quietly
// carrying a fact that is only true for one side of it.
func diffLCS(old, new []eofLine) []diffLine {
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
			ops = append(ops, diffLine{' ', old[i-1].text, old[i-1].noEOL})
			i--
			j--
		case dp[i-1][j] >= dp[i][j-1]:
			ops = append(ops, diffLine{'-', old[i-1].text, old[i-1].noEOL})
			i--
		default:
			ops = append(ops, diffLine{'+', new[j-1].text, new[j-1].noEOL})
			j--
		}
	}
	for i > 0 {
		ops = append(ops, diffLine{'-', old[i-1].text, old[i-1].noEOL})
		i--
	}
	for j > 0 {
		ops = append(ops, diffLine{'+', new[j-1].text, new[j-1].noEOL})
		j--
	}
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

// normalizeChangeOrder reorders every maximal run of consecutive non-context
// ops (no ' ' between them) so every removal ('-') in the run precedes
// every addition ('+'), preserving each side's own relative order. This is
// the conventional diff -u ordering for a substitution, and it is not
// merely cosmetic: diffLCS's backtrack, followed by the reversal needed to
// present ops oldest-to-newest, can leave a '+' printed before a '-' that
// logically replaces the same line (which side's tie-break the DP favors
// decides which). Real `patch` accepts a hunk where a '+' is immediately
// followed, with nothing between them, by a '-' -- but not when a
// "\ No newline at end of file" marker sits between them: "marker, then
// more content" is what patch calls malformed, and only the conventional
// removals-then-additions order guarantees a marker attached to a file's
// true final line is never followed by anything else for that run.
func normalizeChangeOrder(ops []diffLine) []diffLine {
	out := make([]diffLine, 0, len(ops))
	for i := 0; i < len(ops); {
		if ops[i].op == ' ' {
			out = append(out, ops[i])
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].op != ' ' {
			j++
		}
		for k := i; k < j; k++ {
			if ops[k].op == '-' {
				out = append(out, ops[k])
			}
		}
		for k := i; k < j; k++ {
			if ops[k].op == '+' {
				out = append(out, ops[k])
			}
		}
		i = j
	}
	return out
}

// formatHunks groups ops into unified-diff hunks, each padded with up to
// diffContext lines of unchanged context on either side, merging two change
// regions whose padding would otherwise overlap into a single hunk.
// noNewlineMarker is printed immediately after any op whose noEOL is set.
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
		for idx := lo; idx < hi; idx++ {
			op := ops[idx]
			fmt.Fprintf(&b, "%c%s\n", op.op, op.text)
			if op.noEOL {
				b.WriteString(noNewlineMarker + "\n")
			}
		}
	}
	return b.String()
}
