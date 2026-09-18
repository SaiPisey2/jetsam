package emit

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/SaiPisey2/jetsam/internal/aggregate"
	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/yaml.v3"
)

// ruleParser is the PromQL parser RenderRule validates its own output
// against, kept as one shared instance the same way corpus keeps promParser
// -- there is nothing per-call about how it is configured.
var ruleParser = parser.NewParser(parser.Options{})

// RenderRule returns ruleYAML with p's recording rule added to a group,
// building the file's groups: structure from nothing when ruleYAML is
// empty (or has none).
//
// The expression is built from p's own fields -- never by pattern-matching
// a consumer's query text -- so p.Fn is what decides whether the rule
// records a rate or the metric itself: see Proposal's doc comment for why
// summing a counter and rating the sum afterward is a different, wrong
// number from summing the rates directly. p.Keep empty is a real case too,
// not an oversight to guard against: it means every consumer collapses the
// metric to one series, and the grouping clause is omitted entirely rather
// than rendered as the meaningless "sum by () (...)".
//
// Two different things can make this rule unsafe to write, and both are
// checked before anything touches the tree:
//
//  1. p.Metric and p.RuleName are untrusted the same way a scraped metric
//     name is untrusted anywhere else in this package -- see relabel.go.
//     Neither is ever interpolated into YAML source and re-parsed, only
//     set as a yaml.Node's Value, so a newline or a quote in either cannot
//     inject YAML structure.
//  2. That is not enough here, because the scalar being written is itself
//     PromQL, not a passive string like a job name. A metric name carrying
//     a quote or a bracket is a perfectly quotable YAML scalar and an
//     unparseable expr at the same time -- and Prometheus refuses to load
//     a whole rule file over one bad expr in it, so every OTHER rule
//     already there stops evaluating because of a name jetsam wrote. The
//     rendered expression is therefore parsed with the same PromQL parser
//     the corpus uses before it is written, and RuleName is checked the
//     same way: a valid record name is exactly a string that reads back as
//     a bare selector naming itself.
//
// Refuses, rather than overwrites, when a rule already exists under
// p.RuleName anywhere in the file: a same-named rule could be something an
// operator wrote by hand, or something an earlier jetsam run recorded from
// different inputs, and either way silently replacing it is worse than
// declining the change.
func RenderRule(ruleYAML string, p aggregate.Proposal) (string, error) {
	if !validMetricName(p.RuleName) {
		return "", fmt.Errorf("record name %q is not a valid metric name", p.RuleName)
	}
	expr := renderExpr(p)
	if _, err := ruleParser.ParseExpr(expr); err != nil {
		return "", fmt.Errorf("rendered expr %q does not parse as PromQL: %w", expr, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(ruleYAML), &doc); err != nil {
		return "", fmt.Errorf("parse rule file: %w", err)
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", fmt.Errorf("rule file's top level is not a mapping")
	}

	groups := mappingValue(root, "groups")
	if groups == nil {
		root.Content = append(root.Content,
			strScalar("groups"), &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		groups = root.Content[len(root.Content)-1]
	}

	for _, g := range groups.Content {
		rules := mappingValue(g, "rules")
		if rules == nil {
			continue
		}
		for _, r := range rules.Content {
			if scalarValue(mappingValue(r, "record")) == p.RuleName {
				return "", fmt.Errorf("a rule named %q already exists", p.RuleName)
			}
		}
	}

	rules := groupRules(groups)
	rules.Content = append(rules.Content, &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			strScalar("record"), strScalar(p.RuleName),
			strScalar("expr"), strScalar(expr),
		},
	})

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", fmt.Errorf("render rule file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("render rule file: %w", err)
	}
	return buf.String(), nil
}

// groupRules returns the rules sequence RenderRule appends its new rule to,
// creating whatever is missing to get there.
//
// The FIRST existing group is used rather than always inventing a new one:
// the aggregate command gives an operator a single configured rule file
// specifically so every rule jetsam adds lands together, and starting a
// second group on every run would defeat that the moment the file already
// holds one. "jetsam" only names a group this function itself has to
// invent, for a file that started with none at all.
func groupRules(groups *yaml.Node) *yaml.Node {
	var group *yaml.Node
	if len(groups.Content) > 0 {
		group = groups.Content[0]
	} else {
		group = &yaml.Node{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
			Content: []*yaml.Node{
				strScalar("name"), strScalar("jetsam"),
				strScalar("rules"), {Kind: yaml.SequenceNode, Tag: "!!seq"},
			},
		}
		groups.Content = append(groups.Content, group)
	}
	rules := mappingValue(group, "rules")
	if rules == nil {
		group.Content = append(group.Content,
			strScalar("rules"), &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		rules = group.Content[len(group.Content)-1]
	}
	return rules
}

// renderExpr builds p's PromQL expression from its fields alone. See
// RenderRule's doc comment for why that discipline matters here
// specifically -- the rate vs. the counter -- and not only for the general
// reason every other Render in this package avoids string interpolation of
// untrusted text.
func renderExpr(p aggregate.Proposal) string {
	inner := p.Metric
	if p.Fn != "" {
		inner = fmt.Sprintf("%s(%s[%s])", p.Fn, p.Metric, p.Window)
	}
	if len(p.Keep) == 0 {
		return fmt.Sprintf("%s(%s)", p.Op, inner)
	}
	return fmt.Sprintf("%s by (%s) (%s)", p.Op, strings.Join(p.Keep, ", "), inner)
}

// validMetricName reports whether name is safe to write as a recording
// rule's record field. Parsed back through the same parser Prometheus
// itself uses, a valid one reads as a bare selector naming itself -- a
// quote or a newline breaks the parse outright, and a name that instead
// parses into something else entirely (a brace-qualified selector, a
// selector with an offset) does not satisfy Name == name, so either way it
// is rejected rather than written.
func validMetricName(name string) bool {
	if name == "" {
		return false
	}
	expr, err := ruleParser.ParseExpr(name)
	if err != nil {
		return false
	}
	vs, ok := expr.(*parser.VectorSelector)
	return ok && vs.Name == name
}
