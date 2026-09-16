package corpus

import (
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/promapi"
)

func testRules() []promapi.Rule {
	return []promapi.Rule{
		{Group: "api", Name: "ErrorRateHigh", Type: "alerting",
			Query: `sum(rate(http_requests_total{code=~"5.."}[5m])) > 1`},
		{Group: "api", Name: "job:latency:p99", Type: "recording",
			Query: `histogram_quantile(0.99, sum by (le, job) (rate(http_request_duration_seconds_bucket[5m])))`},
	}
}

func TestBuildMarksEveryRuleInputUsed(t *testing.T) {
	all := []string{"http_requests_total", "http_request_duration_seconds_bucket", "go_goroutines"}
	c := Build(testRules(), all)

	for _, want := range []string{"http_requests_total", "http_request_duration_seconds_bucket"} {
		if !c.Used[want] {
			t.Errorf("%s: Used = false, want true", want)
		}
	}
	if c.Used["go_goroutines"] {
		t.Error("go_goroutines: Used = true, want false — no rule reads it")
	}
	if c.Queries != 2 {
		t.Errorf("Queries = %d, want 2", c.Queries)
	}
}

func TestBuildRecordsWhatRecordingRulesProduce(t *testing.T) {
	// A recording rule's OUTPUT is a real series in the inventory. v0.1
	// never proposes dropping one, because doing so means editing a rule
	// file rather than a scrape config.
	c := Build(testRules(), []string{"http_requests_total"})
	if !c.Produced["job:latency:p99"] {
		t.Error("Produced[job:latency:p99] = false, want true")
	}
	if c.Produced["ErrorRateHigh"] {
		t.Error("Produced[ErrorRateHigh] = true — an alerting rule produces no series")
	}
}

func TestBuildBlocksOnAnUnparseableQuery(t *testing.T) {
	rules := []promapi.Rule{{Group: "g", Name: "Broken", Type: "alerting", Query: `rate(x[$interval])`}}
	c := Build(rules, []string{"x", "y"})
	if len(c.Blocked) != 1 {
		t.Fatalf("Blocked = %v, want one entry naming the unreadable rule", c.Blocked)
	}
	if c.Used["y"] {
		t.Error("y: Used = true — a blocked corpus must not mark unrelated metrics used")
	}
}

// TestBuildGivesABlockedRuleNoEffectAtAll pins the invariant that a rule
// jetsam cannot read must contribute nothing, in either direction: not to
// Used (an unreadable query's text may name a metric it doesn't actually
// touch, or may touch metrics it doesn't name) and not to Produced (jetsam
// cannot trust that a rule which failed to parse is even what it claims to
// write). This must be enforced by Build itself -- by never touching either
// map before a parse error is checked -- rather than resting on the
// incidental fact that Extract's error path happens to return a zero-value
// Refs{} today. If that contract ever changed, a silent regression here
// would let jetsam propose drops based on evidence it could not read.
func TestBuildGivesABlockedRuleNoEffectAtAll(t *testing.T) {
	rules := []promapi.Rule{
		{Group: "g", Name: "Good", Type: "alerting", Query: `up == 0`},
		{Group: "g", Name: "Broken", Type: "recording", Query: `rate(x[$interval])`},
	}
	c := Build(rules, []string{"up", "x", "unrelated"})

	if !c.Used["up"] {
		t.Error("up: Used = false, want true — the clean rule was processed normally")
	}
	if c.Used["x"] {
		t.Error("x: Used = true — the broken rule's query text names x, but an unreadable rule must contribute nothing")
	}
	if c.Used["unrelated"] {
		t.Error("unrelated: Used = true, want false")
	}
	if c.Produced["Broken"] {
		t.Error("Produced[Broken] = true — a rule that could not be parsed must not even register what it claims to produce")
	}
	if len(c.Blocked) != 1 {
		t.Fatalf("Blocked = %v, want exactly one entry", c.Blocked)
	}
	if !strings.Contains(c.Blocked[0], "g") || !strings.Contains(c.Blocked[0], "Broken") {
		t.Errorf("Blocked[0] = %q, want it to name both the group %q and the rule %q", c.Blocked[0], "g", "Broken")
	}
}
