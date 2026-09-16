package emit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

func TestBodyWarnsThatHistoryIsNotRecoverable(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)
	// Reverting the commit restores collection but not the history that was
	// never written. It is the one mistake a reviewer cannot undo, so it
	// belongs near the top.
	if !strings.Contains(body, "cannot be recovered") {
		t.Errorf("body does not warn about unrecoverable history:\n%s", body)
	}
}

func TestBodyOmitsMoneyWithoutARate(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)
	if strings.Contains(body, "$") {
		t.Errorf("body invented a cost with no rate configured:\n%s", body)
	}
}

func TestBodyShowsTheArithmeticWhenGivenARate(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 1000, Job: "api"}}, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 10000}, 0.0008)
	if !strings.Contains(body, "0.0008") {
		t.Errorf("body states a cost without the rate it used:\n%s", body)
	}
}

func TestBodyCountsWhatWasDeclined(t *testing.T) {
	res := verdict.Result{Blocked: []string{"rule g/Broken: parse query"}}
	_, body := Body(nil, res, 200, inventory.Inventory{TotalSeries: 1000}, 0)
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
	_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)

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
	_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)

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
	_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)

	if strings.ContainsRune(body, 0x1b) {
		t.Errorf("raw ESC byte survived into the body:\n%q", body)
	}
	if !strings.Contains(body, `\x1b`) {
		t.Errorf("body does not render the ANSI escape visibly:\n%s", body)
	}
}

func TestBodyRendersNewlineInMetricNameVisibly(t *testing.T) {
	drops := []Drop{{Metric: "up\ninjected", Series: 5, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)

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
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, res, 200, inventory.Inventory{TotalSeries: 1000}, 0)

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
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, res, 200, inventory.Inventory{TotalSeries: 1000}, 0)
	if strings.Contains(body, "-include-unreferenced") {
		t.Errorf("body warns about -include-unreferenced although every drop rests on unqueried grade:\n%s", body)
	}
}

func TestBodyEscapesBlockedEntries(t *testing.T) {
	res := verdict.Result{Blocked: []string{"rule g/Broken\x1b[31m: parse query"}}
	_, body := Body(nil, res, 200, inventory.Inventory{TotalSeries: 1000}, 0)
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
			_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)
			assertNoLiveMarkupOrHTML(t, shape, body)
		})
	}
}

func TestBodyNeutralisesMarkdownAndHTMLInJobName(t *testing.T) {
	for i, shape := range mdInjectionShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: "x", Series: 5, Job: shape}}
			_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)
			assertNoLiveMarkupOrHTML(t, shape, body)
		})
	}
}

func TestBodyNeutralisesMarkdownAndHTMLInBlockedEntries(t *testing.T) {
	for i, shape := range mdInjectionShapes {
		t.Run(subtestName(i, shape), func(t *testing.T) {
			drops := []Drop{{Metric: "x", Series: 5, Job: "api"}}
			res := verdict.Result{Blocked: []string{shape}}
			_, body := Body(drops, res, 200, inventory.Inventory{TotalSeries: 1000}, 0)
			assertNoLiveMarkupOrHTML(t, shape, body)
		})
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
	_, body := Body(drops, verdict.Result{}, 200, inventory.Inventory{TotalSeries: 1000}, 0)
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
	_, body := Body(drops, verdict.Result{}, 200, inv, 0)
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
	_, body := Body(drops, verdict.Result{}, 200, inv, 0)
	if strings.Contains(body, "truncated") {
		t.Errorf("body warns about truncation although the inventory was not truncated:\n%s", body)
	}
}
