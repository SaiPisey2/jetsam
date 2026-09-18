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
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Reading is what one pass over the log found.
type Reading struct {
	// Queries is every distinct query string a human or a dashboard ran,
	// sorted. Rule evaluations are excluded -- see Read.
	Queries []string
	// Span is the time between the earliest and latest entry across every
	// matched file. It is what the trust threshold is measured against.
	Span time.Duration
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

type logLine struct {
	Time   string `json:"time"`
	Params struct {
		Query string `json:"query"`
	} `json:"params"`
	HTTPRequest *json.RawMessage `json:"httpRequest"`
}

// tsPattern matches Prometheus' timestamp, whose fractional part varies in
// width and defeats time.RFC3339Nano round-tripping.
var tsPattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?`)

func parseTime(s string) (time.Time, error) {
	m := tsPattern.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
	}
	t, err := time.Parse("2006-01-02T15:04:05", m[1])
	if err != nil {
		return time.Time{}, err
	}
	if m[2] != "" {
		var frac float64
		fmt.Sscanf("0"+m[2], "%g", &frac)
		t = t.Add(time.Duration(frac * float64(time.Second)))
	}
	return t, nil
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
		n, first, last, err := readFile(p, seen)
		if err != nil {
			return nil, err
		}
		entries += n
		if !first.IsZero() && (earliest.IsZero() || first.Before(earliest)) {
			earliest = first
		}
		if !last.IsZero() && last.After(latest) {
			latest = last
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
	}
	return r, nil
}

func readFile(path string, seen map[string]bool) (entries int, first, last time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, first, last, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var src = (interface{ Read([]byte) (int, error) })(f)
	if strings.HasSuffix(path, ".gz") {
		zr, zerr := gzip.NewReader(f)
		if zerr != nil {
			return 0, first, last, fmt.Errorf("open %s: %w", path, zerr)
		}
		defer zr.Close()
		src = zr
	}

	sc := bufio.NewScanner(src)
	// Prometheus writes whole queries onto one line and a dashboard query can
	// be long; the default 64 KiB token limit is not enough.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		entries++
		if !strings.Contains(string(line), readMarker) {
			continue
		}
		var e logLine
		if err := json.Unmarshal(line, &e); err != nil {
			return entries, first, last, fmt.Errorf("%s: malformed query log line: %w", path, err)
		}
		if e.HTTPRequest == nil {
			continue
		}
		t, terr := parseTime(e.Time)
		if terr != nil {
			return entries, first, last, fmt.Errorf("%s: %w", path, terr)
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if t.After(last) {
			last = t
		}
		if q := strings.TrimSpace(e.Params.Query); q != "" {
			seen[q] = true
		}
	}
	if err := sc.Err(); err != nil {
		return entries, first, last, fmt.Errorf("read %s: %w", path, err)
	}
	return entries, first, last, nil
}
