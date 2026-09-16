// Package emit turns verdicts into the change that implements them.
package emit

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Drop is one metric to stop storing, and the scrape job that produces it.
type Drop struct {
	Metric string
	Series int
	Job    string
}

// Render returns promYAML with a metric_relabel_configs drop appended to
// each named job. Jobs are matched by job_name; a drop naming a job with no
// scrape config is an error, never a silent skip -- skipping would leave the
// PR claiming a saving the config does not deliver.
//
// A metric name is untrusted: it was chosen by whatever scraped target
// exposed it, not by the operator editing this file. Two things follow.
//
//  1. Every drop rule is built by constructing yaml.Node values directly
//     and setting Value on scalar nodes -- never by interpolating the
//     metric name into a YAML string and re-parsing it. A metric name
//     containing a newline, a quote, a colon-space, or a leading YAML
//     indicator (&, *, !, ---, [, {) cannot break out of its scalar and
//     inject structure, because it is never handed to a YAML parser as
//     source text; the encoder is the only thing that decides how the
//     Value is quoted.
//  2. metric_relabel_configs' regex field is an anchored RE2 pattern, not
//     a literal string match. The metric name is escaped with
//     regexp.QuoteMeta before it becomes that value, so the rule drops
//     exactly the metric named and nothing a regex metacharacter in that
//     name would otherwise make it match too.
//
// Render is idempotent per job: a drop already covered by an equivalent
// rule in the input is not appended again, so re-running Render against its
// own prior output -- as happens across repeated scans of an evolving repo
// -- produces byte-identical output rather than growing the file on every
// run. See appendDrops for exactly what counts as equivalent.
//
// Render changes only the lines it adds. The encoder is configured with
// SetIndent(2) -- yaml.Marshal's own default is 4, which does not match
// the file it just parsed and would reformat every untouched line of a
// conventionally-written prometheus.yml, burying the actual change inside
// a whole-file diff nobody would review. This is not a guarantee for every
// possible input, though: a file already indented at some width other than
// 2 (4-space, tabs) is reformatted to 2-space on output, because yaml.v3's
// node tree does not record the original indent width, only structure and
// comments. Two more shapes are lost for the same reason -- the node tree
// keeps comment text but not layout around it: blank lines between nodes
// are dropped, and an inline "# comment" with more than one space before
// the "#" is renormalized to exactly one space. A prometheus.yml written
// in the ordinary 2-space style with single-space inline comments -- the
// near-universal Prometheus and general YAML convention -- round-trips
// with only the added lines changed.
func Render(promYAML string, drops []Drop) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(promYAML), &doc); err != nil {
		return "", fmt.Errorf("parse prometheus.yml: %w", err)
	}
	if len(doc.Content) == 0 {
		return "", fmt.Errorf("prometheus.yml is empty")
	}

	byJob := map[string][]Drop{}
	for _, d := range drops {
		byJob[d.Job] = append(byJob[d.Job], d)
	}

	scrapes := mappingValue(doc.Content[0], "scrape_configs")
	if scrapes == nil {
		return "", fmt.Errorf("prometheus.yml has no scrape_configs")
	}

	seen := map[string]bool{}
	for _, sc := range scrapes.Content {
		name := scalarValue(mappingValue(sc, "job_name"))
		ds, ok := byJob[name]
		if !ok {
			continue
		}
		seen[name] = true
		dedup := map[string]bool{}
		metrics := make([]string, 0, len(ds))
		for _, d := range ds {
			if dedup[d.Metric] {
				continue
			}
			dedup[d.Metric] = true
			metrics = append(metrics, d.Metric)
		}
		sort.Strings(metrics)
		appendDrops(sc, metrics)
	}
	jobs := make([]string, 0, len(byJob))
	for job := range byJob {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	for _, job := range jobs {
		if !seen[job] {
			return "", fmt.Errorf("no scrape_config with job_name %q", job)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", fmt.Errorf("render prometheus.yml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("render prometheus.yml: %w", err)
	}
	return buf.String(), nil
}

// appendDrops adds one drop rule per metric to sc's metric_relabel_configs,
// creating the key when it is absent. metrics must already be sorted and
// de-duplicated so re-running Render on the same input produces
// byte-identical output.
//
// A metric already covered by an equivalent existing rule is skipped, so
// that re-running Render against its own prior output -- as happens across
// repeated scans of an evolving repo -- does not append a second,
// byte-identical rule on every run. "Equivalent" is deliberately narrow:
// exact match on source_labels: [__name__], action: drop, and regex equal
// to regexp.QuoteMeta(m). This is never widened to regex containment (e.g.
// treating an existing `go_.*` as already covering `go_gc_duration_seconds`)
// -- getting that judgment wrong in the permissive direction would silently
// decline to drop something the operator asked to drop, which is worse than
// a harmless duplicate rule.
func appendDrops(sc *yaml.Node, metrics []string) {
	list := mappingValue(sc, "metric_relabel_configs")
	if list == nil {
		sc.Content = append(sc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "metric_relabel_configs"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		list = sc.Content[len(sc.Content)-1]
	}
	for _, m := range metrics {
		if hasEquivalentDropRule(list, regexp.QuoteMeta(m)) {
			continue
		}
		list.Content = append(list.Content, dropRule(m))
	}
}

// hasEquivalentDropRule reports whether list already contains a drop rule
// equivalent to one jetsam would emit for a metric whose escaped regex is
// escapedRegex. A rule that cannot be confidently read as equivalent --
// because it is malformed, or shaped differently than the ones this package
// emits -- is treated as not equivalent, never as a match: the safe
// direction on doubt is a duplicate rule, not a silently skipped drop.
func hasEquivalentDropRule(list *yaml.Node, escapedRegex string) bool {
	if list == nil {
		return false
	}
	for _, rule := range list.Content {
		if isEquivalentDropRule(rule, escapedRegex) {
			return true
		}
	}
	return false
}

// isEquivalentDropRule reports whether rule is a metric_relabel_configs
// entry with source_labels exactly [__name__], action exactly "drop", and
// regex exactly equal to escapedRegex. Any deviation -- missing keys,
// source_labels that is not a one-element sequence, a non-scalar regex or
// action, an extra key -- makes it report false rather than panic or guess.
func isEquivalentDropRule(rule *yaml.Node, escapedRegex string) bool {
	if rule == nil || rule.Kind != yaml.MappingNode {
		return false
	}
	var action, regex string
	var sawAction, sawRegex, sourceLabelsOK bool
	for i := 0; i+1 < len(rule.Content); i += 2 {
		key, val := rule.Content[i], rule.Content[i+1]
		if key == nil || key.Kind != yaml.ScalarNode || val == nil {
			continue
		}
		switch key.Value {
		case "action":
			if val.Kind == yaml.ScalarNode {
				action, sawAction = val.Value, true
			}
		case "regex":
			if val.Kind == yaml.ScalarNode {
				regex, sawRegex = val.Value, true
			}
		case "source_labels":
			sourceLabelsOK = val.Kind == yaml.SequenceNode &&
				len(val.Content) == 1 &&
				val.Content[0] != nil &&
				val.Content[0].Kind == yaml.ScalarNode &&
				val.Content[0].Value == "__name__"
		}
	}
	return sawAction && action == "drop" && sawRegex && regex == escapedRegex && sourceLabelsOK
}

// dropRule builds one metric_relabel_configs entry as a YAML mapping node,
// constructed directly rather than by interpolating the metric name into a
// YAML string and re-parsing it. The metric name becomes a scalar node's
// Value, and the regex value is regexp.QuoteMeta(m) so the rule matches
// that literal metric name and nothing else, regardless of what regex
// metacharacters or YAML-significant characters the name contains.
func dropRule(m string) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			strScalar("source_labels"),
			{
				Kind:  yaml.SequenceNode,
				Tag:   "!!seq",
				Style: yaml.FlowStyle,
				Content: []*yaml.Node{
					strScalar("__name__"),
				},
			},
			strScalar("regex"),
			strScalar(regexp.QuoteMeta(m)),
			strScalar("action"),
			strScalar("drop"),
		},
	}
}

// strScalar builds a plain string scalar node with the given value. Using
// Tag "!!str" and letting the encoder pick quoting style (it will switch to
// a quoted or block style itself when the value demands it) is what keeps
// this safe for arbitrary input -- the value is never treated as YAML
// source to be parsed.
func strScalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalarValue(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}
