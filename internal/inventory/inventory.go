// Package inventory is the cost half of jetsam's input: what exists and
// what it costs to store.
package inventory

import "sort"

// Metric is one metric name and the number of series carrying it.
type Metric struct {
	Name   string
	Series int
}

// Inventory is every metric in a Prometheus, largest first.
type Inventory struct {
	Metrics     []Metric
	TotalSeries int
}

// Build turns the TSDB status endpoint's counts into an Inventory ordered
// by series descending, ties broken by name so output is stable between
// runs -- an unstable order makes every PR diff look different even when
// nothing changed.
func Build(counts map[string]int) Inventory {
	inv := Inventory{Metrics: make([]Metric, 0, len(counts))}
	for name, n := range counts {
		inv.Metrics = append(inv.Metrics, Metric{Name: name, Series: n})
		inv.TotalSeries += n
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
