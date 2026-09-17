#!/usr/bin/env bash
# Issues the fixture's known set of reads.
#
# Everything here lands in the query log, and nothing else does, so the
# log's contents are knowable by construction. The metrics read here are
# the ones the stack asserts grade as queried; every other metric in the
# stack is read by nobody, which is the archetype that matters.
set -euo pipefail

prom=${PROM_URL:-http://localhost:9090}

queries=(
	'sum by (mode) (rate(node_cpu_seconds_total[5m]))'
	'node_memory_MemAvailable_bytes'
	'sum by (path) (rate(jetsam_demo_requests_total[5m]))'
)

for q in "${queries[@]}"; do
	curl -sf --get "$prom/api/v1/query" --data-urlencode "query=$q" >/dev/null
done

echo "issued ${#queries[@]} queries"
