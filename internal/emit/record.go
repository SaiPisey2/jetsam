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
// Every field p carries is checked before anything touches the tree, for
// three different reasons:
//
//  1. p.Metric and p.RuleName are untrusted the same way a scraped metric
//     name is untrusted anywhere else in this package -- see relabel.go.
//     Neither is ever interpolated into YAML source and re-parsed, only
//     set as a yaml.Node's Value, so a newline or a quote in either cannot
//     inject YAML structure.
//  2. That is not enough here, because the scalar being written is itself
//     PromQL, not a passive string like a job name -- and parsing the
//     ASSEMBLED expression afterward is not enough either, because a
//     hostile p.Metric can close jetsam's own parenthesis and open one of
//     its own, or open a "#" comment that swallows it, and the finished
//     string still parses. "m_total) unless vector(1" as Metric renders
//     "sum(m_total) unless vector(1)" -- a different query entirely, and a
//     perfectly valid one. So p.Metric is checked the same way p.RuleName
//     already is, BEFORE the two are ever combined: validMetricName
//     requires it read back, on its own, as a bare selector naming itself.
//     A legitimate metric that needs quoting (Prometheus 3's UTF-8 names
//     can contain spaces or punctuation) is refused here rather than
//     rendered -- jetsam has no quoted-selector rendering to fall back to,
//     so refusing is the honest answer, not a verdict that the name is
//     malformed.
//  3. p.Op, p.Keep and p.RuleName can each be individually well-formed and
//     still not agree with each other or with what Decide would have
//     produced: an Op outside {sum, min, max} renders something that does
//     not compose (or, for the empty string, no aggregation at all); a
//     duplicate label in Keep renders the harmless-looking but false "by
//     (path, path)"; and a RuleName that simply does not match what
//     Metric/Keep/Op/Fn/Window would name looks like an ordinary rule in a
//     diff while recording under a name nothing asked for. All three are
//     rejected: Op against the fixed list Decide itself enforces, Keep by
//     refusing a repeated label outright rather than silently deduping it,
//     and RuleName by recomputing it with aggregate.RuleName -- the same
//     function Decide calls -- and erroring on any disagreement.
//
// The rendered expression is still parsed with the same PromQL parser the
// corpus uses, as a final check on the whole assembled string rather than
// its parts individually.
//
// Refuses, rather than overwrites, when a rule already exists under
// p.RuleName anywhere in the file: a same-named rule could be something an
// operator wrote by hand, or something an earlier jetsam run recorded from
// different inputs, and either way silently replacing it is worse than
// declining the change.
//
// Like relabel.go's Render, the encoder is configured with SetIndent(2),
// so a ruleYAML already written in the ordinary 2-space style round-trips
// with only the added lines changed; one written at some other indent
// width is renormalised to 2-space on output, because yaml.v3's node tree
// does not record the original width, only structure and comments.
func RenderRule(ruleYAML string, p aggregate.Proposal) (string, error) {
	if p.Op != "sum" && p.Op != "min" && p.Op != "max" {
		return "", fmt.Errorf("operator %q is not sum, min or max", p.Op)
	}
	if label, dup := duplicateLabel(p.Keep); dup {
		return "", fmt.Errorf("keep label %q appears more than once", label)
	}
	if !validMetricName(p.Metric) {
		return "", fmt.Errorf("metric name %q is not a plain identifier PromQL can select without quoting, and jetsam does not render a quoted selector", p.Metric)
	}
	if !validMetricName(p.RuleName) {
		return "", fmt.Errorf("record name %q is not a valid metric name", p.RuleName)
	}
	if want := aggregate.RuleName(p.Metric, p.Keep, p.Op, p.Fn, p.Window); want != p.RuleName {
		return "", fmt.Errorf("record name %q does not match what Metric, Keep, Op, Fn and Window would name (%q)", p.RuleName, want)
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

// duplicateLabel reports the first label that appears more than once in
// keep, and whether one exists. Real Decide output can never contain a
// duplicate -- Required is built from a set's keys -- but RenderRule takes
// nothing about Proposal on faith once it is in hand (the same "checked
// anyway rather than trusted from a distance" discipline decide.go itself
// uses on Op, for the same reason): a duplicate here would render the
// harmless-looking, well-formed, and false "by (path, path)", so it is
// rejected outright rather than silently deduplicated.
func duplicateLabel(keep []string) (string, bool) {
	seen := make(map[string]bool, len(keep))
	for _, k := range keep {
		if seen[k] {
			return k, true
		}
		seen[k] = true
	}
	return "", false
}

// validMetricName reports whether name is safe to use, unquoted, as a bare
// PromQL selector -- RenderRule calls it on both p.Metric and p.RuleName,
// for two different failure modes it closes at once. Parsed back through
// the same parser Prometheus itself uses, a valid name reads as a bare
// selector naming itself: a quote, a newline, a stray parenthesis or
// bracket, or a leading "-" either breaks the parse outright or parses into
// something other than a plain selector on that name (a unary expression,
// a brace-qualified selector, a selector with an offset), and any of those
// fails Name == name. Checking the name ALONE, before it is ever combined
// with anything else into a larger expression string, is what catches a
// name built to close a parenthesis jetsam appended around it or open a
// "#" comment that swallows the rest of that string -- parsing the finished
// expression afterward could not distinguish that from an ordinary,
// unrelated parse failure.
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
