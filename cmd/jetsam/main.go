// Command jetsam finds Prometheus metrics nothing reads and proposes
// dropping them.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/SaiPisey2/jetsam/internal/config"
	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/report"
	"github.com/SaiPisey2/jetsam/internal/verdict"
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
	case "init":
		runInit(os.Args[2:])
	case "scan":
		runScan(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "jetsam.yaml", "config file")
	fs.Parse(args)
	if err := config.WriteDefault(*path); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s\n", *path)
}

func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	path := fs.String("config", "jetsam.yaml", "config file")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Prometheus.Timeout)
	defer cancel()

	cl := promapi.New(cfg.Prometheus.URL, cfg.Prometheus.Timeout)
	counts, err := cl.TSDBStatus(ctx, cfg.Prometheus.MetricLimit)
	if err != nil {
		fail(err)
	}
	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		fail(err)
	}

	inv := inventory.Build(counts)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	cor := corpus.Build(rules, names)
	// v0.1 never reads a query log, so the evidence for "nobody queried
	// this" does not exist yet; cfg.HaveQueryLog() only reports that a
	// path is configured in YAML, not that anything was read from it.
	// Wiring the configured path to Compute here would let one line of
	// YAML mark metrics droppable on rule evidence alone. This parameter
	// becomes real in v0.3 when log ingestion lands.
	report.Scan(os.Stdout, inv, cor, verdict.Compute(inv, cor, false))
}
