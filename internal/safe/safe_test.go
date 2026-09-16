package safe

import "testing"

func TestText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "ANSI escape is rendered visibly",
			in:   "up\x1b[2K\x1b[1;31mFAKE\x1b[0m",
			want: `up\x1b[2K\x1b[1;31mFAKE\x1b[0m`,
		},
		{
			name: "CRLF is rendered visibly",
			in:   "up\r\nnode_cpu_seconds_total",
			want: `up\r\nnode_cpu_seconds_total`,
		},
		{
			name: "tab is rendered visibly",
			in:   "up\ttab",
			want: `up\ttab`,
		},
		{
			name: "plain ASCII is unchanged",
			in:   "node_cpu_seconds_total",
			want: "node_cpu_seconds_total",
		},
		{
			name: "UTF-8 is unchanged",
			in:   "café_total",
			want: "café_total",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
