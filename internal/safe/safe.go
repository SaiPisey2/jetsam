// Package safe renders remote-sourced strings for output a human or a
// Markdown renderer trusts to be well-formed.
package safe

import (
	"strconv"
	"strings"
	"unicode"
)

// Text renders s safely for output that a human reads. Metric and rule
// names come from a remote Prometheus and are not trusted: a name
// carrying ANSI escapes can clear or forge lines in a terminal, and one
// carrying newlines or pipes can break the structure of a Markdown table
// in a generated pull request. Control characters are rendered visibly
// rather than executed, so what is shown is what the remote actually
// sent.
func Text(s string) string {
	var hasControl bool
	for _, r := range s {
		if unicode.IsControl(r) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
			continue
		}
		// strconv.QuoteRune always escapes control runes into a visible
		// form such as '\x1b', '\r' or '\n'; strip the surrounding rune
		// quotes to get just the escape sequence.
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}
