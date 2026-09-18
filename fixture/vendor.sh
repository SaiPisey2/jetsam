#!/usr/bin/env bash
# Refetches every vendored fixture at the pin recorded in VENDOR.md.
#
# Vendored files are committed and never hand-edited. Bumping a pin means
# editing the versions below, re-running this, and reviewing the diff --
# which is the point: an upstream change to what counts as a "used" metric
# should be something a human looked at.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

KUBE_PROMETHEUS_TAG=v0.18.0
DASHBOARD_ID=1860
DASHBOARD_REVISION=45

mkdir -p prometheus/rules grafana/dashboards
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch_rules() { # manifest-name output-name
	local url="https://raw.githubusercontent.com/prometheus-operator/kube-prometheus/${KUBE_PROMETHEUS_TAG}/manifests/$1"
	echo "fetching $1"
	curl -fsSL "$url" -o "$tmp/$1"
	go run ./vendortool "$tmp/$1" "prometheus/rules/$2"
}

fetch_rules nodeExporter-prometheusRule.yaml node-exporter.yaml
fetch_rules prometheus-prometheusRule.yaml prometheus.yaml

echo "fetching dashboard ${DASHBOARD_ID} revision ${DASHBOARD_REVISION}"
curl -fsSL "https://grafana.com/api/dashboards/${DASHBOARD_ID}/revisions/${DASHBOARD_REVISION}/download" \
	-o grafana/dashboards/node-exporter-full.json

echo
echo "vendored."
echo "Run 'go test ./fixture/ -run TestVendored' to check them against VENDOR.md."
