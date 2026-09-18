package querylog

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// entry writes one query-log line. kind is "read" or "rule".
func entry(ts, query, kind string) string {
	if kind == "rule" {
		return `{"time":"` + ts + `","ruleGroup":{"name":"g","file":"f"},"params":{"query":"` + query + `"}}` + "\n"
	}
	return `{"time":"` + ts + `","httpRequest":{"clientIP":"1.2.3.4","method":"GET","path":"/api/v1/query"},"params":{"query":"` + query + `"}}` + "\n"
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGz(t *testing.T, dir, name, content string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadCountsOnlyRealReads is the substance of this package. A real query
// log measured on the fixture was 100% rule evaluations -- 92,461 entries with
// four genuine reads. If rule evaluations counted as reads, every metric any
// rule touches would grade as queried, which is circular: the rules already
// ARE the corpus.
func TestReadCountsOnlyRealReads(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-01T00:00:00.000Z", "up", "read")+
			entry("2026-08-10T00:00:00.000Z", "rule_only_metric", "rule")+
			entry("2026-08-20T00:00:00.000Z", "node_cpu_seconds_total", "read"))

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Queries) != 2 {
		t.Errorf("Queries = %v, want the 2 reads only", r.Queries)
	}
	for _, q := range r.Queries {
		if q == "rule_only_metric" {
			t.Error("a rule evaluation was counted as a read")
		}
	}
}

// The span is what licenses the unqueried grade, so it must come from the log
// itself and must span every matched file, not just one.
func TestSpanCoversEveryMatchedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log", entry("2026-08-31T00:00:00.000Z", "recent", "read"))
	write(t, dir, "queries.log.1", entry("2026-08-01T00:00:00.000Z", "older", "read"))

	r, err := Read(filepath.Join(dir, "queries.log*"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.Files != 2 {
		t.Errorf("Files = %d, want 2", r.Files)
	}
	want := 30 * 24 * time.Hour
	if r.Span != want {
		t.Errorf("Span = %v, want %v -- the span must cover every matched file", r.Span, want)
	}
}

func TestReadsGzippedMembers(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log", entry("2026-08-31T00:00:00.000Z", "plain_metric", "read"))
	writeGz(t, dir, "queries.log.1.gz", entry("2026-08-01T00:00:00.000Z", "gzipped_metric", "read"))

	r, err := Read(filepath.Join(dir, "queries.log*"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	found := map[string]bool{}
	for _, q := range r.Queries {
		found[q] = true
	}
	if !found["gzipped_metric"] {
		t.Errorf("Queries = %v, want the gzipped member's read included", r.Queries)
	}
}

// A read whose own query text contains the literal string "ruleGroup" must
// still count. The prefilter selects positively on httpRequest rather than
// rejecting on ruleGroup precisely so that false-rejecting a real read is
// impossible -- that is the dangerous direction, because it makes a metric
// someone reads look unread.
func TestAReadMentioningRuleGroupStillCounts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-01T00:00:00.000Z", `metric_about_ruleGroup_health`, "read"))

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Queries) != 1 {
		t.Errorf("Queries = %v, want the read counted despite naming ruleGroup", r.Queries)
	}
}

func TestEmptyGlobMeansNoLogConfigured(t *testing.T) {
	r, err := Read("")
	if err != nil {
		t.Fatalf("Read(\"\"): %v", err)
	}
	if r != nil {
		t.Errorf("Read(\"\") = %v, want nil -- no log configured is not the same as an empty log", r)
	}
}

// A configured glob that matches nothing is a mistake worth reporting, not an
// empty log. An empty log would license dropping everything.
func TestGlobMatchingNothingIsAnError(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "nothing-here-*.log")); err == nil {
		t.Fatal("want an error when the configured glob matches no file")
	}
}

func TestMalformedLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log", `{"time":"2026-08-01T00:00:00.000Z","httpRequest":{},`+"\n")
	if _, err := Read(filepath.Join(dir, "queries.log")); err == nil {
		t.Fatal("want an error on a malformed line -- a log jetsam cannot read must not look like one with no reads")
	}
}

// If Span were computed only from read timestamps, a log containing zero
// reads -- the ordinary case, since a real log is close to 100% rule
// evaluations -- would report Span=0 no matter how long it had actually been
// running. That would silently disable the unqueried grade in production,
// however many months of coverage existed, because the trust threshold gates
// on Span >= a minimum window.
func TestSpanCoversRuleEvaluationsToo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-01T00:00:00Z", "rule_one", "rule")+
			entry("2026-08-31T00:00:00Z", "rule_two", "rule"))

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := 30 * 24 * time.Hour
	if r.Span != want {
		t.Errorf("Span = %v, want %v -- rule evaluations are evidence the log was running, even with zero reads", r.Span, want)
	}
	if len(r.Queries) != 0 {
		t.Errorf("Queries = %v, want none -- a log with no reads must report none", r.Queries)
	}
}

