package emit

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

// TestBodyTotalClampsRatherThanOverflowsNegative pins C3/F3: two drops of
// math.MaxInt-1 series each sum to more than math.MaxInt can represent.
// Plain int addition wraps that into a negative number -- the auditor's
// concrete repro produced the PR title "## jetsam: -4 series nothing
// reads" -- which is silently wrong in the one number this tool asks a
// human to trust before deleting data. Saturating addition clamps to
// math.MaxInt instead: still wrong (it understates the true sum) but
// visibly, absurdly wrong rather than silently negative.
func TestBodyTotalClampsRatherThanOverflowsNegative(t *testing.T) {
	huge := math.MaxInt - 1
	drops := []Drop{
		{Metric: "a", Series: huge, Job: "api"},
		{Metric: "b", Series: huge, Job: "api"},
	}
	title, _ := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1}, 0)
	if strings.Contains(title, "-") {
		t.Errorf("title went negative on overflow: %q", title)
	}
	want := fmt.Sprintf("%d", math.MaxInt)
	if !strings.Contains(title, want) {
		t.Errorf("title = %q, want it to contain the clamped total %s", title, want)
	}
}

func TestBodyWarnsThatHistoryIsNotRecoverable(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	// Reverting the commit restores collection but not the history that was
	// never written. It is the one mistake a reviewer cannot undo, so it
	// belongs near the top.
	if !strings.Contains(body, "cannot be recovered") {
		t.Errorf("body does not warn about unrecoverable history:\n%s", body)
	}
}

func TestBodyOmitsMoneyWithoutARate(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if strings.Contains(body, "$") {
		t.Errorf("body invented a cost with no rate configured:\n%s", body)
	}
}

func TestBodyShowsTheArithmeticWhenGivenARate(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 1000, Job: "api"}}, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 10000}, 0.0008)
	if !strings.Contains(body, "0.0008") {
		t.Errorf("body states a cost without the rate it used:\n%s", body)
	}
}

func TestBodyCountsWhatWasDeclined(t *testing.T) {
	res := verdict.Result{Blocked: []string{"rule g/Broken: parse query"}}
	_, body := Body(nil, res, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if !strings.Contains(body, "Broken") {
		t.Errorf("body hides what jetsam declined to touch:\n%s", body)
	}
}

// --- Adversarial: every remote-sourced string must be neutralized before it
// lands in the Markdown body. safe.Text handles control characters; a pipe
// or a backtick is Markdown's own problem, and body.go must handle it too.

// countTableRows returns how many lines of body begin with "|" -- the
// simplest structural signature of an intact Markdown table. A metric name
// that injects an unescaped "|" or pairs an unescaped backtick with another
// one elsewhere on the line can change this count without adding or
// removing a real drop, which is exactly the corruption these tests rule
// out.
func countTableRows(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "|") {
			n++
		}
	}
	return n
}

