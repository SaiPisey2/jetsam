# jetsam

Work in progress. Not yet released.

Finds the Prometheus metrics nothing reads, and proposes dropping them.

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
jetsam init
jetsam scan
jetsam propose
```

`propose` prints the pull request it would open -- the diff to your scrape
config and the PR body -- and opens nothing. Add `-apply -owner OWNER -repo
REPO` to actually open it, with a GitHub token in `$GITHUB_TOKEN`:

```
export GITHUB_TOKEN=...
jetsam propose -apply -owner myorg -repo myrepo
```

The token is read from the environment only -- never accept it as a flag,
since a flag value lands in `ps` output and shell history. `-apply` without
`-owner`, `-repo`, or a token is refused before anything else runs.

By default `propose` only ever proposes a metric backed by real evidence
that nothing reads it, which in v0.1 (no query log support yet) means it
proposes nothing at all. Pass `-include-unreferenced` to also propose
metrics no rule references, on faith rather than evidence -- jetsam cannot
see ad-hoc or Grafana Explore queries against them. The PR body states
which grade every drop rests on and carries an extra warning wherever that
grade is `unreferenced`; a non-empty set of unreadable rules still forbids
every drop regardless of this flag.

## License

Apache 2.0.
