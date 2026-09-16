package emit

import (
	"strings"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/verdict"
)

func TestBodyWarnsThatHistoryIsNotRecoverable(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, verdict.Result{}, 200, 1000, 0)
	// Reverting the commit restores collection but not the history that was
	// never written. It is the one mistake a reviewer cannot undo, so it
	// belongs near the top.
	if !strings.Contains(body, "cannot be recovered") {
		t.Errorf("body does not warn about unrecoverable history:\n%s", body)
	}
}

func TestBodyOmitsMoneyWithoutARate(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 10, Job: "api"}}, verdict.Result{}, 200, 1000, 0)
	if strings.Contains(body, "$") {
		t.Errorf("body invented a cost with no rate configured:\n%s", body)
	}
}

func TestBodyShowsTheArithmeticWhenGivenARate(t *testing.T) {
	_, body := Body([]Drop{{Metric: "x", Series: 1000, Job: "api"}}, verdict.Result{}, 200, 10000, 0.0008)
	if !strings.Contains(body, "0.0008") {
		t.Errorf("body states a cost without the rate it used:\n%s", body)
	}
}

func TestBodyCountsWhatWasDeclined(t *testing.T) {
	res := verdict.Result{Blocked: []string{"rule g/Broken: parse query"}}
	_, body := Body(nil, res, 200, 1000, 0)
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
	_, body := Body(drops, verdict.Result{}, 200, 1000, 0)

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
	_, body := Body(drops, verdict.Result{}, 200, 1000, 0)

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
	_, body := Body(drops, verdict.Result{}, 200, 1000, 0)

	if strings.ContainsRune(body, 0x1b) {
		t.Errorf("raw ESC byte survived into the body:\n%q", body)
	}
	if !strings.Contains(body, `\x1b`) {
		t.Errorf("body does not render the ANSI escape visibly:\n%s", body)
	}
}

func TestBodyRendersNewlineInMetricNameVisibly(t *testing.T) {
	drops := []Drop{{Metric: "up\ninjected", Series: 5, Job: "api"}}
	_, body := Body(drops, verdict.Result{}, 200, 1000, 0)

	if got, want := countTableRows(body), 3; got != want {
		t.Errorf("table rows = %d, want %d (a raw newline forged a row):\n%s", got, want, body)
	}
	if !strings.Contains(body, `\n`) {
		t.Errorf("body does not render the embedded newline visibly:\n%s", body)
	}
}

func TestBodyEscapesBlockedEntries(t *testing.T) {
	res := verdict.Result{Blocked: []string{"rule g/Broken\x1b[31m: parse query"}}
	_, body := Body(nil, res, 200, 1000, 0)
	if strings.ContainsRune(body, 0x1b) {
		t.Errorf("raw ESC byte survived from a blocked entry into the body:\n%q", body)
	}
}