func TestBodyEscapesPipeInMetricName(t *testing.T) {
	drops := []Drop{{Metric: "bad|metric", Series: 5, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)

	// header + separator + exactly one data row for one drop.
	if got, want := countTableRows(body), 3; got != want {
		t.Errorf("table rows = %d, want %d (an unescaped pipe changed the row count):\n%s", got, want, body)
	}
	if !strings.Contains(body, `bad\|metric`) {
		t.Errorf("body does not escape the pipe in the metric name:\n%s", body)
	}
	if strings.Contains(body, "bad|metric") {
		t.Errorf("body contains an unescaped pipe in the metric name:\n%s", body)
	}
}

func TestBodyEscapesBacktickInMetricName(t *testing.T) {
	drops := []Drop{{Metric: "weird`metric", Series: 5, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)

	if got, want := countTableRows(body), 3; got != want {
		t.Errorf("table rows = %d, want %d (a backtick broke the table):\n%s", got, want, body)
	}
	// The metric name must never be wrapped in unescaped backticks: a
	// second backtick elsewhere on the same line could pair with it to
	// form an inline code span that swallows a real "|" delimiter.
	if strings.Contains(body, "weird`metric") {
		t.Errorf("body contains an unescaped backtick in the metric name:\n%s", body)
	}
}

func TestBodyRendersANSIEscapeVisibly(t *testing.T) {
	drops := []Drop{{Metric: "up\x1b[31mFAKE\x1b[0m", Series: 5, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)

	if strings.ContainsRune(body, 0x1b) {
		t.Errorf("raw ESC byte survived into the body:\n%q", body)
	}
	if !strings.Contains(body, `\x1b`) {
		t.Errorf("body does not render the ANSI escape visibly:\n%s", body)
	}
}

func TestBodyRendersNewlineInMetricNameVisibly(t *testing.T) {
	drops := []Drop{{Metric: "up\ninjected", Series: 5, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)

	if got, want := countTableRows(body), 3; got != want {
		t.Errorf("table rows = %d, want %d (a raw newline forged a row):\n%s", got, want, body)
	}
	if !strings.Contains(body, `\n`) {
		t.Errorf("body does not render the embedded newline visibly:\n%s", body)
	}
}

// TestBodyStatesTheGradeADropRestsOn pins the spec requirement that the
// body says which grade each drop rests on, and carries a distinct warning
// -- next to, not instead of, the irreversibility one -- when that grade is
// "unreferenced": no rule names the metric, but jetsam has no query log and
// so cannot see ad-hoc or Grafana Explore reads of it.
func TestBodyStatesTheGradeADropRestsOn(t *testing.T) {
	res := verdict.Result{Verdicts: []verdict.Verdict{
		{Metric: "x", Series: 10, Grade: verdict.GradeUnreferenced, Droppable: true},
	}}
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, res, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)

	if !strings.Contains(body, "unreferenced") {
		t.Errorf("body does not state the grade the drop rests on:\n%s", body)
	}
	if !strings.Contains(body, "-include-unreferenced") {
		t.Errorf("body does not warn that -include-unreferenced was needed to propose this drop:\n%s", body)
	}
	if !strings.Contains(body, "cannot be recovered") {
		t.Errorf("the unreferenced-grade warning must not replace the irreversibility warning:\n%s", body)
	}
}

func TestBodyDoesNotWarnAboutUnreferencedWhenEveryDropIsFullyEvidenced(t *testing.T) {
	res := verdict.Result{Verdicts: []verdict.Verdict{
		{Metric: "x", Series: 10, Grade: verdict.GradeUnqueried, Droppable: true},
	}}
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, res, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if strings.Contains(body, "-include-unreferenced") {
		t.Errorf("body warns about -include-unreferenced although every drop rests on unqueried grade:\n%s", body)
	}
}

func TestBodyEscapesBlockedEntries(t *testing.T) {
	res := verdict.Result{Blocked: []string{"rule g/Broken\x1b[31m: parse query"}}
	_, body := Body(nil, res, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if strings.ContainsRune(body, 0x1b) {
		t.Errorf("raw ESC byte survived from a blocked entry into the body:\n%q", body)
	}
}

// --- Markdown/HTML injection: a metric name, job name or blocked-query
// description can carry link, image and raw-HTML syntax. Anyone who can
// expose a metric on a scraped target controls this string, and a human
// reads this body to decide whether to permanently delete monitoring
// data -- so a phishing link or a tracking beacon must never render live.

// mdInjectionShapes are the concrete payloads a reviewer confirmed render
// as live Markdown/HTML today against an unpatched mdText: a clickable
// link, an image (a beacon: rendering it alone fires a GET request), a
// forged table cell close/open, a script tag, an HTML comment, a
// reference-style link, an image with an onerror handler, and a bare "<"
// and ">" (either could start or end a tag once other characters around it
// cooperate).
var mdInjectionShapes = []string{
	"[click me](http://evil.example/steal)",
	"![](http://evil.example/beacon.png)",
	"</td><td>forged",
	"<script>alert(1)</script>",
	"<!-- hidden -->",
	"[x][y]",
	"<img src=x onerror=alert(1)>",
	"<",
	">",
}

// assertNoLiveMarkupOrHTML checks the RENDERED body, not the input: a live
// link, an image, a script tag, a forged closing/opening tag pair, or an
// HTML comment must not appear literally in the output, and the drop
// table's row count must be exactly what one drop plus a header and
// separator produce -- neither more (a forged row) nor fewer (swallowed
// content).
func assertNoLiveMarkupOrHTML(t *testing.T, shape, body string) {
	t.Helper()
	for _, bad := range []string{"](http", "<script", "</", "<!--", "<img"} {
		if strings.Contains(body, bad) {
			t.Errorf("shape %q: body contains unneutralised %q:\n%s", shape, bad, body)
		}
	}
	if got, want := countTableRows(body), 3; got != want {
		t.Errorf("shape %q: table rows = %d, want %d (injection changed table structure):\n%s", shape, got, want, body)
	}
}

// subtestName turns shape into a readable, slash-free subtest name: several
// of the payloads above contain "/", which Go's testing package would
// otherwise read as a subtest hierarchy separator.
func subtestName(i int, shape string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return -1
		}
	}, shape)
	return fmt.Sprintf("%02d_%s", i, clean)
}

func TestBodyNeutralisesMarkdownAndHTMLInMetricName(t *testing.T) {
	for i, shape := range mdInjectionShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: shape, Series: 5, Job: "api"}}
			_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
			assertNoLiveMarkupOrHTML(t, shape, body)
		})
	}
}

