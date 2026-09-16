// Package report renders jetsam's findings for a terminal.
package report

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// Scan writes the scan report: a header, then every metric graded, largest
// first.
func Scan(w io.Writer, inv inventory.Inventory, c corpus.Corpus, res verdict.Result) {
	fmt.Fprintf(w, "Metrics    %d\n", len(inv.Metrics))
	fmt.Fprintf(w, "Series     %d\n", inv.TotalSeries)
	fmt.Fprintf(w, "Queries    %d read from rules\n", c.Queries)
	if res.DroppableSeries > 0 {
		fmt.Fprintf(w, "Droppable  %d series (%.1f%% of stored)\n",
			res.DroppableSeries, 100*inv.Share(res.DroppableSeries))
	} else {
		fmt.Fprintf(w, "Droppable  none -- no query log configured, so ad-hoc reads are invisible\n")
	}
	for _, b := range res.Blocked {
		fmt.Fprintf(w, "Blocked    %s\n", b)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIES\tSHARE\tGRADE\tDROP\tMETRIC\tWHY")
	for _, v := range res.Verdicts {
		drop := "no"
		if v.Droppable {
			drop = "yes"
		}
		fmt.Fprintf(tw, "%d\t%.1f%%\t%s\t%s\t%s\t%s\n",
			v.Series, 100*inv.Share(v.Series), v.Grade, drop, v.Metric, v.Reason)
	}
	tw.Flush()
}
