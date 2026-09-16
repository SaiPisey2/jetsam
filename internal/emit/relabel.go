// Package emit turns verdicts into the change that implements them.
package emit

import (
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
		metrics := make([]string, 0, len(ds))
		for _, d := range ds {
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

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("render prometheus.yml: %w", err)
	}
	return string(out), nil
}

// appendDrops adds one drop rule per metric to sc's metric_relabel_configs,
// creating the key when it is absent. metrics must already be sorted so
// re-running Render on the same input produces byte-identical output.
func appendDrops(sc *yaml.Node, metrics []string) {
	list := mappingValue(sc, "metric_relabel_configs")
	if list == nil {
		sc.Content = append(sc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "metric_relabel_configs"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		list = sc.Content[len(sc.Content)-1]
	}
	for _, m := range metrics {
		list.Content = append(list.Content, dropRule(m))
	}
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
