package corpus

import "regexp"

// Grafana template variables are not valid PromQL, but only some of them
// stop a query parsing.
//
// A variable inside a label value -- node_load1{instance="$node"} -- is
// already legal PromQL: it is just a string. A variable inside a DURATION --
// rate(x[$__rate_interval]) -- is not, because the grammar wants a duration
// literal there.
//
// That distinction is not academic. Measured against Grafana dashboard 1860,
// the most-downloaded dashboard there is: of its 284 panel queries, all 284
// contain a "$", 160 parse anyway, and the 124 that fail ALL call
// rate()/increase()/delta() while none of the 160 does. So a corpus reader
// that simply refuses what it cannot parse does not refuse everything -- it
// silently keeps the non-rate half and drops the rest, which is a partial,
// biased corpus that looks like it is working. That is worse than refusing
// outright, because nothing announces it.
var (
	// A variable occupying a whole duration slot.
	durationVar = regexp.MustCompile(`\[\s*\$\{?[A-Za-z_][A-Za-z0-9_]*\}?\s*\]`)
	// Grafana's built-in interval and range variables, which can also appear
	// outside brackets.
	intervalVar = regexp.MustCompile(`\$__(interval_ms|interval|rate_interval|range_s|range)`)
)

// Substitute rewrites a Grafana panel query into something the PromQL parser
// accepts, so its metric names can be extracted.
//
// It exists for parsing only. jetsam never evaluates these queries, and
// replacing a duration cannot change which metrics a query names -- which is
// the whole safety argument for doing this at all rather than refusing.
// Substitution makes a query parseable; it does not make it accurate, and it
// does not need to.
func Substitute(query string) string {
	q := durationVar.ReplaceAllString(query, "[5m]")
	return intervalVar.ReplaceAllString(q, "5m")
}
