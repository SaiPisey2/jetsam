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
