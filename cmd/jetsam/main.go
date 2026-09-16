// Command jetsam finds Prometheus metrics nothing reads and proposes
// dropping them.
package main

import (
	"fmt"
	"os"
)

const usage = `jetsam finds Prometheus metrics nothing reads.

usage:
  jetsam init  [-config jetsam.yaml]
  jetsam scan  [-config jetsam.yaml]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
