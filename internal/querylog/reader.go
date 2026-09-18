// Package querylog reads Prometheus' query log to learn what was actually
// read, and over what period.
//
// This is the only evidence jetsam has that is NEGATIVE: the absence of a
// metric from the log, across a long enough window, is what licenses the
// claim that nobody queried it. Everything here is shaped by that. A log that
// cannot be fully read is an error, never an empty one, because an empty log
// would license dropping everything.
package querylog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Reading is what one pass over the log found.
type Reading struct {
	// Queries is every distinct query string a human or a dashboard ran,
	// sorted. Rule evaluations are excluded -- see Read.
	Queries []string
	// Span is the time between the log's earliest and latest entry, of
	// EITHER kind, across every matched file. It is the log's coverage --
	// how long it has actually been recording -- not the span of reads
	// within it. A rule evaluation is not a read, but it is just as good
	// evidence the log was running and would have caught a real read had one
	// happened. Restricting this to read timestamps would report Span=0 for
	// the ordinary case of a log with zero reads, which is the case the
	// trust threshold exists to evaluate: a real log is close to 100% rule
	// evaluations, so that ordinary case is the common one, not an edge
	// case.
	Span time.Duration
	// End is the timestamp of the latest entry across every matched file.
	// Span says how much time the log covers; End says when that coverage
	// stopped. A glob that matches only last year's rotated archives has a
	// perfectly good span and tells you nothing about today, and nothing in
	// jetsam refuses on that -- so End exists to make the gap visible to
	// whoever is being asked to approve a deletion.
	End time.Time
	// Files is how many files the glob matched, and Entries how many lines
	// were examined. Both are reported so an operator can tell a log that
	// covers a month from one that was rotated away yesterday.
	Files   int
	Entries int
}

// readMarker is the cheap prefilter. Prometheus writes a JSON object per
// line; a rule evaluation carries a "ruleGroup" key and a real read carries
// "httpRequest".
//
// The filter selects POSITIVELY on httpRequest rather than rejecting on
// ruleGroup, and that asymmetry is deliberate. A read's own query text can
// contain the literal string "ruleGroup" -- a metric named
// prometheus_rule_group_last_duration_seconds, say -- and false-rejecting a
// genuine read is the dangerous direction: it makes a metric somebody reads
// look unread, which is how a drop gets proposed for data in use.
//
// The prefilter matters for volume, not correctness. Measured on the fixture:
// 92,461 entries over six hours, 100% rule evaluations, projecting to 8.8 GB
// across a 30-day window on a stack with only 64 rules. Decoding every line
// as JSON would dominate the runtime; a substring check does not.
const readMarker = `"httpRequest"`

// logLine decodes only the fields Read still needs from a matched line.
// Timestamps are no longer decoded here -- see extractTime -- because Span
// now needs every line's timestamp, not just a read's, and extracting it
// without a JSON decode is what keeps that cheap.
type logLine struct {
	Params struct {
		Query string `json:"query"`
	} `json:"params"`
	HTTPRequest *json.RawMessage `json:"httpRequest"`
}

// parseTime parses Prometheus' timestamp, including its timezone offset.
// Prometheus writes it via Go's own time formatting, so the fractional part
// never exceeds nanosecond precision and time.RFC3339Nano's variable-width
// ".999999999" fractional element parses it exactly -- verified against
// 8,823 distinct timestamps drawn from the real fixture log (all "Z", widths
// from 0 to 9 digits) plus the two variable-width examples on record
// (".919751712" and ".42401"), all of which round-trip through it cleanly.
//
// The offset is not discarded: a log that ever reported a non-UTC offset and
// had it silently read as UTC would be wrong by hours in a way nobody would
// notice, so it is applied via the standard library rather than assumed
// away.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("unrecognised timestamp %q: %w", s, err)
	}
	return t, nil
}

// timeFieldPrefix is how every Prometheus query-log line begins, whichever
// kind it is.
const timeFieldPrefix = `{"time":"`