// The first and last entries in the file set the bound regardless of kind; a
// read in the middle must not narrow it.
func TestSpanUsesFirstAndLastEntryRegardlessOfKind(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-01T00:00:00Z", "opening_rule", "rule")+
			entry("2026-08-15T00:00:00Z", "middle_read", "read")+
			entry("2026-08-31T00:00:00Z", "closing_rule", "rule"))

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := 30 * 24 * time.Hour
	if r.Span != want {
		t.Errorf("Span = %v, want %v -- must cover the opening and closing entries, not just the read in the middle", r.Span, want)
	}
	if len(r.Queries) != 1 || r.Queries[0] != "middle_read" {
		t.Errorf("Queries = %v, want just the middle read", r.Queries)
	}
}

// Prometheus writes UTC in practice, but a log that ever reported a real
// offset must not be silently misread as UTC -- a wrong span by hours is
// exactly the kind of error nobody would notice. parseTime applies the
// offset via the standard library rather than discarding it.
func TestParseTimeHandlesTimezoneOffsets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-01T05:00:00+05:00", "early_rule", "rule")+ // = 2026-08-01T00:00:00Z
			entry("2026-08-01T02:00:00Z", "late_read", "read")) // = 2026-08-01T02:00:00Z

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := 2 * time.Hour
	if r.Span != want {
		t.Errorf("Span = %v, want %v -- the +05:00 offset must be applied, not dropped", r.Span, want)
	}
}

// Span must not trust physical file order. An inflated span is the dangerous
// direction -- it can push a log past the trust threshold and license a drop
// it should not -- so the bound has to be a running min/max over every
// entry's actual parsed time, not whichever line happens to come first or
// last on disk.
func TestSpanIsCorrectWhenLinesAreOutOfOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-31T00:00:00Z", "physically_first_but_latest", "rule")+
			entry("2026-08-15T00:00:00Z", "middle", "rule")+
			entry("2026-08-01T00:00:00Z", "physically_last_but_earliest", "rule"))

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := 30 * 24 * time.Hour
	if r.Span != want {
		t.Errorf("Span = %v, want %v -- physical position in the file must not matter", r.Span, want)
	}
}

// Same as above, but mixing kinds and physical order together, since the
// running min/max has to hold regardless of which kind of entry carries the
// true extreme.
func TestSpanIgnoresPhysicalPositionAcrossKinds(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-20T00:00:00Z", "mid_read", "read")+
			entry("2026-08-31T00:00:00Z", "latest_rule", "rule")+
			entry("2026-08-01T00:00:00Z", "earliest_rule", "rule")+
			entry("2026-08-10T00:00:00Z", "another_read", "read"))

	r, err := Read(filepath.Join(dir, "queries.log"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := 30 * 24 * time.Hour
	if r.Span != want {
		t.Errorf("Span = %v, want %v -- span must track the true earliest/latest regardless of physical order or kind", r.Span, want)
	}
}

// A truncated or garbled line buried in the middle of the file -- not the
// physically first or last line -- must still be caught. A log that cannot
// be fully read is an error, never an entry silently dropped; a corrupted
// rule-evaluation line (the ~100% case on a real log) must not be absorbed
// as an unremarkable entry just because it never reaches the httpRequest
// prefilter's JSON decode.
func TestAMalformedRuleLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log",
		entry("2026-08-01T00:00:00Z", "opening", "rule")+
			`{"time":"2026-08-15T00:00:00`+"\n"+ // truncated mid-write: no closing quote
			entry("2026-08-31T00:00:00Z", "closing", "rule"))

	if _, err := Read(filepath.Join(dir, "queries.log")); err == nil {
		t.Fatal("want an error on a truncated rule-shaped line buried in the file -- it must not be silently absorbed as an ordinary entry")
	}
}

// The fix for the above must not over-reject: a file ending in a single
// trailing newline, which is the ordinary way a log file is terminated, must
// not be mistaken for an unparseable final line.
func TestATrailingNewlineIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "queries.log", entry("2026-08-01T00:00:00Z", "trailing_newline_ok", "read"))

	if _, err := Read(filepath.Join(dir, "queries.log")); err != nil {
		t.Fatalf("Read: %v -- a well-formed file ending in a newline must not error", err)
	}
}
