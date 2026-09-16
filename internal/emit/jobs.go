package emit

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// JobNames returns the set of job_name values promYAML's scrape_configs
// define.
//
// It exists so the propose layer can partition drop candidates BEFORE
// calling Render: Render itself stays strict on purpose (an unknown job is
// an error, never a silent skip, because skipping would leave a PR
// claiming a saving the config does not deliver) and that strictness is
// correct for a function told to edit one specific file. But a whole
// propose run aborting because ONE candidate among many belongs to a job
// this file does not define -- scrape configs split across files via
// scrape_config_files, or jobs arriving from service discovery, or an
// operator pointing jetsam at one file among several -- makes jetsam
// useless against almost any real production Prometheus. The fix belongs
// above Render, not in it: know the defined jobs up front, route only the
// candidates that match to Render, and report the rest as declined.
//
// A missing or empty scrape_configs is not an error here, unlike in
// Render: it just means every candidate will decline, which the caller
// reports the same way as any other declined candidate, not as a crash.
func JobNames(promYAML string) (map[string]bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(promYAML), &doc); err != nil {
		return nil, fmt.Errorf("parse prometheus.yml: %w", err)
	}
	names := map[string]bool{}
	if len(doc.Content) == 0 {
		return names, nil
	}
	scrapes := mappingValue(doc.Content[0], "scrape_configs")
	if scrapes == nil {
		return names, nil
	}
	for _, sc := range scrapes.Content {
		if name := scalarValue(mappingValue(sc, "job_name")); name != "" {
			names[name] = true
		}
	}
	return names, nil
}
