package report

import (
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

func TestScanStatesWhyNothingIsProposed(t *testing.T) {
	inv := inventory.Build(map[string]int{"unread": 400})
	c := corpus.Corpus{Queries: 5, Used: map[string]bool{}, Produced: map[string]bool{}}
	res := verdict.Compute(inv, c, false)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	// A report that shows an unread metric but never says why it is not
	// being proposed reads as a broken tool rather than an honest one.
	if !strings.Contains(out, "unreferenced") {
		t.Errorf("report does not grade the metric:\n%s", out)
	}
	if !strings.Contains(out, "query log") {
		t.Errorf("report does not explain that a query log is missing:\n%s", out)
	}
}
