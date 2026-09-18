# Vendored fixtures

`fixture/` is the local compose stack the tests run against; it is not a
demo and is not referenced by the README. The README's GIF is recorded by
`demo/demo.sh` and `demo/record.sh`, which run against the public
Prometheus demo instance on the internet and share nothing with this
stack — no config, no data, no assertions. Do not tune fixture data to
make the GIF read better: the GIF never sees it.

Everything here is fetched by `fixture/vendor.sh` and committed. Nothing here
is hand-edited. These artifacts decide what the stack counts as a "used"
metric, and they were written by people with no knowledge of this project
— which is the only reason they are worth testing against.

Bumping a pin: edit the version in `fixture/vendor.sh`, re-run it, then run
`go test ./fixture/ -run TestVendored`. It will fail, telling you exactly
which counts moved.

Every count pinned from a vendored artifact lives in exactly two places:
the tables in this file, and the top of `fixture/vendor_test.go` -- the
`wantGroups`, `wantRules`, `wantRecording`, `wantAlerting`,
`wantPanelQueries` and `wantParsingQueries` constants, plus the per-file
`vendoredRuleFiles` table beside them. `fixture/stack_test.go` and
`fixture/verdict_test.go` reference those constants rather than repeating
the numbers, so updating the constants and this file in the same commit
is the whole procedure.

## Prometheus rules

Source: `prometheus-operator/kube-prometheus`, `manifests/`
Pin: `v0.18.0`
Fetched: 2026-09-18
Transform: `fixture/vendortool` lifts `spec.groups` out of the
`PrometheusRule` custom resource into a plain Prometheus rule file.

| File | Groups | Rules | Recording | Alerting |
| --- | --- | --- | --- | --- |
| `prometheus/rules/node-exporter.yaml` | 2 | 41 | 15 | 26 |
| `prometheus/rules/prometheus.yaml` | 1 | 23 | 0 | 23 |
| **total** | **3** | **64** | **15** | **49** |

The scrape job names in `fixture/prometheus/prometheus.yml` are chosen to
match these rules' selectors, not the other way round: every node-exporter
rule selects `job="node-exporter"`, and every prometheus rule selects
`job="prometheus-k8s", namespace="monitoring"`. The rules are the upstream
artifact, so they are the authority. Renaming a job to something tidier
like `node` or `prometheus` does not fail anything loudly -- the rules
still load and still evaluate `health: ok` -- it just makes all 15
recording rules produce zero series and all 49 alerting rules permanently
inactive, which is a fixture that measures nothing.

## Grafana dashboard

Source: https://grafana.com/api/dashboards/1860 — "Node Exporter Full"
Pin: revision `45`
Fetched: 2026-09-18
Transform: none.

| Property | Value |
| --- | --- |
| Panel queries | 284 |
| Panel queries containing a template variable | 284 (all of them) |
| Panel queries that parse as PromQL unsubstituted | 160 |
| Panel queries that fail to parse unsubstituted | 124 |

The last two rows are the fixture's most important property, and the
split is the point. Containing a template variable does not make a query
unparseable: a `$` inside a label value (`instance="$node"`) is perfectly
valid PromQL. Every one of the 124 failures is a `$` inside a *duration*
(`[$__rate_interval]`), which is a syntax error. Measured with
`parser.NewParser(parser.Options{})`, the same parser `internal/corpus`
uses.

So a corpus reader with no substitution does not refuse this dashboard —
it silently succeeds on 160 of its queries and harvests 132 distinct
metric names from them. The recovered subset is biased, and the bias is
total rather than a tendency: all 124 failing queries call `rate()`,
`increase()` or `delta()`, and none of the 160 that parse does. The
recovered corpus is precisely the non-rate half of the dashboard.

A partial corpus that looks like it is working is more dangerous than
uniform refusal, because nothing announces the gap: a metric read only by
a `rate()` panel would grade as unread. Sub-project B has to substitute,
and must not treat "most queries parsed" as good enough.

## Container images

Pinned by digest in `fixture/docker-compose.yml`.

| Image | Version | Digest |
| --- | --- | --- |
| `prom/prometheus` | v3.13.0 | `sha256:c6b27ea434f8389bfe233fbc7be381cf50587c286e871bc842008f5a1b1908a7` |
| `prom/node-exporter` | v1.10.0 | `sha256:dbb30baa213b9a8c9aff7fe00c124ec371752c92215f014ec6f26159e9c55ff3` |
| `grafana/grafana` | 12.3.0 | `sha256:70d9599b186ce287be0d2c5ba9a78acb2e86c1a68c9c41449454d0fc3eeb84e8` |

## Query log

Query logging is turned on via `global.query_log_file` in
`fixture/prometheus/prometheus.yml`, not a CLI flag -- `--query.log-file`
does not exist in Prometheus v3.13.0 and crashes the server on startup.
The destination is bind-mounted to `fixture/querylog/queries.log` on the
host. `fixture/query.sh` issues 3 queries.

### Composition of the log (a property, not a pin)

Nothing below is asserted by any test and none of it should be treated as
a count to keep up to date; the numbers move with every second the stack
runs. The stable property is the one worth knowing: rule evaluations
dominate the query log. They are above 95% of entries within moments of
startup and climb towards ~99.5% the longer the stack runs, because
`fixture/query.sh`'s reads are one-shot while rule evaluation is continuous.

Entries carrying a `ruleGroup` field are the server evaluating its own
rules; a real read carries an `httpRequest` field instead. Sub-project B
must filter on `ruleGroup` and must not count rule evaluations as reads:
if it counts every log entry as a read, then every metric any rule
touches grades as "queried" -- which is circular, because the rules
already ARE its corpus.

The log also grows fast, on the order of half a megabyte in the first few
minutes at this stack's 15s evaluation interval, so B should expect a
large file from a long-running stack and read it as a stream rather than
all at once.
