// Package inventory is the cost half of jetsam's input: what exists and
// what it costs to store.
package inventory

import (
	"sort"

	"github.com/SaiPisey2/jetsam/internal/promapi"
)

// Metric is one metric name and the number of series carrying it.
type Metric struct {
	Name   string
	Series int
}

// Inventory is every metric in a Prometheus, largest first.
type Inventory struct {
	Metrics []Metric
	// TotalSeries is the Prometheus's true total series count
	// (promapi.Status.HeadSeries), NOT the sum of Metrics' Series. Summing
	// Metrics undercounts silently whenever metric_limit truncated the
	// list this Inventory was built from -- Metrics then holds only the
	// top MetricLimit names, but TotalSeries still reports the truth.
	TotalSeries int
	// MetricLimit is the tsdb status limit this Inventory was built with.
	MetricLimit int
	// NameCount is the true number of distinct metric names this
	// Prometheus holds, independent of MetricLimit.
	NameCount int
	// Truncated is true when metrics outside the top MetricLimit by
	// series count exist and were never graded -- see promapi.Status.
	Truncated bool
}

// Build turns the TSDB status endpoint's response into an Inventory
// ordered by series descending, ties broken by name so output is stable
// between runs -- an unstable order makes every PR diff look different
// even when nothing changed.
//
// TotalSeries is taken from status.HeadSeries, not summed from
// status.Counts: see the Inventory.TotalSeries doc comment for why the sum
// is the wrong number whenever status.Truncated is true.
func Build(status promapi.Status) Inventory {
	inv := Inventory{
		Metrics:     make([]Metric, 0, len(status.Counts)),
		TotalSeries: status.HeadSeries,
		MetricLimit: status.Limit,
		NameCount:   status.NameCount,
		Truncated:   status.Truncated,
	}
	for name, n := range status.Counts {
		inv.Metrics = append(inv.Metrics, Metric{Name: name, Series: n})
	}
	sort.Slice(inv.Metrics, func(a, b int) bool {
		if inv.Metrics[a].Series != inv.Metrics[b].Series {
			return inv.Metrics[a].Series > inv.Metrics[b].Series
		}
		return inv.Metrics[a].Name < inv.Metrics[b].Name
	})
	return inv
}

// Share returns series as a fraction of everything stored, or 0 when
// nothing is.
func (i Inventory) Share(series int) float64 {
	if i.TotalSeries == 0 {
		return 0
	}
	return float64(series) / float64(i.TotalSeries)
}
