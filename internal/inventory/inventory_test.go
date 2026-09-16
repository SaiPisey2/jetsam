package inventory

import (
	"testing"

	"github.com/SaiPisey2/jetsam/internal/promapi"
)

func TestBuildSortsBySeriesDescendingThenName(t *testing.T) {
	inv := Build(promapi.Status{
		Counts:     map[string]int{"b_metric": 10, "a_metric": 10, "big": 500},
		HeadSeries: 520,
	})
	want := []string{"big", "a_metric", "b_metric"}
	for i, w := range want {
		if inv.Metrics[i].Name != w {
			t.Errorf("Metrics[%d] = %q, want %q", i, inv.Metrics[i].Name, w)
		}
	}
	if inv.TotalSeries != 520 {
		t.Errorf("TotalSeries = %d, want 520", inv.TotalSeries)
	}
}

func TestShareIsZeroWhenNothingIsStored(t *testing.T) {
	// An empty Prometheus must not divide by zero; a percentage of nothing
	// is nothing, not NaN, and NaN would print as "NaN%" in a PR body.
	if got := Build(promapi.Status{}).Share(0); got != 0 {
		t.Errorf("Share = %v, want 0", got)
	}
}

// TestBuildUsesHeadSeriesNotSumOfCounts pins C2: TotalSeries must come from
// status.HeadSeries, never from summing status.Counts, because Counts is
// bounded by metric_limit and HeadSeries is not. A build that silently
// fell back to summing would understate the truth exactly when it matters
// most -- a truncated inventory.
func TestBuildUsesHeadSeriesNotSumOfCounts(t *testing.T) {
	inv := Build(promapi.Status{
		Counts:     map[string]int{"a": 10, "b": 20}, // sums to 30
		HeadSeries: 15777,                            // the true total, unrelated to the sum
	})
	if inv.TotalSeries != 15777 {
		t.Errorf("TotalSeries = %d, want 15777 (HeadSeries), not the sum of Counts (30)", inv.TotalSeries)
	}
}

func TestBuildCarriesTruncationThrough(t *testing.T) {
	inv := Build(promapi.Status{
		Counts:    map[string]int{"a": 10},
		Limit:     40,
		NameCount: 100,
		Truncated: true,
	})
	if !inv.Truncated {
		t.Error("Truncated = false, want true (carried from promapi.Status)")
	}
	if inv.MetricLimit != 40 {
		t.Errorf("MetricLimit = %d, want 40", inv.MetricLimit)
	}
	if inv.NameCount != 100 {
		t.Errorf("NameCount = %d, want 100", inv.NameCount)
	}
}

func TestBuildNotTruncatedWhenStatusIsNot(t *testing.T) {
	inv := Build(promapi.Status{
		Counts:    map[string]int{"a": 10},
		Truncated: false,
	})
	if inv.Truncated {
		t.Error("Truncated = true, want false")
	}
}