func TestBodyNeutralisesMarkdownAndHTMLInJobName(t *testing.T) {
	for i, shape := range mdInjectionShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: "x", Series: 5, Job: shape}}
			_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
			assertNoLiveMarkupOrHTML(t, shape, body)
		})
	}
}

func TestBodyNeutralisesMarkdownAndHTMLInBlockedEntries(t *testing.T) {
	for i, shape := range mdInjectionShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: "x", Series: 5, Job: "api"}}
			res := verdict.Result{Blocked: []string{shape}}
			_, body := Body(drops, res, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
			assertNoLiveMarkupOrHTML(t, shape, body)
		})
	}
}

// --- GFM extended autolink: unlike the delimiter-based injections above,
// these payloads carry no "[", "(", "<" or any other Markdown/HTML
// delimiter at all. GitHub Flavored Markdown's extended autolink extension
// turns bare "www.", "http://", "https://" and email-shaped text into live
// links purely from their own characters, so a job label or a UTF-8 metric
// name that merely contains a "." or an "@" is enough to render as a
// clickable link or a mailto: in this approval surface, with no other
// escaping defeating it.

// autolinkShapes are the concrete payloads confirmed to render as live
// <a href=...> links against a real GFM renderer (goldmark with
// extension.GFM) before this fix.
var autolinkShapes = []string{
	"www.evil.example/steal",
	"http://evil.example/steal",
	"https://evil.example/steal",
	"visit www.evil.example now",
	"contact me@evil.example",
}

// assertAutolinkDefeated checks the rendered body against exactly what
// mdText is supposed to have done to shape: the escaped form must be
// present, the raw unescaped payload must not be (that raw form is what a
// GFM extended-autolink scanner anchors on -- a literal "." for the
// www/http(s) domain parser, a literal "@" for the email autolink -- since
// neither survives mdText), and no rendered-link markup ("<a ", "href=")
// must appear regardless. Comparing against mdText(shape) rather than a
// hardcoded string keeps this test anchored to the function under test: it
// fails before the fix (mdText left "." and "@" untouched, so the "raw
// payload absent" check trips) and passes after it.
func assertAutolinkDefeated(t *testing.T, shape, body string) {
	t.Helper()
	want := mdText(shape)
	if !strings.Contains(body, want) {
		t.Errorf("shape %q: body does not contain the escaped form %q:\n%s", shape, want, body)
	}
	if strings.Contains(body, shape) {
		t.Errorf("shape %q: body contains the raw, unescaped payload -- a GFM renderer would autolink it:\n%s", shape, body)
	}
	for _, bad := range []string{"<a ", "href="} {
		if strings.Contains(body, bad) {
			t.Errorf("shape %q: body contains rendered-link markup %q:\n%s", shape, bad, body)
		}
	}
}

