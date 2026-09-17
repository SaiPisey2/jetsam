#!/usr/bin/env bash
# Records the README demo into .github/assets/demo.gif: a title card and a
# terminal recording per step, cross faded into one GIF.
#
#   bash demo/record.sh
#
# Needs asciinema, agg, ffmpeg and Chrome. No local stack: every step runs
# against https://prometheus.demo.prometheus.io, which is public,
# read-only and needs no credentials -- so the numbers in the GIF are real
# and anyone can reproduce them.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="${JETSAM_DEMO_DIR:-${TMPDIR:-/tmp}/jetsam-demo}"
out="$root/.github/assets/demo.gif"
chrome="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
promurl="${JETSAM_DEMO_PROM:-https://prometheus.demo.prometheus.io}"

width=1197  # matches a 140x36 terminal at the font size agg is given below
height=725
fps=15
# Cross fades are what the file size is made of: every blended frame is a
# full-frame update, so 0.35s here costs several MB against ~1MB at 0.15s.
xfade=0.15

for tool in asciinema agg ffmpeg ffprobe; do
	command -v "$tool" >/dev/null || { echo "$tool not installed: brew install asciinema agg ffmpeg" >&2; exit 1; }
done
[ -x "$chrome" ] || { echo "Chrome not found; set CHROME=/path/to/chrome" >&2; exit 1; }
curl -sf "$promurl/api/v1/status/tsdb?limit=1" >/dev/null || {
	echo "cannot reach $promurl" >&2; exit 1; }

CGO_ENABLED=0 go build -o "$root/jetsam" ./cmd/jetsam

rm -rf "$work"
mkdir -p "$work/shots"
cp "$root/demo/demo.sh" "$work/"

# The scrape config propose edits. Every job_name here is one the public
# demo really exposes, so a drop lands in the job that actually produces
# the metric. Written without blank lines between entries because yaml.v3
# does not preserve them, and a diff whose every other line is a removed
# blank line hides the change being demonstrated.
cat >"$work/prometheus.yml" <<'YML'
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: prometheus
    static_configs:
      - targets: ['demo.promlabs.com:9090']
  - job_name: node
    static_configs:
      - targets: ['demo.promlabs.com:9100']
  - job_name: cadvisor
    static_configs:
      - targets: ['demo.promlabs.com:8080']
  - job_name: grafana
    static_configs:
      - targets: ['demo.promlabs.com:3000']
  - job_name: caddy
    static_configs:
      - targets: ['demo.promlabs.com:2019']
  - job_name: alertmanager
    static_configs:
      - targets: ['demo.promlabs.com:9093']
  - job_name: blackbox
    static_configs:
      - targets: ['demo.promlabs.com:9115']
  - job_name: random
    static_configs:
      - targets: ['demo.promlabs.com:8081']
YML

# demo.sh runs `jetsam init` on camera and init refuses to overwrite, so
# the config cannot exist yet; this runs straight after it. It sets the
# three things a default config leaves blank or generic: which Prometheus,
# which scrape config propose edits, and how much of the inventory to
# grade.
#
# metric_limit is the top 40 metrics by series rather than the default
# 5000. That is a real config knob, not a trick -- and scan and the pull
# request body both print the truncation warning, on camera, saying so.
# The full run grades all 1374 and takes half a minute, most of it the
# per-candidate job lookups, with a diff several thousand lines long.
cat >"$work/configure.sh" <<SETUP
set -e
python3 - <<'PY'
import re
s = open('jetsam.yaml').read()
s = s.replace('url: http://localhost:9090', 'url: $promurl')
s = s.replace('metric_limit: 5000', 'metric_limit: 40')
s = s.replace('prometheus_file: ""', 'prometheus_file: ./prometheus.yml')
open('jetsam.yaml', 'w').write(s)
PY
grep -q 'prometheus_file: ./prometheus.yml' jetsam.yaml
SETUP

# The segment list the final GIF is assembled from, in order. Each entry is
# a source file and how long to hold it; terminal recordings hold
# themselves, so their duration is read back from the file.
srcs=() durs=()
segment() { srcs+=("$1"); durs+=("$2"); }

