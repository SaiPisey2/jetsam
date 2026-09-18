#!/usr/bin/env bash
# Block until the stack can actually be measured, not merely until its
# containers are running.
#
# `docker compose up -d` returns as soon as the processes start, but the
# suite reads scraped series, TSDB head stats and recording-rule output --
# none of which exist for the first scrape interval or two. Without this
# gate `make demo-up && make demo-test` is a race that a fast machine or a
# warm image cache loses, and CI runs exactly that pair back to back.
#
# Every poll below reads /api/v1/targets, /api/v1/series or
# /api/v1/status/tsdb -- never /api/v1/query -- because Prometheus writes
# every /api/v1/query (and /api/v1/query_range) call to the query log.
# fixture/query.sh is meant to be the only source of the log's httpRequest
# entries (see its header comment and fixture/VENDOR.md's query-log
# section); a gate that polls with count(...) queries would silently add
# dozens of its own reads to that log, including reads of
# instance:node_cpu_utilisation:rate5m, the one metric fixture/verdict_test.go
# pins as read by nothing. /api/v1/series?match[]=<selector> reports
# whether/how many series exist for a selector without executing PromQL,
# so it never touches the log.
set -euo pipefail

PROM=${PROM:-http://localhost:9090}
DEADLINE=$((SECONDS + 180))
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# wait_for polls until the given command succeeds, or gives up loudly. A
# silent timeout here would just move the confusing failure downstream.
# If the predicate is recording_rules_produce_series, it also names the
# specific rule that never produced anything, via LAST_MISSING_RULE.
wait_for() {
  local what=$1
  shift
  until "$@"; do
    if [ "$SECONDS" -ge "$DEADLINE" ]; then
      echo "fixture/ready.sh: timed out waiting for $what" >&2
      if [ -n "${LAST_MISSING_RULE:-}" ]; then
        echo "fixture/ready.sh: recording rule producing no series: $LAST_MISSING_RULE" >&2
      fi
      exit 1
    fi
    sleep 2
  done
  echo "ready: $what"
}

# series_count prints how many series match the given selector via
# /api/v1/series, or nothing on any failure. This never lands in the
# query log: /api/v1/series does not execute a PromQL query.
series_count() {
  curl -s --max-time 5 -G --data-urlencode "match[]=$1" "$PROM/api/v1/series" 2>/dev/null |
    python3 -c 'import sys,json
try:
    d = json.load(sys.stdin)["data"]
except Exception:
    d = []
print(len(d))' 2>/dev/null
}

targets_up() {
  # 3 is the number of scrape jobs fixture/stack_test.go's wantJobs pins
  # (prometheus-k8s, node-exporter, loadgen); that list is authoritative.
  [ "$(curl -s --max-time 5 "$PROM/api/v1/targets?state=active" |
    python3 -c 'import sys,json
try:
    t = json.load(sys.stdin)["data"]["activeTargets"]
except Exception:
    t = []
print(sum(1 for x in t if x["health"] == "up"))' 2>/dev/null || echo 0)" = "3" ]
}

loadgen_scraped() {
  # 400 is the exact series count fixture/stack_test.go's
  # TestLoadgenExposesItsExactCardinality asserts against this same stack.
  [ "$(series_count 'jetsam_demo_requests_total')" = "400" ]
}

# Recording rule names, extracted portably: `awk` behaves the same under
# BSD (macOS, /usr/bin/awk) and GNU, unlike the GNU-sed-only
# `s/.*record: *//;t;d` this replaces, which BSD sed rejects outright
# with "undefined label ';d'". That failure used to sit inside a process
# substitution where neither `set -e` nor `pipefail` could see it, so the
# loop below silently got zero names and the gate passed having checked
# nothing. Read the names into an array up front and assert the count
# before ever polling Prometheus with them, so a broken extraction -- or
# a change to the vendored rules -- is a loud failure here, not a no-op.
RULE_NAMES=()
while IFS= read -r name; do
  [ -n "$name" ] && RULE_NAMES+=("$name")
done < <(awk '/record:/ { sub(/.*record: */, ""); print }' "$HERE"/prometheus/rules/*.yaml | tr -d '"')

if [ "${#RULE_NAMES[@]}" -eq 0 ]; then
  echo "fixture/ready.sh: extracted zero recording-rule names from $HERE/prometheus/rules/*.yaml -- extraction is broken, not confirming an empty stack" >&2
  exit 1
fi

# 15 is fixture/vendor_test.go's wantRecording constant (see fixture/VENDOR.md);
# a change to the vendored rule count should fail here too, not only in
# `go test -run TestVendored`.
if [ "${#RULE_NAMES[@]}" -ne 15 ]; then
  echo "fixture/ready.sh: extracted ${#RULE_NAMES[@]} recording-rule names, want exactly 15 (fixture/vendor_test.go's wantRecording)" >&2
  exit 1
fi

# Every recording rule, not a sample of them: the job names in
# prometheus.yml have to match the vendored rules' selectors, and a
# mismatch shows up as recording rules that evaluate "ok" and write
# nothing at all. Checking one rule would not catch a partial mismatch.
# On timeout, LAST_MISSING_RULE names the one that produced nothing.
recording_rules_produce_series() {
  LAST_MISSING_RULE=""
  local name n
  for name in "${RULE_NAMES[@]}"; do
    n=$(series_count "$name")
    if [ -z "$n" ] || [ "$n" = "0" ]; then
      LAST_MISSING_RULE=$name
      return 1
    fi
  done
  return 0
}

# The inventory comes from the TSDB status endpoint, not from a query, so
# gate on that separately rather than assuming the two agree.
#
# instance:node_cpu_utilisation:rate5m is the metric fixture/verdict_test.go
# pins as written by a recording rule and read by nothing (the canary for
# the Produced protection); keep this name in sync with that file.
inventory_has_rule_output() {
  curl -s --max-time 5 "$PROM/api/v1/status/tsdb?limit=5000" |
    grep -q 'instance:node_cpu_utilisation:rate5m'
}

wait_for "all three scrape targets to be up" targets_up
wait_for "loadgen's 400 series to be scraped" loadgen_scraped
wait_for "every recording rule to produce series" recording_rules_produce_series
wait_for "the rule output to reach the TSDB status endpoint" inventory_has_rule_output