func TestBodyDefeatsAutolinkInMetricName(t *testing.T) {
	for i, shape := range autolinkShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: shape, Series: 5, Job: "api"}}
			_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
			assertAutolinkDefeated(t, shape, body)
		})
	}
}

func TestBodyDefeatsAutolinkInJobName(t *testing.T) {
	for i, shape := range autolinkShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: "x", Series: 5, Job: shape}}
			_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
			assertAutolinkDefeated(t, shape, body)
		})
	}
}

func TestBodyDefeatsAutolinkInBlockedEntries(t *testing.T) {
	for i, shape := range autolinkShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: "x", Series: 5, Job: "api"}}
			res := verdict.Result{Blocked: []string{shape}}
			_, body := Body(drops, res, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
			assertAutolinkDefeated(t, shape, body)
		})
	}
}

// TestBodyLeavesOrdinaryNamesReadable pins the other side of the F1 fix:
// escaping "." and "@" must not mangle names that were never an attack.
// node_cpu_seconds_total and "up" contain neither character and must
// round-trip untouched; a UTF-8 name (the Prometheus UTF-8 metric-naming
// scheme allows arbitrary characters, including "." and non-ASCII letters)
// must still be readable after entity-escaping -- "café.total" becomes
// "café&#46;total", which a human reads as "café.total" and no renderer
// autolinks.
func TestBodyLeavesOrdinaryNamesReadable(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"node_cpu_seconds_total", "node_cpu_seconds_total"},
		{"up", "up"},
		{"café.total", "café&#46;total"},
	}
	for _, tc := range cases {
		drops := []Drop{{Metric: tc.name, Series: 5, Job: "api"}}
		_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
		if !strings.Contains(body, tc.want) {
			t.Errorf("ordinary metric name %q: body does not contain %q:\n%s", tc.name, tc.want, body)
		}
		if strings.Contains(body, "www.") || strings.Contains(body, "@evil") {
			t.Errorf("ordinary metric name %q: body unexpectedly contains autolinkable text:\n%s", tc.name, body)
		}
	}
}

// TestBodyPercentageUsesTotalSeriesNotASum pins C2 at the body layer: the
// percentage must be computed against inv.TotalSeries (which callers set
// from promapi.Status.HeadSeries), not any other number. 10 series out of
// a TotalSeries of 1000 is 1.0%; if this ever silently changed to divide
// by something else (e.g. a sum of only the metrics graded) the math
// here would drift and this test would catch it.
func TestBodyPercentageUsesTotalSeriesNotASum(t *testing.T) {
	drops := []Drop{{Metric: "x", Series: 10, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if !strings.Contains(body, "1.0%") {
		t.Errorf("body does not state the percentage computed against TotalSeries:\n%s", body)
	}
}

// TestBodyWarnsWhenTheInventoryWasTruncated pins the PR-body half of C2:
// a truncated inventory must carry a loud warning block naming the
// configured limit and the true metric-name count, so a human approving
// the PR knows the percentage above cannot account for ungraded metrics.
func TestBodyWarnsWhenTheInventoryWasTruncated(t *testing.T) {
	drops := []Drop{{Metric: "x", Series: 10, Job: "api"}}
	inv := inventory.Inventory{TotalSeries: 15777, MetricLimit: 40, NameCount: 1374, Truncated: true}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inv, 0)
	if !strings.Contains(body, "[!WARNING]") {
		t.Errorf("body does not carry a warning block at all:\n%s", body)
	}
	if !strings.Contains(body, "truncated") {
		t.Errorf("body does not say the inventory was truncated:\n%s", body)
	}
	if !strings.Contains(body, "40") || !strings.Contains(body, "1374") {
		t.Errorf("body's truncation warning does not name both the limit and the true metric-name count:\n%s", body)
	}
}

// TestBodyDoesNotWarnAboutTruncationWhenThereIsNone is the negative case.
func TestBodyDoesNotWarnAboutTruncationWhenThereIsNone(t *testing.T) {
	drops := []Drop{{Metric: "x", Series: 10, Job: "api"}}
	inv := inventory.Inventory{TotalSeries: 1000, Truncated: false}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inv, 0)
	if strings.Contains(body, "truncated") {
		t.Errorf("body warns about truncation although the inventory was not truncated:\n%s", body)
	}
}

