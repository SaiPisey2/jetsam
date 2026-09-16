package corpus

import (
	"fmt"

	"github.com/SaiPisey2/jetsam/internal/promapi"
)

// Corpus is every query jetsam knows about, reduced to the question that
// matters: which metrics does anything read?
type Corpus struct {
	// Queries is how many queries were read. The PR body quotes it, so a
	// reader can judge how much evidence a verdict rests on.
	Queries int
	// Used is every metric some query touches.
	Used map[string]bool
	// Produced is every metric a recording rule writes. v0.1 never
	// proposes dropping one: that would mean editing a rule file, not a
	// scrape config.
	Produced map[string]bool
	// Blocked lists queries jetsam could not read, each with its source.
	// A non-empty Blocked forbids every drop -- an unreadable query may
	// reference anything at all.
	Blocked []string
}

// Build reads every rule and reports what they touch. Every rule
// Prometheus evaluates counts as a live consumer, so a metric read only by
// a recording rule is used, whether or not anything reads that rule's
// output. That is the safe direction: the alternative -- proving a
// recording rule dead and discounting it -- needs dashboard evidence jetsam
// does not have in v0.1.
func Build(rules []promapi.Rule, allMetrics []string) Corpus {
	c := Corpus{Used: map[string]bool{}, Produced: map[string]bool{}}
	for _, r := range rules {
		c.Queries++
		if r.Type == "recording" {
			c.Produced[r.Name] = true
		}
		refs, err := Extract(r.Query)
		if err != nil {
			c.Blocked = append(c.Blocked, fmt.Sprintf("rule %s/%s: %v", r.Group, r.Name, err))
			continue
		}
		for _, m := range refs.Resolve(allMetrics) {
			c.Used[m] = true
		}
	}
	return c
}