# --- title cards ------------------------------------------------------
# Rendered as a web page rather than drawn with ffmpeg: this ffmpeg has no
# drawtext filter, and Chrome renders text better anyway.
card() { # number title subtitle
	local file="$work/shots/card$1.png"
	cat >"$work/card.html" <<HTML
<!doctype html><meta charset="utf-8">
<style>
  html, body { margin: 0; height: 100%; }
  body {
    background: #171b21; color: #e6edf3;
    display: flex; align-items: center; justify-content: center;
    font-family: -apple-system, BlinkMacSystemFont, "Helvetica Neue", sans-serif;
  }
  .card { width: 760px; }
  .n { font-family: Menlo, monospace; font-size: 15px; letter-spacing: .2em;
       color: #6e7681; margin-bottom: 24px; }
  h1 { font-size: 44px; font-weight: 600; letter-spacing: -.01em; margin: 0 0 16px; }
  p  { font-size: 20px; color: #9198a1; margin: 0; }
</style>
<div class="card">
  <div class="n">$1 / 5</div>
  <h1>$2</h1>
  <p>$3</p>
</div>
HTML
	shot "file://$work/card.html" "$file"
	segment "$file" 1.7
}

shot() { # url destination
	rm -f "$2"
	"$chrome" --headless=new --disable-gpu --hide-scrollbars \
		--virtual-time-budget=3000 --screenshot="$2" --window-size="$width,$height" \
		--user-data-dir="$work/chrome/$(basename "$2" .png)" "$1" >/dev/null 2>&1 &
	local pid=$! i
	for ((i = 0; i < 60; i++)); do [ -s "$2" ] && break; sleep 0.5; done
	sleep 1
	kill "$pid" 2>/dev/null || true
	[ -s "$2" ] || { echo "screenshot failed: $1" >&2; exit 1; }
}

# --- terminal steps ---------------------------------------------------
step() { # name hold-seconds
	local name=$1
	local hold=$2
	local gif="$work/seg-$name.gif"
	(
		cd "$work"
		PATH="$root:$PATH" TERM=xterm-256color \
			asciinema rec --quiet --window-size 140x36 --command "bash demo.sh $name" "$name.cast"
	)
	# The pause demo.sh leaves at the end of a step produces no terminal
	# output, so the recording stops at the last line printed and the pause
	# is not in it. --last-frame-duration puts it back, which is also why
	# the hold is stated here rather than read off the recording.
	#
	# --idle-time-limit collapses the gap where jetsam is waiting on the
	# network. Nothing is printed during it, so nothing is hidden.
	agg --quiet --theme github-dark --font-size 14 --line-height 1.4 \
		--idle-time-limit 2 --last-frame-duration "$hold" --fps-cap "$fps" \
		"$work/$name.cast" "$gif"
	segment "$gif" "$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$gif")"
}

card 1 "Point it at Prometheus" "No agent, no sidecar, no month of waiting."
step init 2
(cd "$work" && bash configure.sh)

card 2 "Price every metric" "What it stores, and what anything actually reads."
step scan 8

card 3 "It proposes nothing" "No query log means no proof. That is the default."
step propose 9

card 4 "Unless you accept the risk" "One flag, and it writes the relabel rules."
step diff 10

card 5 "The pull request says what it cannot see" "Every refusal counted, and the one thing you cannot undo."
step body 11

# --- one GIF ----------------------------------------------------------
# Every segment is normalised to the same size and frame rate, then chained
# through xfade. Each xfade's offset is where the cross fade starts on the
# stream built so far: the running total minus one fade, because each fade
# overlaps the two segments it joins.
inputs=() graph=""
for i in "${!srcs[@]}"; do
	case "${srcs[i]}" in
	*.png) inputs+=(-loop 1 -t "${durs[i]}" -i "${srcs[i]}") ;;
	*) inputs+=(-i "${srcs[i]}") ;;
	esac
	graph+="[$i:v]fps=$fps,scale=$width:$height:force_original_aspect_ratio=decrease,pad=$width:$height:0:0:color=0x171b21,format=rgb24,setsar=1[s$i];"
done

prev="[s0]" run="${durs[0]}"
for i in "${!srcs[@]}"; do
	[ "$i" = 0 ] && continue
	offset=$(awk -v r="$run" -v x="$xfade" 'BEGIN{printf "%.3f", r - x}')
	graph+="$prev[s$i]xfade=transition=fade:duration=$xfade:offset=$offset[x$i];"
	prev="[x$i]"
	run=$(awk -v r="$run" -v d="${durs[i]}" -v x="$xfade" 'BEGIN{printf "%.3f", r + d - x}')
done
graph+="${prev}split[a][b];[a]palettegen=stats_mode=diff:max_colors=160[p];[b][p]paletteuse=dither=none"

mkdir -p "$(dirname "$out")"
ffmpeg -v error "${inputs[@]}" -filter_complex "$graph" -loop 0 -y "$out"

echo "wrote ${out#"$root"/} ($(du -h "$out" | cut -f1), $(
	ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$out" | cut -d. -f1)s, ${#srcs[@]} segments)"
