# jetsam

Finds the Prometheus metrics nothing reads, and proposes dropping them.

v0.2.0 -- see Limits for what it cannot see yet.

Prometheus tells you how many series each metric has. Your rules tell you
which metrics anything actually queries. jetsam joins the two and opens a
pull request against your scrape config for the difference.

Every recommendation names the queries it was checked against, so you can
redo the check by hand.

![jetsam scanning the public Prometheus demo, proposing nothing, then writing relabel rules once told to](.github/assets/demo.gif)

Recorded against [prometheus.demo.prometheus.io](https://prometheus.demo.prometheus.io)
with `demo/record.sh`. Every number in it is real.

## Install

```
go install github.com/SaiPisey2/jetsam/cmd/jetsam@latest
```

## Use

```
jetsam init                      # writes jetsam.yaml
$EDITOR jetsam.yaml              # set prometheus.url, and prometheus_file for propose
jetsam scan
jetsam propose -include-unreferenced
```

`propose` prints the pull request it would open -- the diff to your scrape
config and the PR body -- and opens nothing.

By default `propose` only ever proposes a metric backed by real evidence
that nothing reads it. With no dashboards and no query log configured that
means it proposes nothing at all -- `-include-unreferenced` is the escape
hatch for that install, and it means accepting a metric on faith (no rule
references it, but jetsam cannot see ad-hoc or Grafana Explore queries
against it):

```
jetsam propose -include-unreferenced
```

Configure `grafana.url` and `query_log.path` instead, and a metric earns
its drop on evidence: no rule or dashboard references it, and a query log
covering at least `query_log.min_window` shows nothing read it either. On
that install, plain `propose` -- no flag -- proposes drops on its own:

```
jetsam propose
```

The PR body states which grade every drop rests on, how many dashboards
were read, and how much of the query log's window backs an `unqueried`
verdict -- and carries an extra warning wherever a drop rests on
`unreferenced` instead. A non-empty set of unreadable rules, or a Grafana
that is configured but unreachable, still forbids every drop regardless of
`-include-unreferenced`.

`grafana.url` is optional and separate from the credential: set
`$GRAFANA_TOKEN` in the environment, never in `jetsam.yaml` or as a flag --
the same reasoning as `$GITHUB_TOKEN` below.

Add `-apply -owner OWNER -repo REPO` to actually open it, with a GitHub
token in `$GITHUB_TOKEN`:

```
export GITHUB_TOKEN=...
jetsam propose -apply -owner myorg -repo myrepo
```

Because of that, `-apply` opens nothing on a default install until you pass
`-include-unreferenced` or configure a qualifying query log (optionally
alongside `grafana.url`, which only ever narrows what looks unread).

The token is read from the environment only -- never accept it as a flag,
since a flag value lands in `ps` output and shell history. `-apply` without
`-owner`, `-repo`, or a token is refused before anything else runs.

## Limits

- **Rules, dashboards and the query log are still not everything.** jetsam
  reads `/api/v1/rules`, every Grafana dashboard `grafana.url` can see, and
  the query log at `query_log.path`. It still cannot see ad-hoc queries run
  outside that log's window, `remote_read`, or anything scraping
  `/federate`. A metric read only through one of those looks unreferenced
  or, if it happens to also predate the log's window, unqueried in error.
- **Nothing is proposed without evidence.** See `-include-unreferenced`
  above. Without a qualifying query log, every unread metric is graded
  `unreferenced`, which never auto-proposes.
- **Grafana configured but unreachable withholds every drop.** `scan`
  still runs and still grades from rules, but "no dashboard reads it" and
  "nobody looked" produce identical numbers, so nothing is proposed until
  Grafana answers again -- `-include-unreferenced` does not override this.
- **`min_window` is a Go duration, not a calendar one.** `720h`, not
  `30d` -- `d` is not a Go duration unit and `jetsam.yaml` fails to parse
  rather than silently treating the minimum as zero.
- **One unreadable rule blocks every drop.** A rule jetsam cannot parse
  might reference anything, so nothing is proposed until it is fixed.
  `jetsam scan` names which.
- **The inventory can be truncated.** `prometheus.metric_limit` caps how
  many metric names are graded. When it truncates, jetsam says so in both
  the report and the pull request -- but metrics outside the cap are simply
  not considered.
- **The generated diff normalises YAML layout.** Blank lines between scrape
  configs are dropped and indentation is normalised to two spaces. The dry
  run prints the exact diff; read it before `-apply`.
- **Reverting restores collection, not history.** Series not written while
  a drop rule is live cannot be recovered.
- **`-apply` has never opened a real pull request.** The branch, the
  stale-blob refusals and the "do not reopen a closed PR" behaviour are
  tested against a fake forge and a stubbed GitHub API, and the dry run is
  correct against real scrape configs. Only the live write path is
  unproven.
- **Resolving each candidate's job is one Prometheus query.** Against a
  1374-metric instance, resolving every candidate returned by
  `-include-unreferenced` takes about 26 seconds -- this runs concurrently,
  not one query at a time.

## License

Apache 2.0.
