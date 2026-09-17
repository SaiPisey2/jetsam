#!/usr/bin/env bash
# Block until the stack can actually be measured, not merely until its
# containers are running.
#
# `docker compose up -d` returns as soon as the processes start, but the
# suite reads scraped series, TSDB head stats and recording-rule output --
# none of which exist for the first scrape interval or two. Without this
# gate `make demo-up && make demo-test` is a race that a fast machine or a
# warm image cache loses, and CI runs exactly that pair back to back.
set -euo pipefail

PROM=${PROM:-http://localhost:9090}
DEADLINE=$((SECONDS + 180))
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# ask runs a PromQL query and prints the scalar result, or nothing.
ask() {
  curl -sG --data-urlencode "query=$1" "$PROM/api/v1/query" |
    python3 -c 'import sys,json
try:
    r = json.load(sys.stdin)["data"]["result"]
except Exception:
    r = []
print(r[0]["value"][1] if r else "")' 2>/dev/null || true
}

# wait_for polls until the given command succeeds, or gives up loudly. A
# silent timeout here would just move the confusing failure downstream.
wait_for() {
  local what=$1
  shift
  until "$@"; do
    if [ "$SECONDS" -ge "$DEADLINE" ]; then
      echo "demo/ready.sh: timed out waiting for $what" >&2
      exit 1
    fi
    sleep 2
  done
  echo "ready: $what"
}

targets_up() {
  [ "$(curl -s "$PROM/api/v1/targets?state=active" |
    python3 -c 'import sys,json
try:
    t = json.load(sys.stdin)["data"]["activeTargets"]
except Exception:
    t = []
print(sum(1 for x in t if x["health"] == "up"))' 2>/dev/null || echo 0)" = "3" ]
}

loadgen_scraped() {
  [ "$(ask 'count(jetsam_demo_requests_total)')" = "400" ]
}

# Every recording rule, not a sample of them: the job names in
# prometheus.yml have to match the vendored rules' selectors, and a
# mismatch shows up as recording rules that evaluate "ok" and write
# nothing at all. Checking one rule would not catch a partial mismatch.
recording_rules_produce_series() {
  local name
  while read -r name; do
    [ -n "$name" ] || continue
    [ -n "$(ask "count($name)")" ] || return 1
  done < <(sed 's/.*record: *//;t;d' "$HERE"/prometheus/rules/*.yaml | tr -d '"')
}

# The inventory comes from the TSDB status endpoint, not from a query, so
# gate on that separately rather than assuming the two agree.
inventory_has_rule_output() {
  curl -s "$PROM/api/v1/status/tsdb?limit=5000" |
    grep -q 'instance:node_cpu_utilisation:rate5m'
}

wait_for "all three scrape targets to be up" targets_up
wait_for "loadgen's 400 series to be scraped" loadgen_scraped
wait_for "every recording rule to produce series" recording_rules_produce_series
wait_for "the rule output to reach the TSDB status endpoint" inventory_has_rule_output