// --- A metric produced by more than one job gets one emit.Drop per job it
// is dropped from (cmd/jetsam does this deliberately: a relabel rule
// genuinely is added to each of those jobs), and every one of those Drops
// carries that metric's FULL series count from the inventory. Summing
// Series across every Drop therefore counts a shared metric's series once
// per job producing it -- which is how a real run against the public demo
// reported a drops-percentage over 100%: it proposed dropping more series
// than the instance held. The headline total, and the percentage derived
// from it, must count each metric once no matter how many jobs it is
// dropped from.

// TestBodyTotalCountsASharedMetricOnce pins the fix directly: two Drops
// naming the SAME metric under two different jobs, 100 series each, must
// total 100 -- not 200 -- and the stated percentage must match that 100,
// not a doubled figure.
func TestBodyTotalCountsASharedMetricOnce(t *testing.T) {
	drops := []Drop{
		{Metric: "shared_metric", Series: 100, Job: "api"},
		{Metric: "shared_metric", Series: 100, Job: "batch"},
	}
	title, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if !strings.Contains(title, "100 unread series") {
		t.Errorf("title = %q, want it to count shared_metric once (100), not once per job (200)", title)
	}
	if !strings.Contains(body, "Drops 100 series -- 10.0%") {
		t.Errorf("body does not state the deduplicated total and matching percentage:\n%s", body)
	}
	if strings.Contains(body, "200 series") || strings.Contains(body, "20.0%") {
		t.Errorf("body contains the doubled, undeduplicated total or its percentage:\n%s", body)
	}
}

// TestBodyTotalDoesNotCollapseDistinctMetrics is the negative case: two
// Drops naming DIFFERENT metrics, 100 series each, must still sum to 200.
// Deduplication is keyed on metric name, not series count or position, and
// must never merge genuinely distinct metrics into one.
func TestBodyTotalDoesNotCollapseDistinctMetrics(t *testing.T) {
	drops := []Drop{
		{Metric: "metric_a", Series: 100, Job: "api"},
		{Metric: "metric_b", Series: 100, Job: "api"},
	}
	title, _ := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if !strings.Contains(title, "200 unread series") {
		t.Errorf("title = %q, want 200 (two distinct metrics must not be collapsed)", title)
	}
}

// TestBodyTotalCountsAMixOfSharedAndSingleJobMetricsOnce combines both: one
// metric dropped from three jobs (100 series) plus two metrics dropped
// from a single job each (50 and 25 series). The total must be
// 100+50+25=175, counting the three-job metric exactly once, not 100*3+50+25.
func TestBodyTotalCountsAMixOfSharedAndSingleJobMetricsOnce(t *testing.T) {
	drops := []Drop{
		{Metric: "shared", Series: 100, Job: "a"},
		{Metric: "shared", Series: 100, Job: "b"},
		{Metric: "shared", Series: 100, Job: "c"},
		{Metric: "solo_1", Series: 50, Job: "a"},
		{Metric: "solo_2", Series: 25, Job: "b"},
	}
	title, _ := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)
	if !strings.Contains(title, "175 unread series") {
		t.Errorf("title = %q, want 175 (100 once + 50 + 25)", title)
	}
}

