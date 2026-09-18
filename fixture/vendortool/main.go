// Command vendortool converts a Kubernetes PrometheusRule manifest into a
// plain Prometheus rule file.
//
// The conversion is a YAML round trip rather than a text dedent on
// purpose. These manifests carry Go template expressions and colons
// inside their annotation strings, and a line-based dedent corrupts them
// in ways that still parse, which is the worst kind of wrong.
package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: vendortool <prometheusRule.yaml> <out.yaml>")
		os.Exit(2)
	}
	in, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var manifest struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Groups yaml.Node `yaml:"groups"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(in, &manifest); err != nil {
		fmt.Fprintln(os.Stderr, "parse manifest:", err)
		os.Exit(1)
	}
	if manifest.Kind != "PrometheusRule" {
		fmt.Fprintf(os.Stderr, "expected a PrometheusRule manifest, got kind %q\n", manifest.Kind)
		os.Exit(1)
	}
	if manifest.Spec.Groups.IsZero() {
		fmt.Fprintln(os.Stderr, "manifest has no spec.groups")
		os.Exit(1)
	}

	out, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer out.Close()

	doc := yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "groups"},
			&manifest.Spec.Groups,
		},
	}
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}
	if err := enc.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}
}
