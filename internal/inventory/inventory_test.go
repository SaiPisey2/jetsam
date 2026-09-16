package inventory

import "testing"

func TestBuildSortsBySeriesDescendingThenName(t *testing.T) {
	inv := Build(map[string]int{"b_metric": 10, "a_metric": 10, "big": 500})
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
	if got := Build(nil).Share(0); got != 0 {
		t.Errorf("Share = %v, want 0", got)
	}
}
