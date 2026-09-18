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

The counts quoted above (API-vs-rule-evaluation ratios, log growth rate)
remain unpinned observations, not assertions. `fixture/stack_test.go`'s
`TestQueryLogCapturesTheKnownQueries` still only checks that the three
known queries are present and logs the breakdown; nothing in the suite
fails if the ratio moves, because it is a property of how long the stack
has been running, not of the fixture's configuration.

### The live log is contaminated, and cannot stand in for "no evidence"

Grafana's own dashboard auto-refresh sends every one of a dashboard's
panel queries to Prometheus on its own schedule, and jetsam's own
integration tests query Prometheus too. Both land in
`fixture/querylog/queries.log` as ordinary `httpRequest` entries,
indistinguishable from a human's read. Measured against this fixture: a
corpus built from the live log alone reads roughly 650 distinct queries
and marks on the order of 600+ of the roughly 660 metrics in the
inventory as used -- effectively the whole surface, whether or not any
dashboard or rule ever named a given metric.

Two consequences follow, and both bit an earlier version of this test
suite:

- **The live log cannot be used to prove a metric's dashboard/rule
  evidence is what makes it `used`.** Almost everything is `used` via the
  query log regardless. A test that wants to isolate a single evidence
  source (dashboards, or the absence of one) has to either exclude
  `QueryLog` entirely or replace it with an explicit, short, hand-written
  `querylog.Reading{Queries: [...]}` -- see `liveSources`,
  `TestDashboardsAloneFlipTheDashboardCanaries`, and the
  `withholdingBaseline` helper in `fixture/verdict_test.go`.
- **The live log cannot be used to prove a withholding rule actually
  withholds anything.** With the live log wired in, essentially every
  metric is already `used`, so `Droppable` is `false` for nearly the
  whole inventory before any override is applied. A test asserting
  "nothing is droppable" against that baseline is true regardless of
  whether the code under test runs at all -- `TestAShortQueryLogLicensesNothing`
  and `TestUnreachableGrafanaWithholdsEveryDrop` originally asserted
  exactly this and both kept passing with the withholding branch in
  `internal/verdict.Compute` disabled outright. Both now build a
  restricted, explicit query log first to create a real droppable metric,
  then assert the override under test takes it away.

Whoever relies on this fixture's query log for anything beyond "the file
exists and contains recognizable entries" needs to know this before
trusting it as an independent evidence source in a test.

## The dashboard canary

`fixture/verdict_test.go`'s `grade()` now builds its corpus from all
three sources -- rules, dashboards (authenticated with the token
`fixture/grafana/token.sh` writes), and the query log -- mirroring
`cmd/jetsam/main.go`'s `gather`, instead of rules alone.

Two metrics were pinned as `unreferenced` specifically because v0.1's
corpus was rules only, and were documented as the canaries for the moment
dashboards entered the corpus. Both now grade `used`, in
`TestKnownArchetypesGradeCorrectly`:

| Metric | Was | Now | Why |
| --- | --- | --- | --- |
| `jetsam_demo_requests_total` | `unreferenced` | `used` | only a dashboard (`loadgen`) reads it |
| `node_scrape_collector_duration_seconds` | `unreferenced` | `used` | dashboard 1860 reads it, in one of the 160 panel queries that parse without substitution |

That flip is the acceptance test for this whole sub-project: a canary
that did not sing would mean the dashboards were not really in the
corpus, regardless of anything else passing. Because the live query log
also contains every dashboard's auto-refreshed queries (see above), this
flip alone does not prove the dashboard path specifically works --
`TestDashboardsAloneFlipTheDashboardCanaries` excludes the query log to
isolate it, and `TestRulesAloneLeaveTheDashboardCanariesUnreferenced`
pins the before-state with neither dashboards nor the query log, so the
flip is attributed to dashboards and nothing else.

A third canary, found by isolating the oracle rather than pinned in
advance: `node_uname_info` is named by no panel query at all, only by the
vendored dashboard's `job`/`nodename`/`node` template variables (all
`label_values(node_uname_info, ...)`). `TestDashboardVariablesAreUsedWithNoQueryLogAtAll`
asserts it grades `used` from a dashboards-only corpus with no query log
-- the real-world shape of a fresh install -- and is the assertion that
would have caught the gap described below before it shipped.

### Differential oracle against mimirtool

A differential oracle, `fixture/oracle_test.go`, checks jetsam's answer
against `mimirtool analyze grafana` + `analyze prometheus` -- an
independent implementation of the dashboard half of this question. It is
compared against a corpus built from `Dashboards` alone (not the full
`liveSources` corpus, for the same contamination reason described
above), and asserts jetsam's dashboard-only used-set is a superset of
mimirtool's. `mimirtool analyze ruler` does not work against a plain
Prometheus -- it calls a Mimir ruler API a plain Prometheus does not
serve -- so the oracle covers dashboards only.

Isolating the comparison this way first surfaced a real, narrow gap:
`internal/grafana.Client.Dashboards` collected panel target queries only,
not a dashboard's own template-variable definitions (`templating.list[]`,
Grafana's "job"/"nodename"/"instance" dropdowns). The vendored dashboard's
`job`, `nodename`, and `node` variables all query
`label_values(node_uname_info, ...)`, and `node_uname_info` appears
nowhere in any panel's target expression -- so a dashboards-only corpus
never marked it used, while mimirtool's dashboard analysis did. This was
a real safety gap, not a documentation boundary: on an install with
Grafana configured and no query log -- the ordinary starting
configuration -- `node_uname_info` graded `unreferenced`, and
`-include-unreferenced` would propose dropping a metric that three of the
dashboard's own variables resolve through, breaking every panel on it.
"The fixture's query log happens to also catch this read" was never a
defence, because a fresh install has no log at all.

**Fixed.** `grafana.Dashboard` now carries `VariableQueries []string`
alongside `Queries`, collected from `templating.list[].query` (handling
both shapes it takes across a real dashboard: a plain string for a
datasource variable, and `{"query": "...", "refId": "..."}` for a
Prometheus query variable). `corpus.Build` runs `VariableQueries` through
the same substitute-then-extract path as a panel query; `label_values()`
is a Grafana template function, not PromQL, so this reliably takes the
same conservative fallback an unparseable panel does, reported under
`Corpus.DashboardVariablesUnparsed` rather than
`DashboardPanelsUnparsed` so an operator does not read it as a broken
panel. On the vendored dashboard this adds exactly two names to the
dashboards-only used-set: `node_uname_info` (the fix), and a harmless
`prometheus` from the `ds_prometheus` datasource variable's plain-string
query -- which is not a real metric in this fixture's inventory, so it
never affects any verdict (`Refs.Resolve` already hits every literally-
named string in `r.Names` regardless of whether it exists in `allMetrics`;
this is pre-existing `internal/corpus` behavior, not something this fix
introduced). `TestKnownArchetypesGradeCorrectly`,
`TestDashboardVariablesAreUsedWithNoQueryLogAtAll` in
`fixture/verdict_test.go`, and equivalent unit tests in
`internal/grafana` and `internal/corpus` cover it. With the gap closed,
mimirtool and jetsam agree on the dashboard-only used-set with no
exception needed -- `fixture/oracle_test.go` no longer carries a named
allowance for this metric.
