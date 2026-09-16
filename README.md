# jetsam

Finds the Prometheus metrics nothing reads, and proposes dropping them.

v0.1.0 -- see Limits for what it cannot see yet.

Prometheus tells you how many series each metric has. Your rules tell you
which metrics anything actually queries. jetsam joins the two and opens a
pull request against your scrape config for the difference.

Every recommendation names the queries it was checked against, so you can
redo the check by hand.

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
config and the PR body -- and opens nothing. Add `-apply -owner OWNER -repo
REPO` to actually open it, with a GitHub token in `$GITHUB_TOKEN`:

By default `propose` only ever proposes a metric backed by real evidence
that nothing reads it, which in v0.1 (no query log support yet) means it
proposes nothing at all. Pass `-include-unreferenced` to also propose
metrics no rule references, on faith rather than evidence -- jetsam cannot
see ad-hoc or Grafana Explore queries against them. The PR body states
which grade every drop rests on and carries an extra warning wherever that
grade is `unreferenced`; a non-empty set of unreadable rules still forbids
every drop regardless of this flag.

```
export GITHUB_TOKEN=...
jetsam propose -apply -owner myorg -repo myrepo
```

Because of that, `-apply` opens nothing on a default install until you pass
`-include-unreferenced` or configure a query log.

The token is read from the environment only -- never accept it as a flag,
since a flag value lands in `ps` output and shell history. `-apply` without
`-owner`, `-repo`, or a token is refused before anything else runs.

## Limits

- **Rules are the only evidence.** jetsam reads `/api/v1/rules`. It cannot
  see Grafana dashboards, ad-hoc queries, Explore, `remote_read`, or
  anything scraping `/federate`. A metric read only through one of those
  looks unreferenced. Grafana dashboards and query-log ingestion are v0.3.
- **Nothing is proposed without evidence.** See `-include-unreferenced`
  above. Without a query log, every unread metric is graded
  `unreferenced`, which never auto-proposes.
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
- **Resolving each candidate's job is one Prometheus query.** Against a
  1374-metric instance, resolving every candidate returned by
  `-include-unreferenced` takes about 26 seconds -- this runs concurrently,
  not one query at a time.

## License

Apache 2.0.