// extractTime pulls the raw timestamp out of a line without decoding it as
// JSON, so that a rule evaluation's timestamp -- which Read now needs for
// Span -- costs exactly what a real read's already cost, not a second
// JSON-decode pass over the whole file. It returns ok=false for a line whose
// own time field is missing or truncated, which readFile treats as an error:
// a rule-shaped line never reaches json.Unmarshal (see readMarker), so this
// is the one check that runs over every line regardless of kind, and it is
// what stops a corrupted or truncated line from being silently absorbed as
// an unremarkable entry that also happens to feed Span's bounds.
func extractTime(line []byte) (string, bool) {
	if !bytes.HasPrefix(line, []byte(timeFieldPrefix)) {
		return "", false
	}
	rest := line[len(timeFieldPrefix):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return string(rest[:end]), true
}

// Read streams every file the glob matches and reports what was read.
//
// An empty glob returns (nil, nil): no log is configured, which is a
// different thing from a log with nothing in it. A glob that matches no file
// is an error -- a configured path that finds nothing is a mistake worth
// reporting, not evidence that nobody queried anything.
func Read(glob string) (*Reading, error) {
	if strings.TrimSpace(glob) == "" {
		return nil, nil
	}
	paths, err := filepath.Glob(glob)
	if err != nil {
		return nil, fmt.Errorf("query log path %q: %w", glob, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("query log path %q matched no file", glob)
	}
	sort.Strings(paths)

	seen := map[string]bool{}
	var earliest, latest time.Time
	entries := 0

	for _, p := range paths {
		n, fileEarliest, fileLatest, err := readFile(p, seen)
		if err != nil {
			return nil, err
		}
		entries += n
		if !fileEarliest.IsZero() && (earliest.IsZero() || fileEarliest.Before(earliest)) {
			earliest = fileEarliest
		}
		if !fileLatest.IsZero() && fileLatest.After(latest) {
			latest = fileLatest
		}
	}

	out := make([]string, 0, len(seen))
	for q := range seen {
		out = append(out, q)
	}
	sort.Strings(out)

	r := &Reading{Queries: out, Files: len(paths), Entries: entries}
	if !earliest.IsZero() && !latest.IsZero() {
		r.Span = latest.Sub(earliest)
		r.End = latest
	}
	return r, nil
}

func readFile(path string, seen map[string]bool) (entries int, earliest, latest time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, earliest, latest, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var src = (interface{ Read([]byte) (int, error) })(f)
	if strings.HasSuffix(path, ".gz") {
		zr, zerr := gzip.NewReader(f)
		if zerr != nil {
			return 0, earliest, latest, fmt.Errorf("open %s: %w", path, zerr)
		}
		defer zr.Close()
		src = zr
	}

	sc := bufio.NewScanner(src)
	// Prometheus writes whole queries onto one line and a dashboard query can
	// be long; the default 64 KiB token limit is not enough.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		entries++

		// Every non-empty line, rule evaluation or real read, must carry a
		// parseable timestamp. This is the one check that runs over every
		// line regardless of kind -- a rule-shaped line never reaches
		// json.Unmarshal below -- so it is what stops a truncated or
		// corrupted line from being silently counted as an unremarkable
		// entry. A log that cannot be fully read is an error, never an
		// entry quietly dropped.
		raw, ok := extractTime(line)
		if !ok {
			return entries, earliest, latest, fmt.Errorf("%s: line %d: no parseable time field", path, lineNum)
		}
		t, terr := parseTime(raw)
		if terr != nil {
			return entries, earliest, latest, fmt.Errorf("%s: line %d: %w", path, lineNum, terr)
		}
		// The bound is a running min/max over every entry's actual parsed
		// time, not the physically first and last line. Nothing here should
		// have to trust that a log file is chronological -- a rotation
		// artifact, a clock correction, or a manual splice could put an
		// out-of-order line anywhere -- and an inflated span is the
		// dangerous direction: it can push a log past the trust threshold
		// and license a drop it should not.
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
		if t.After(latest) {
			latest = t
		}

		if !strings.Contains(string(line), readMarker) {
			continue
		}
		var e logLine
		if err := json.Unmarshal(line, &e); err != nil {
			return entries, earliest, latest, fmt.Errorf("%s: line %d: malformed query log line: %w", path, lineNum, err)
		}
		if e.HTTPRequest == nil {
			continue
		}
		if q := strings.TrimSpace(e.Params.Query); q != "" {
			seen[q] = true
		}
	}
	if err := sc.Err(); err != nil {
		return entries, earliest, latest, fmt.Errorf("read %s: %w", path, err)
	}
	return entries, earliest, latest, nil
}
