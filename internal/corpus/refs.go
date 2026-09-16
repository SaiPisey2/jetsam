// Package corpus collects every query anything is known to run against a
// Prometheus, and reports which metrics those queries touch.
package corpus

import (
	"fmt"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// Refs is what one query touches.
type Refs struct {
	// Names is every metric named literally, deduplicated and sorted.
	Names []string
	// NameMatchers holds every non-equality __name__ matcher, to be
	// resolved against a real metric list by Resolve.
	NameMatchers []*labels.Matcher
	// MatchesEverything is true when some selector carries no __name__
	// constraint at all -- `{job="api"}` selects every metric that job
	// exposes, so the query touches all of them.
	MatchesEverything bool
}

// promParser is the PromQL parser used by Extract. Prometheus v0.314.0
// exposes parsing through a Parser value (created via NewParser) rather
// than the free-standing parser.ParseExpr function; a single stateless
// instance is reused across calls.
var promParser = parser.NewParser(parser.Options{})

// Extract parses query and reports what it touches. An unparseable query
// is an error, never an empty Refs: "this query references nothing" and
// "jetsam could not read this query" must not be the same answer, because
// the first licenses a drop and the second must forbid one.
func Extract(query string) (Refs, error) {
	expr, err := promParser.ParseExpr(query)
	if err != nil {
		return Refs{}, fmt.Errorf("parse query: %w", err)
	}

	var r Refs
	seen := map[string]bool{}
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		if vs.Name != "" {
			if !seen[vs.Name] {
				seen[vs.Name] = true
				r.Names = append(r.Names, vs.Name)
			}
			return nil
		}
		// No bare name: look for a __name__ matcher instead.
		named := false
		for _, m := range vs.LabelMatchers {
			if m.Name != labels.MetricName {
				continue
			}
			named = true
			if m.Type == labels.MatchEqual {
				if !seen[m.Value] {
					seen[m.Value] = true
					r.Names = append(r.Names, m.Value)
				}
				continue
			}
			r.NameMatchers = append(r.NameMatchers, m)
		}
		if !named {
			r.MatchesEverything = true
		}
		return nil
	})
	sort.Strings(r.Names)
	return r, nil
}

// Resolve returns every metric in all that this query touches: the ones it
// names literally, plus everything its __name__ matchers select, plus all
// of them when it carries a bare selector.
func (r Refs) Resolve(all []string) []string {
	if r.MatchesEverything {
		out := append([]string(nil), all...)
		sort.Strings(out)
		return out
	}
	hit := map[string]bool{}
	for _, n := range r.Names {
		hit[n] = true
	}
	for _, m := range r.NameMatchers {
		for _, name := range all {
			if m.Matches(name) {
				hit[name] = true
			}
		}
	}
	out := make([]string, 0, len(hit))
	for n := range hit {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
