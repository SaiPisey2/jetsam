#!/usr/bin/env bash
# One step of the README demo. Each step is recorded on its own so that
# demo/record.sh can put a title card between them; `bash demo.sh scan`
# also just runs the step in your own terminal.
#
# Every command is real and every number is whatever
# prometheus.demo.prometheus.io actually returns -- nothing is pre-baked.
# The pipes exist only to keep each step on one screen.
set -u

prompt='$ '

# Types a command out character by character, so the recording looks like
# someone using it rather than a wall of text appearing at once.
type_cmd() {
	printf '%s' "$prompt"
	local i
	for ((i = 0; i < ${#1}; i++)); do
		printf '%s' "${1:i:1}"
		sleep 0.03
	done
	printf '\n'
}

run() {
	type_cmd "$1"
	eval "$1"
}

clear
sleep 0.6

case "${1:?usage: demo.sh <step>}" in
init)
	run "jetsam init"
	sleep 2
	;;
# The config is shown here rather than in the init step because init
# writes the commented default -- localhost, no scrape config -- and
# record.sh fills those in straight afterwards. Showing the default and
# then scanning a remote Prometheus would be a continuity hole.
scan)
	run "grep -v -e '^ *#' -e '^\$' jetsam.yaml"
	run "jetsam scan | head -14"
	sleep 8
	;;
propose)
	run "jetsam propose"
	sleep 9
	;;
# The one run that proposes anything, saved so the next step can show the
# pull request body the same command produced rather than paying for a
# second thirty-second scan.
diff)
	run "jetsam propose -include-unreferenced > proposal.txt"
	run "sed -n '3,26p' proposal.txt"
	sleep 10
	;;
body)
	run "sed -n '/^## jetsam/,/^### job/p' proposal.txt | head -20"
	sleep 11
	;;
*)
	echo "unknown step: $1" >&2
	exit 1
	;;
esac
