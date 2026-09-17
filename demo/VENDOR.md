# Vendored fixtures

Everything here is fetched by `demo/vendor.sh` and committed. Nothing here
is hand-edited. These artifacts decide what the stack counts as a "used"
metric, and they were written by people with no knowledge of this project
— which is the only reason they are worth testing against.

Bumping a pin: edit the version in `demo/vendor.sh`, re-run it, then run
`go test ./demo/ -run TestVendored`. It will fail, telling you exactly
which counts moved. Update the numbers here and in `demo/vendor_test.go`
in the same commit, so the change is reviewable rather than silent.

## Prometheus rules

Source: `prometheus-operator/kube-prometheus`, `manifests/`
Pin: `v0.18.0`
Fetched: 2026-09-18
Transform: `demo/vendortool` lifts `spec.groups` out of the
`PrometheusRule` custom resource into a plain Prometheus rule file.

| File | Groups | Rules | Recording | Alerting |
| --- | --- | --- | --- | --- |
| `prometheus/rules/node-exporter.yaml` | 2 | 41 | 15 | 26 |
| `prometheus/rules/prometheus.yaml` | 1 | 23 | 0 | 23 |
| **total** | **3** | **64** | **15** | **49** |

## Grafana dashboard

Source: https://grafana.com/api/dashboards/1860 — "Node Exporter Full"
Pin: revision `45`
Fetched: 2026-09-18
Transform: none.

| Property | Value |
| --- | --- |
| Panel queries | 284 |
| Panel queries containing a template variable | 284 (all of them) |

That second row is the fixture's most important property. A real
dashboard's queries are not valid PromQL until template variables are
substituted, so a corpus reader that simply refuses what it cannot parse
would refuse everything, permanently. Sub-project B has to substitute.

## Container images

Pinned by digest in `demo/docker-compose.yml`.

| Image | Version | Digest |
| --- | --- | --- |
| `prom/prometheus` | v3.13.0 | `sha256:c6b27ea434f8389bfe233fbc7be381cf50587c286e871bc842008f5a1b1908a7` |
| `prom/node-exporter` | v1.10.0 | `sha256:dbb30baa213b9a8c9aff7fe00c124ec371752c92215f014ec6f26159e9c55ff3` |
| `grafana/grafana` | 12.3.0 | `sha256:70d9599b186ce287be0d2c5ba9a78acb2e86c1a68c9c41449454d0fc3eeb84e8` |