// TestBodyPercentageNeverExceedsOneHundred pins the property the auditor's
// real repro violated directly: when inv.TotalSeries is the true head
// series count, the stated percentage of a proposal that drops a subset of
// this Prometheus's own metrics can never read over 100%. A drop
// proposal, by construction, can only name metrics this Prometheus
// actually holds today; a total exceeding TotalSeries is proof the total
// double-counted something, not evidence this Prometheus undercounted
// itself.
func TestBodyPercentageNeverExceedsOneHundred(t *testing.T) {
	drops := []Drop{
		{Metric: "shared", Series: 6000, Job: "a"},
		{Metric: "shared", Series: 6000, Job: "b"},
		{Metric: "shared", Series: 6000, Job: "c"},
		{Metric: "shared", Series: 6000, Job: "d"},
	}
	// TotalSeries is the true head count this metric's 6000 series are
	// already counted within -- not 4x that, which is what an undeduplicated
	// sum across four jobs would need to stay under 100%.
	inv := inventory.Inventory{TotalSeries: 6500}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inv, 0)

	idx := strings.Index(body, "Drops ")
	if idx < 0 {
		t.Fatalf("body does not state a drops percentage at all:\n%s", body)
	}
	line := body[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	var series int
	var pct float64
	if _, err := fmt.Sscanf(line, "Drops %d series -- %f%%", &series, &pct); err != nil {
		t.Fatalf("could not parse the drops line %q: %v", line, err)
	}
	if pct > 100 {
		t.Errorf("percentage = %.1f%%, want <= 100%% (a real drop proposal cannot exceed what this Prometheus holds):\n%s", pct, body)
	}
}

// TestBodyListsASharedMetricUnderEveryJob pins the other half of the
// chosen fix: the per-job tables are unaffected by deduplication. A
// relabel rule genuinely is added to every job a shared metric is dropped
// from, so it must still appear as a row under each job's table, series
// count and all -- only the headline total and percentage are
// deduplicated. The body must also say plainly, near the tables, that a
// shared metric is listed once per job while the total above counts it
// once.
func TestBodyListsASharedMetricUnderEveryJob(t *testing.T) {
	drops := []Drop{
		{Metric: "shared_metric", Series: 100, Job: "api"},
		{Metric: "shared_metric", Series: 100, Job: "batch"},
	}
	_, body := Body(drops, verdict.Result{}, corpus.Corpus{Queries: 200}, inventory.Inventory{TotalSeries: 1000}, 0)

	if !strings.Contains(body, "### job \"api\"") || !strings.Contains(body, "### job \"batch\"") {
		t.Fatalf("body does not carry a table for both jobs:\n%s", body)
	}
	apiTable := body[strings.Index(body, "### job \"api\""):]
	batchTable := body[strings.Index(body, "### job \"batch\""):]
	if !strings.Contains(apiTable, "shared_metric") {
		t.Errorf("shared_metric missing from the \"api\" job table:\n%s", body)
	}
	if !strings.Contains(batchTable, "shared_metric") {
		t.Errorf("shared_metric missing from the \"batch\" job table:\n%s", body)
	}
	if !strings.Contains(body, "produced by more than one job") {
		t.Errorf("body does not state that a shared metric is listed once per job while the total counts it once:\n%s", body)
	}
}

