package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// applyWithPatch writes oldContent to <dir>/prometheus.yml, writes diff to
// <dir>/change.patch, and runs `patch -p1` against it, returning the
// resulting file's content. It skips the calling test if no `patch` binary
// is on PATH, rather than failing: the point of these tests is to prove the
// diff jetsam prints is one a real patch tool accepts, and that can only be
// checked where such a tool exists.
func applyWithPatch(t *testing.T, oldContent, diff string) string {
	t.Helper()
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("no `patch` binary on PATH; cannot verify the diff applies")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(target, []byte(oldContent), 0o644); err != nil {
		t.Fatalf("write %s: %v", target, err)
	}
	patchFile := filepath.Join(dir, "change.patch")
	if err := os.WriteFile(patchFile, []byte(diff), 0o644); err != nil {
		t.Fatalf("write %s: %v", patchFile, err)
	}

	cmd := exec.Command("patch", "-p1", "-i", patchFile)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("patch -p1 failed: %v\n%s\n--- diff ---\n%s", err, out, diff)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read patched %s: %v", target, err)
	}
	return string(got)
}

func TestUnifiedDiffOnIdenticalInputsIsEmpty(t *testing.T) {
	yaml := "global:\n  scrape_interval: 15s\n"
	if diff := unifiedDiff("prometheus.yml", yaml, yaml); diff != "" {
		t.Errorf("unifiedDiff(x, x) = %q, want empty", diff)
	}
}

// TestUnifiedDiffFindsATrailingNewlineOnlyChange pins requirement (a): two
// files whose lines are textually identical but which disagree about
// whether the file ends in a newline are NOT the same file, and reporting
// that as "no change" hides a real byte-level difference.
func TestUnifiedDiffFindsATrailingNewlineOnlyChange(t *testing.T) {
	old := "a\nb\n"
	new := "a\nb"

	diff := unifiedDiff("prometheus.yml", old, new)
	if diff == "" {
		t.Fatal("unifiedDiff reported no change between a file with and without a trailing newline")
	}
	if !strings.Contains(diff, noNewlineMarker) {
		t.Errorf("diff does not carry the no-newline-at-EOF marker:\n%s", diff)
	}
	if got := applyWithPatch(t, old, diff); got != new {
		t.Errorf("applying the diff produced %q, want %q", got, new)
	}
}

// TestUnifiedDiffAppliesWhenOldLacksATrailingNewline models editing a
// hand-maintained prometheus.yml that happens to be missing its final
// newline: Render's real output (via yaml.Marshal) always ends in one, so
// this is the realistic direction the mismatch occurs in production.
func TestUnifiedDiffAppliesWhenOldLacksATrailingNewline(t *testing.T) {
	old := "scrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['h']"
	new := old + "\n    metric_relabel_configs:\n      - source_labels: [__name__]\n        regex: foo\n        action: drop\n"

	diff := unifiedDiff("prometheus.yml", old, new)
	if diff == "" {
		t.Fatal("unifiedDiff reported no change")
	}
	if !strings.Contains(diff, noNewlineMarker) {
		t.Errorf("diff does not mark old's missing trailing newline:\n%s", diff)
	}
	// The marker must sit right after the removed copy of old's last line,
	// not after the re-added copy: it describes OLD's file, not new's.
	if i := strings.Index(diff, noNewlineMarker); i < 0 || !strings.Contains(diff[:i], "-      - targets: ['h']") {
		t.Errorf("marker is not attached to the removed (old) copy of the final line:\n%s", diff)
	}
	if got := applyWithPatch(t, old, diff); got != new {
		t.Errorf("applying the diff produced %q, want %q", got, new)
	}
}

// TestUnifiedDiffAppliesWhenNewLacksATrailingNewline is the mirror case: a
// real change whose result happens to have no trailing newline.
func TestUnifiedDiffAppliesWhenNewLacksATrailingNewline(t *testing.T) {
	old := "a\nb\n"
	new := "a\nb\nc"

	diff := unifiedDiff("prometheus.yml", old, new)
	if diff == "" {
		t.Fatal("unifiedDiff reported no change")
	}
	if !strings.HasSuffix(strings.TrimRight(diff, "\n"), noNewlineMarker) {
		t.Errorf("diff does not end with the no-newline-at-EOF marker for the new file:\n%s", diff)
	}
	if got := applyWithPatch(t, old, diff); got != new {
		t.Errorf("applying the diff produced %q, want %q", got, new)
	}
}

// TestUnifiedDiffHunkCountsIgnoreTheMarkerLine guards against the marker
// line being miscounted as part of the hunk's line counts in its @@
// header -- it annotates the preceding line, it is not a line of its own.
func TestUnifiedDiffHunkCountsIgnoreTheMarkerLine(t *testing.T) {
	old := "a\nb\n"
	new := "a\nb"

	diff := unifiedDiff("prometheus.yml", old, new)
	if !strings.Contains(diff, "@@ -1,2 +1,2 @@") {
		t.Errorf("hunk header counts were thrown off by the marker line:\n%s", diff)
	}
}
