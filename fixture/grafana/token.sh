#!/usr/bin/env bash
# Creates a viewer-scoped service account token and writes it to .token.
#
# Grafana cannot provision a token from a file, so it is created through
# the API after startup using the bootstrap admin credentials. The token
# is what sub-project B authenticates its dashboard reads with; the file
# is gitignored.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
grafana=${GRAFANA_URL:-http://localhost:3000}
admin=${GRAFANA_ADMIN:-admin:admin}

healthy=
for _ in $(seq 1 60); do
	if curl -sf -u "$admin" "$grafana/api/health" >/dev/null; then
		healthy=1
		break
	fi
	sleep 1
done
[ -n "$healthy" ] || { echo "grafana at $grafana did not become healthy within 60s" >&2; exit 1; }

# Parsed with python3's json module rather than sed: a field-order or
# whitespace change in Grafana's response would silently break a sed
# extraction and produce a confusing 401 two steps later.
id=$(curl -sf -u "$admin" -X POST "$grafana/api/serviceaccounts" \
	-H 'Content-Type: application/json' \
	-d '{"name":"jetsam-demo","role":"Viewer","isDisabled":false}' |
	python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))' 2>/dev/null || true)

if [ -z "$id" ]; then
	# Already exists from a previous run: look it up rather than fail.
	id=$(curl -sf -u "$admin" "$grafana/api/serviceaccounts/search?query=jetsam-demo" |
		python3 -c 'import json,sys; a=json.load(sys.stdin).get("serviceAccounts",[]); print(a[0]["id"] if a else "")')
fi
[ -n "$id" ] || { echo "could not create or find the service account" >&2; exit 1; }

# Captured into a variable rather than redirected straight onto .token:
# a redirect truncates the file before the pipeline runs, so a failed
# request would abort the script (set -e) with a zero-byte .token already
# on disk and the -s guard below never reached.
key=$(curl -sf -u "$admin" -X POST "$grafana/api/serviceaccounts/$id/tokens" \
	-H 'Content-Type: application/json' \
	-d "{\"name\":\"jetsam-demo-$(date +%s)\"}" |
	python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])' 2>/dev/null) || {
	echo "could not create a service account token (grafana at $grafana rejected the request)" >&2
	exit 1
}
[ -n "$key" ] || { echo "token creation returned no key" >&2; exit 1; }
printf '%s\n' "$key" >.token

[ -s .token ] || { echo "token creation returned no key" >&2; exit 1; }
echo "wrote fixture/grafana/.token"
