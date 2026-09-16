package emit

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleYAML = `global:
  scrape_interval: 15s

scrape_configs:
  # the API tier, owned by the platform team
  - job_name: api
    static_configs:
      - targets: ['localhost:9100']

  - job_name: worker
    static_configs:
      - targets: ['localhost:9101']
`

func TestRenderAppendsADropToTheRightJob(t *testing.T) {
	got, err := Render(sampleYAML, []Drop{{Metric: "go_gc_duration_seconds", Series: 1258, Job: "api"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, "metric_relabel_configs") {
		t.Fatalf("no metric_relabel_configs emitted:\n%s", got)
	}
	// The drop must land under job api, not job worker.
	api := got[strings.Index(got, "job_name: api"):strings.Index(got, "job_name: worker")]
	if !strings.Contains(api, "go_gc_duration_seconds") {
		t.Errorf("drop did not land under job api:\n%s", got)
	}
}

func TestRenderPreservesCommentsElsewhere(t *testing.T) {
	// An edit that strips an operator's comments out of their scrape config
	// is not mergeable, however correct the relabel rule is.
	got, err := Render(sampleYAML, []Drop{{Metric: "x", Series: 1, Job: "api"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, "owned by the platform team") {
		t.Errorf("comment lost:\n%s", got)
	}
}

func TestRenderRefusesAnUnknownJob(t *testing.T) {
	// Silently skipping means the PR claims a saving it does not deliver.
	if _, err := Render(sampleYAML, []Drop{{Metric: "x", Job: "ghost"}}); err == nil {
		t.Fatal("Render accepted a job with no scrape_config, want an error")
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	drops := []Drop{
		{Metric: "z_metric", Series: 1, Job: "api"},
		{Metric: "a_metric", Series: 2, Job: "api"},
		{Metric: "m_metric", Series: 3, Job: "api"},
	}
	first, err := Render(sampleYAML, drops)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := Render(sampleYAML, drops)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if first != second {
		t.Fatalf("Render is not deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// ruleCount returns how many entries sit in metric_relabel_configs under
// job, without assuming anything about their shape -- unlike findRegex,
// this must not fail the test on a malformed or unrelated entry, since
// several dedupe tests deliberately plant those.
func ruleCount(t *testing.T, renderedYAML, job string) int {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(renderedYAML), &doc); err != nil {
		t.Fatalf("rendered YAML does not parse: %v\n%s", err, renderedYAML)
	}
	root := doc.Content[0]
	var scrapes *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "scrape_configs" {
			scrapes = root.Content[i+1]
		}
	}
	if scrapes == nil {
		t.Fatalf("no scrape_configs in rendered YAML:\n%s", renderedYAML)
	}
	for _, sc := range scrapes.Content {
		var name string
		var relabels *yaml.Node
		for i := 0; i+1 < len(sc.Content); i += 2 {
			switch sc.Content[i].Value {
			case "job_name":
				name = sc.Content[i+1].Value
			case "metric_relabel_configs":
				relabels = sc.Content[i+1]
			}
		}
		if name != job {
			continue
		}
		if relabels == nil {
			return 0
		}
		return len(relabels.Content)
	}
	t.Fatalf("job %q not found in rendered YAML:\n%s", job, renderedYAML)
	return -1
}

// TestRenderIsIdempotentAcrossRepeatedRuns feeds Render's own output back
// into Render with the same drop, as happens across repeated scans of an
// evolving repo (a second scan after a PR merges, or two runs in one
// session). Without dedupe this appends a byte-identical rule on every
// pass, growing the operator's prometheus.yml without bound.
func TestRenderIsIdempotentAcrossRepeatedRuns(t *testing.T) {
	drop := []Drop{{Metric: "go_gc_duration_seconds", Series: 1258, Job: "api"}}

	first, err := Render(sampleYAML, drop)
	if err != nil {
		t.Fatalf("Render (pass 1): %v", err)
	}
	if n := ruleCount(t, first, "api"); n != 1 {
		t.Fatalf("pass 1: %d rules under job api, want 1", n)
	}

	second, err := Render(first, drop)
	if err != nil {
		t.Fatalf("Render (pass 2): %v", err)
	}
	if n := ruleCount(t, second, "api"); n != 1 {
		t.Fatalf("pass 2: %d rules under job api, want 1 (dedupe did not hold)", n)
	}

	third, err := Render(second, drop)
	if err != nil {
		t.Fatalf("Render (pass 3): %v", err)
	}
	if n := ruleCount(t, third, "api"); n != 1 {
		t.Fatalf("pass 3: %d rules under job api, want 1 (dedupe did not hold)", n)
	}
}

// TestRenderTwiceIsByteIdentical asserts that two independent Render calls
// against the same fresh input -- not feeding one into the other -- also
// agree byte-for-byte, so a re-run in CI or a second PR run produces an
// empty diff rather than a changed file.
func TestRenderTwiceIsByteIdentical(t *testing.T) {
	drop := []Drop{{Metric: "go_gc_duration_seconds", Series: 1258, Job: "api"}}
	a, err := Render(sampleYAML, drop)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b, err := Render(sampleYAML, drop)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if a != b {
		t.Fatalf("two renders of the same input differ:\na:\n%s\nb:\n%s", a, b)
	}
}

const sampleYAMLWithUnrelatedRule = `global:
  scrape_interval: 15s

scrape_configs:
  - job_name: api
    static_configs:
      - targets: ['localhost:9100']
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: some_other_metric_entirely
        action: drop
`

// TestRenderLeavesAnUnrelatedRuleAloneAndDoesNotDuplicateItsOwn plants a
// pre-existing drop rule for a different metric under job api, then asks
// Render to drop a second metric. The unrelated rule must survive
// untouched, and jetsam's own rule must be added exactly once (not
// duplicated on a second pass).
func TestRenderLeavesAnUnrelatedRuleAloneAndDoesNotDuplicateItsOwn(t *testing.T) {
	drop := []Drop{{Metric: "go_gc_duration_seconds", Series: 1, Job: "api"}}
	got, err := Render(sampleYAMLWithUnrelatedRule, drop)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, "some_other_metric_entirely") {
		t.Fatalf("unrelated pre-existing rule was lost:\n%s", got)
	}
	if n := ruleCount(t, got, "api"); n != 2 {
		t.Fatalf("got %d rules under job api, want 2 (1 unrelated + 1 new)", n)
	}

	again, err := Render(got, drop)
	if err != nil {
		t.Fatalf("Render (second pass): %v", err)
	}
	if n := ruleCount(t, again, "api"); n != 2 {
		t.Fatalf("second pass: got %d rules under job api, want 2 (new rule was duplicated)", n)
	}
}

const sampleYAMLWithKeepRule = `global:
  scrape_interval: 15s

scrape_configs:
  - job_name: api
    static_configs:
      - targets: ['localhost:9100']
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: go_gc_duration_seconds
        action: keep
`

// TestRenderDoesNotTreatAKeepRuleAsEquivalentToADrop plants a pre-existing
// rule with the same regex jetsam would emit, but action: keep instead of
// action: drop. A keep rule is not a drop rule, so it must not suppress
// jetsam's drop -- the two rules do different, non-overlapping things.
func TestRenderDoesNotTreatAKeepRuleAsEquivalentToADrop(t *testing.T) {
	drop := []Drop{{Metric: "go_gc_duration_seconds", Series: 1, Job: "api"}}
	got, err := Render(sampleYAMLWithKeepRule, drop)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := ruleCount(t, got, "api"); n != 2 {
		t.Fatalf("got %d rules under job api, want 2 (pre-existing keep + new drop)", n)
	}
	if !strings.Contains(got, "action: keep") || !strings.Contains(got, "action: drop") {
		t.Fatalf("expected both a keep and a drop rule:\n%s", got)
	}
}

const sampleYAMLWithMalformedRule = `global:
  scrape_interval: 15s

scrape_configs:
  - job_name: api
    static_configs:
      - targets: ['localhost:9100']
    metric_relabel_configs:
      - source_labels: __name__
        regex: go_gc_duration_seconds
        action: drop
`

// TestRenderTreatsAMalformedExistingRuleAsNotEquivalent plants a
// pre-existing rule whose source_labels is a bare scalar rather than a
// sequence -- not a shape this package ever emits. Render must not panic,
// and must not mistake it for an equivalent rule: on doubt, the safe
// direction is a duplicate-looking rule, not a silently skipped drop.
func TestRenderTreatsAMalformedExistingRuleAsNotEquivalent(t *testing.T) {
	drop := []Drop{{Metric: "go_gc_duration_seconds", Series: 1, Job: "api"}}
	got, err := Render(sampleYAMLWithMalformedRule, drop)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := ruleCount(t, got, "api"); n != 2 {
		t.Fatalf("got %d rules under job api, want 2 (malformed original + jetsam's new one)", n)
	}
}

// TestRenderCollapsesDuplicateDropsInOneCall asks Render, in a single call,
// to drop the same metric twice for one job. It must emit one rule, not
// two.
func TestRenderCollapsesDuplicateDropsInOneCall(t *testing.T) {
	drops := []Drop{
		{Metric: "go_gc_duration_seconds", Series: 1, Job: "api"},
		{Metric: "go_gc_duration_seconds", Series: 999, Job: "api"},
	}
	got, err := Render(sampleYAML, drops)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := ruleCount(t, got, "api"); n != 1 {
		t.Fatalf("got %d rules under job api, want 1 (duplicate drops in one call were not collapsed)", n)
	}
}

// sampleYAMLManyStyles exercises, in one document, the styles a real
// prometheus.yml mixes: a 2-space indented block sequence, a flow-style
// mapping/sequence value, a single-quoted string and a double-quoted
// string, a comment above a job, an inline comment after a value, a
// comment inside a list, and a multi-line block scalar regex (unusual but
// legal). Two jobs are present; only "api" receives a drop in the tests
// below, so "grafana" is the sharpest signal available for reformatting:
// nothing about it changes at all.
const sampleYAMLManyStyles = `global:
  scrape_interval: 15s
scrape_configs:
  # the API tier, owned by the platform team
  - job_name: api
    static_configs:
      - targets: ['localhost:9100']
    relabel_configs:
      # rewrite the instance label before scraping
      - source_labels: [__address__]
        target_label: instance
        replacement: "static-instance"
        action: replace
  - job_name: grafana
    static_configs: [{targets: ['localhost:3000']}]
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: "grafana_build_info" # already dropped by hand
        action: drop
      - source_labels: [__name__]
        regex: |
          grafana_(request|response)_duration_seconds
        action: keep
`

// linesOf splits s into lines without a trailing empty element from a
// final newline, so line counts and indices line up with what a human
// reading the file would call "line 1", "line 2", etc.
func linesOf(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// assertOnlyAdditions checks that every line of input appears, verbatim
// and in the same relative order, somewhere in output -- i.e. input's
// lines are a subsequence of output's lines. It does not require input's
// lines to be contiguous in output: Render is allowed to insert new lines
// between them (that is the whole point), but every line already there,
// including its exact leading whitespace, must survive unmodified and
// unremoved.
func assertOnlyAdditions(t *testing.T, input, output string) {
	t.Helper()
	in := linesOf(input)
	out := linesOf(output)
	j := 0
	for i, want := range in {
		for j < len(out) && out[j] != want {
			j++
		}
		if j == len(out) {
			t.Fatalf("input line %d was changed or removed -- not found unchanged and in order in the output\nmissing line: %q\n\n--- input ---\n%s\n\n--- output ---\n%s",
				i+1, want, input, output)
		}
		j++
	}
}

// TestRenderChangesOnlyTheLinesItAdds is the sharp regression test for
// whole-file reformatting: it renders a drop into exactly one of two jobs
// and asserts every line of the original file -- 2-space sequences, flow
// style, both quote styles, all three comment positions, and a multi-line
// block scalar -- still appears, unchanged and in order, in the output.
// The untouched "grafana" job is the strongest signal: nothing under it
// should differ from the input at all.
func TestRenderChangesOnlyTheLinesItAdds(t *testing.T) {
	got, err := Render(sampleYAMLManyStyles, []Drop{{Metric: "go_gc_duration_seconds", Series: 1, Job: "api"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertOnlyAdditions(t, sampleYAMLManyStyles, got)

	// The untouched job's own rendered form should be byte-identical to
	// what it looked like before -- not just "present as a subsequence" --
	// since nothing added a line inside it.
	grafanaIn := sampleYAMLManyStyles[strings.Index(sampleYAMLManyStyles, "job_name: grafana"):]
	grafanaOut := got[strings.Index(got, "job_name: grafana"):]
	if grafanaOut != grafanaIn {
		t.Errorf("untouched job grafana was reformatted:\n--- before ---\n%s\n--- after ---\n%s", grafanaIn, grafanaOut)
	}
}

// TestRenderTwiceOnManyStylesIsByteIdentical re-confirms the idempotence
// property established earlier still holds once the encoder is configured
// explicitly (SetIndent(2)) rather than left at yaml.Marshal's default.
func TestRenderTwiceOnManyStylesIsByteIdentical(t *testing.T) {
	drop := []Drop{{Metric: "go_gc_duration_seconds", Series: 1, Job: "api"}}
	a, err := Render(sampleYAMLManyStyles, drop)
	if err != nil {
		t.Fatalf("Render (first): %v", err)
	}
	b, err := Render(sampleYAMLManyStyles, drop)
	if err != nil {
		t.Fatalf("Render (second): %v", err)
	}
	if a != b {
		t.Fatalf("two renders of the same many-styles input differ:\na:\n%s\nb:\n%s", a, b)
	}
	// And rendering the same drop into Render's own prior output must
	// still not grow the rule count (idempotence survives the encoder
	// change too).
	again, err := Render(a, drop)
	if err != nil {
		t.Fatalf("Render (against own output): %v", err)
	}
	if n := ruleCount(t, again, "api"); n != 1 {
		t.Fatalf("re-rendering against own output produced %d rules under job api, want 1", n)
	}
}

// findRegex walks the rendered YAML and returns the regex: value of the
// metric_relabel_configs entry for job/metric-index i (0-based, in document
// order under that job). It fails the test if the structure it expects is
// not there, so a YAML-injection that reshapes the document is caught even
// when it happens not to break parsing entirely.
func findRegex(t *testing.T, renderedYAML, job string) []string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(renderedYAML), &doc); err != nil {
		t.Fatalf("rendered YAML does not parse: %v\n%s", err, renderedYAML)
	}
	root := doc.Content[0]
	var scrapes *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "scrape_configs" {
			scrapes = root.Content[i+1]
		}
	}
	if scrapes == nil {
		t.Fatalf("no scrape_configs in rendered YAML:\n%s", renderedYAML)
	}
	var regexes []string
	for _, sc := range scrapes.Content {
		var name string
		var relabels *yaml.Node
		for i := 0; i+1 < len(sc.Content); i += 2 {
			switch sc.Content[i].Value {
			case "job_name":
				name = sc.Content[i+1].Value
			case "metric_relabel_configs":
				relabels = sc.Content[i+1]
			}
		}
		if name != job {
			continue
		}
		if relabels == nil {
			t.Fatalf("job %q has no metric_relabel_configs:\n%s", job, renderedYAML)
		}
		for _, rule := range relabels.Content {
			if rule.Kind != yaml.MappingNode {
				t.Fatalf("drop rule is not a mapping (kind=%v) -- possible YAML injection:\n%s", rule.Kind, renderedYAML)
			}
			var action, sourceLabels, regex string
			var sawRegex bool
			for i := 0; i+1 < len(rule.Content); i += 2 {
				switch rule.Content[i].Value {
				case "action":
					action = rule.Content[i+1].Value
				case "source_labels":
					if rule.Content[i+1].Kind == yaml.SequenceNode && len(rule.Content[i+1].Content) == 1 {
						sourceLabels = rule.Content[i+1].Content[0].Value
					}
				case "regex":
					regex = rule.Content[i+1].Value
					sawRegex = true
				}
			}
			if action != "drop" {
				t.Fatalf("drop rule action = %q, want %q:\n%s", action, "drop", renderedYAML)
			}
			if sourceLabels != "__name__" {
				t.Fatalf("drop rule source_labels = %q, want %q:\n%s", sourceLabels, "__name__", renderedYAML)
			}
			if !sawRegex {
				t.Fatalf("drop rule has no regex field:\n%s", renderedYAML)
			}
			regexes = append(regexes, regex)
		}
	}
	return regexes
}

// TestRenderEscapesYAMLStructure feeds metric names that, if interpolated
// into a YAML string and re-parsed, would break out of the scalar and
// inject structure into the operator's scrape config. Each must still
// produce exactly one well-formed drop rule naming that literal metric.
func TestRenderEscapesYAMLStructure(t *testing.T) {
	cases := []struct {
		name   string
		metric string
	}{
		{"embedded newline and injected action", "evil_metric\n  action: keep\n"},
		{"quote and colon-space", `weird"metric: value`},
		{"leading YAML alias indicator", "&anchor_metric"},
		{"leading YAML tag indicator", "!!str metric"},
		{"leading document marker", "---\nmetric"},
		{"leading flow-sequence indicator", "[not_a_list]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(sampleYAML, []Drop{{Metric: tc.metric, Series: 1, Job: "api"}})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			regexes := findRegex(t, got, "api")
			if len(regexes) != 1 {
				t.Fatalf("got %d drop rules under job api, want 1 (injection may have added/removed structure):\n%s", len(regexes), got)
			}
			re, err := regexp.Compile(regexes[0])
			if err != nil {
				t.Fatalf("emitted regex %q does not compile: %v", regexes[0], err)
			}
			if !re.MatchString(tc.metric) {
				t.Errorf("emitted regex %q does not match its own metric name %q", regexes[0], tc.metric)
			}
			// Comments elsewhere must still have survived the round trip.
			if !strings.Contains(got, "owned by the platform team") {
				t.Errorf("comment lost while rendering adversarial metric %q:\n%s", tc.metric, got)
			}
			// findRegex already proved the document has exactly one drop
			// rule under job api, with action: drop and source_labels:
			// [__name__] as real mapping keys -- so any "action: keep" or
			// extra structure the metric string contains landed as opaque
			// scalar content, not as sibling YAML keys. A raw substring
			// check on the rendered text cannot distinguish those two
			// cases (a correctly block-quoted scalar may contain the text
			// "action: keep" as data), so it is not asserted here.
		})
	}
}

// TestRenderEscapesRegexMetacharacters feeds metric names that are also
// meaningful RE2 patterns. Prometheus anchors metric_relabel_configs regex
// values, but an unescaped metacharacter still changes what the pattern
// matches: unescaped, these examples would match strings other than
// themselves, or (worse) an unrelated metric that happens to satisfy the
// pattern.
func TestRenderEscapesRegexMetacharacters(t *testing.T) {
	cases := []struct {
		name         string
		metric       string
		mustNotMatch []string
	}{
		{
			name:         "dot-star matches everything unescaped",
			metric:       ".*",
			mustNotMatch: []string{"up", "node_cpu_seconds_total", "go_gc_duration_seconds"},
		},
		{
			name:         "pipe is alternation unescaped",
			metric:       "foo|bar",
			mustNotMatch: []string{"foo", "bar"},
		},
		{
			name:         "dot matches any char unescaped",
			metric:       "go_gc_durationXseconds",
			mustNotMatch: []string{"go_gc_duration_seconds"},
		},
		{
			name:         "character class unescaped",
			metric:       "metric[0-9]",
			mustNotMatch: []string{"metric5"},
		},
		{
			name:         "parens and plus unescaped",
			metric:       "(metric)+",
			mustNotMatch: []string{"metric", "metricmetric"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(sampleYAML, []Drop{{Metric: tc.metric, Series: 1, Job: "api"}})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			regexes := findRegex(t, got, "api")
			if len(regexes) != 1 {
				t.Fatalf("got %d drop rules under job api, want 1:\n%s", len(regexes), got)
			}
			re, err := regexp.Compile("^(?:" + regexes[0] + ")$")
			if err != nil {
				t.Fatalf("emitted regex %q does not compile: %v", regexes[0], err)
			}
			if !re.MatchString(tc.metric) {
				t.Errorf("emitted regex %q does not match its own literal metric name %q", regexes[0], tc.metric)
			}
			for _, other := range tc.mustNotMatch {
				if re.MatchString(other) {
					t.Errorf("emitted regex %q for metric %q also matches unrelated metric %q -- over-broad drop", regexes[0], tc.metric, other)
				}
			}
		})
	}
}
