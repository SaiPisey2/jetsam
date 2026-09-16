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

func TestScanNeverEmitsARawEscapeByte(t *testing.T) {
	// A metric name carrying an ANSI escape can clear or forge lines in a
	// terminal. The remote Prometheus is not trusted, so the raw ESC byte
	// (0x1b) must never reach the rendered report.
	const hostile = "up\x1b[2K\x1b[1;31mFAKE\x1b[0m"
	inv := inventory.Build(map[string]int{hostile: 400})
	c := corpus.Corpus{Queries: 0, Used: map[string]bool{}, Produced: map[string]bool{}}
	res := verdict.Compute(inv, c, false)

	var sb strings.Builder
	Scan(&sb, inv, c, res)
	out := sb.String()

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("report contains a raw ESC byte:\n%q", out)
	}
	if !strings.Contains(out, `\x1b`) {
		t.Errorf("report does not render the escape visibly:\n%s", out)
	}
}
