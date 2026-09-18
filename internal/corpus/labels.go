package corpus

import (
	"fmt"
	"sort"

	"github.com/prometheus/prometheus/promql/parser"
)

// LabelNeed is what one query needs from one metric.
//
// All is the refusal case and it is the default on any doubt: it means the
// query needs every label the metric carries, so no label can be aggregated
// away without changing what that query returns.
type LabelNeed struct {
	All      bool
	Required []string
	// Op is the aggregation applied directly over the metric, if any, and
	// OpSafe is whether that operator survives partial aggregation.
	Op     string
	OpSafe bool
}

// composes lists the aggregation operators that are idempotent under partial
// aggregation: sum by (X) over sum by (X+Y) equals sum by (X) over the raw
// series, and likewise for min and max.
//
// count is deliberately absent although it looks composable. count by (X)
// over series already collapsed to X+Y counts the COLLAPSED series, not the
// original ones -- a different number, silently. It is rescuable by recording
// count and rewriting the consumer to sum, which is a real but separate case
// and is not implemented here. avg, quantile, stddev and stdvar need more
// state than the aggregated series carries.
var composes = map[parser.ItemType]bool{
	parser.SUM: true,
	parser.MIN: true,
	parser.MAX: true,
}

// preservesEverySeries lists the aggregators that return their INPUT series
// rather than a collapsed one.
//
// This is the trap the whole file exists for. sum(m) and topk(5, m) both
// parse to an AggregateExpr with an empty Grouping and Without false -- they
// are indistinguishable by shape. But sum collapses to nothing while topk
// hands back the original series with every label intact. Reading the
// grouping alone would silently strip labels a dashboard displays.
// Op.IsAggregatorWithParam() does not help either: it is true for quantile,
// which collapses, as well as for topk, which does not.
var preservesEverySeries = map[parser.ItemType]bool{
	parser.TOPK:    true,
	parser.BOTTOMK: true,
}

// LabelsNeeded reports what query needs from metric.
//
// An unparseable query is an error, never an empty need: "this query needs no
// labels" and "jetsam could not read this query" must not be the same answer,
// because the first licenses removing every label.
func LabelsNeeded(query, metric string) (LabelNeed, error) {
	expr, err := promParser.ParseExpr(query)
	if err != nil {
		return LabelNeed{}, fmt.Errorf("parse query: %w", err)
	}

	var need LabelNeed
	req := map[string]bool{}
	touches := false

	parser.Inspect(expr, func(n parser.Node, path []parser.Node) error {
		switch v := n.(type) {
		case *parser.VectorSelector:
			if !selectorTouches(v, metric) {
				return nil
			}
			touches = true
			// Every matcher on the selector is a label the query depends on.
			for _, m := range v.LabelMatchers {
				if m.Name != "__name__" {
					req[m.Name] = true
				}
			}
			agg := enclosingAggregate(path)
			if agg == nil {
				// Not aggregated: the query returns these series as they
				// are, so every label is part of the answer.
				need.All = true
				return nil
			}
			need.Op = agg.Op.String()
			need.OpSafe = composes[agg.Op]
			if preservesEverySeries[agg.Op] {
				need.All = true
				return nil
			}
			if agg.Without {
				// `without (pod)` keeps every OTHER label, so everything but
				// the named ones is required -- which, without knowing the
				// full label set here, is every label.
				need.All = true
				return nil
			}
			for _, g := range agg.Grouping {
				req[g] = true
			}

		case *parser.BinaryExpr:
			if v.VectorMatching == nil {
				return nil
			}
			for _, l := range v.VectorMatching.MatchingLabels {
				req[l] = true
			}
			for _, l := range v.VectorMatching.Include {
				req[l] = true
			}

		case *parser.Call:
			// label_replace and label_join name their source labels as
			// string ARGUMENTS. A walk that only inspects selectors and
			// groupings never sees them.
			switch v.Func.Name {
			case "label_replace":
				if len(v.Args) >= 4 {
					if s, ok := v.Args[3].(*parser.StringLiteral); ok {
						req[s.Val] = true
					}
				}
			case "label_join":
				for i := 3; i < len(v.Args); i++ {
					if s, ok := v.Args[i].(*parser.StringLiteral); ok {
						req[s.Val] = true
					}
				}
			}
		}
		return nil
	})

	if !touches {
		return LabelNeed{}, nil
	}
	if need.All {
		return need, nil
	}
	var out []string
	for l := range req {
		out = append(out, l)
	}
	sort.Strings(out)
	need.Required = out
	return need, nil
}

// selectorTouches reports whether this selector can select metric.
func selectorTouches(v *parser.VectorSelector, metric string) bool {
	if v.Name == metric {
		return true
	}
	for _, m := range v.LabelMatchers {
		if m.Name == "__name__" && m.Matches(metric) {
			return true
		}
	}
	return false
}

// enclosingAggregate returns the nearest AggregateExpr above this node, or
// nil when the selector is not aggregated at all.
func enclosingAggregate(path []parser.Node) *parser.AggregateExpr {
	for i := len(path) - 1; i >= 0; i-- {
		if a, ok := path[i].(*parser.AggregateExpr); ok {
			return a
		}
	}
	return nil
}
