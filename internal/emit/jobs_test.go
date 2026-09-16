package emit

import "testing"

func TestJobNamesReturnsEveryDefinedJob(t *testing.T) {
	names, err := JobNames(sampleYAML)
	if err != nil {
		t.Fatalf("JobNames: %v", err)
	}
	for _, want := range []string{"api", "worker"} {
		if !names[want] {
			t.Errorf("JobNames() missing %q; got %v", want, names)
		}
	}
	if len(names) != 2 {
		t.Errorf("JobNames() = %v, want exactly {api, worker}", names)
	}
}

func TestJobNamesOnAnEmptyScrapeConfigsIsNotAnError(t *testing.T) {
	// Unlike Render, JobNames must not error here: a missing or empty
	// scrape_configs just means every candidate will decline, reported by
	// the caller like any other declined candidate.
	names, err := JobNames("global:\n  scrape_interval: 15s\n")
	if err != nil {
		t.Fatalf("JobNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("JobNames() = %v, want empty", names)
	}
}

func TestJobNamesOnUnparseableYAMLIsAnError(t *testing.T) {
	if _, err := JobNames("not: [valid: yaml"); err == nil {
		t.Fatal("JobNames accepted unparseable YAML, want an error")
	}
}