// TestBodyRendersDeceptiveNamesVisibly is the PR-body-level check on what
// safe.Text guarantees per string: nothing that reorders or hides glyphs
// reaches the surface a human approves an irreversible deletion from.
func TestBodyRendersDeceptiveNamesVisibly(t *testing.T) {
	rlo, pop, zwsp := string(rune(0x202E)), string(rune(0x202C)), string(rune(0x200B))
	metric := "cpu_total" + rlo + "latot_ksid" + pop
	job := "api" + zwsp + "x"

	drops := []Drop{{Metric: metric, Series: 10, Job: job}}
	res := verdict.Result{Verdicts: []verdict.Verdict{{Metric: metric, Grade: verdict.GradeUnqueried}}}
	_, body := Body(drops, res, corpus.Corpus{Queries: 5}, inventory.Inventory{TotalSeries: 100}, 0)

	for _, r := range []rune{0x202E, 0x202C, 0x200B} {
		if strings.ContainsRune(body, r) {
			t.Errorf("PR body still carries U+%04X verbatim; it must be rendered as a visible escape", r)
		}
	}
	for _, r := range []rune{0x202E, 0x202C, 0x200B} {
		want := fmt.Sprintf("\\u%04x", r)
		if !strings.Contains(body, want) {
			t.Errorf("PR body does not contain %s; want the code point rendered visibly", want)
		}
	}
}

// TestBodyStatesDashboardsRead pins that the PR body, not just the terminal
// report, says how many dashboards backed the grading -- an approval
// surface for an irreversible deletion should not require reading the
// terminal output to know what evidence existed.
func TestBodyStatesDashboardsRead(t *testing.T) {
	c := corpus.Corpus{Queries: 5, Dashboards: 3, DashboardsConfigured: true, DashboardsReachable: true}
	_, body := Body(nil, verdict.Result{}, c, inventory.Inventory{TotalSeries: 100}, 0)
	if !strings.Contains(body, "3 read from Grafana") {
		t.Errorf("body does not state how many dashboards were read:\n%s", body)
	}
}

// TestBodyWarnsWhenDashboardsUnreachable is the PR-body half of the same
// correction made to report.Scan: Grafana configured but unfetchable must
// be stated plainly, not silently absent from the body a human approves.
func TestBodyWarnsWhenDashboardsUnreachable(t *testing.T) {
	c := corpus.Corpus{Queries: 5, DashboardsConfigured: true, DashboardsReachable: false}
	_, body := Body(nil, verdict.Result{}, c, inventory.Inventory{TotalSeries: 100}, 0)
	if !strings.Contains(body, "could not be fetched") {
		t.Errorf("body does not say dashboard evidence was unavailable:\n%s", body)
	}
}

// TestBodyStatesQueryLogSpan pins that the PR body states the query log's
// actual coverage, the same fact report.Scan prints on the terminal.
func TestBodyStatesQueryLogSpan(t *testing.T) {
	c := corpus.Corpus{Queries: 5, LogRead: true, LogSpan: 400 * time.Hour, LogQualifies: true}
	_, body := Body(nil, verdict.Result{}, c, inventory.Inventory{TotalSeries: 100}, 0)
	if !strings.Contains(body, "400h") {
		t.Errorf("body does not state the query log's span:\n%s", body)
	}
}

// TestBodyBoundsUnqueriedClaimByLogWindow pins the brief's Step 5
// requirement: when a drop rests on "unqueried" grade, the body must state
// that the claim is bounded by the log's window and name the span --
// "unqueried" is full confidence only within that window, not an
// unconditional guarantee.
func TestBodyBoundsUnqueriedClaimByLogWindow(t *testing.T) {
	res := verdict.Result{Verdicts: []verdict.Verdict{
		{Metric: "x", Series: 10, Grade: verdict.GradeUnqueried, Droppable: true},
	}}
	c := corpus.Corpus{Queries: 5, LogRead: true, LogSpan: 720 * time.Hour, LogQualifies: true}
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, res, c, inventory.Inventory{TotalSeries: 1000}, 0)
	if !strings.Contains(body, "720h") {
		t.Errorf("body does not name the log's span when bounding the unqueried claim:\n%s", body)
	}
	if !strings.Contains(body, "unqueried") {
		t.Errorf("body does not mention the unqueried grade it is bounding:\n%s", body)
	}
}
