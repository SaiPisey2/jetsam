package emit

import (
	"context"
	"testing"

	"github.com/SaiPisey2/jetsam/internal/forge"
)

func TestApplyIsIdempotent(t *testing.T) {
	p := forge.NewFakeProvider()
	ctx := context.Background()

	first, err := Apply(ctx, p, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	second, err := Apply(ctx, p, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	// Re-running must find the open PR, not stack a second one on the same
	// branch. A bot that opens a duplicate every run gets blocked at the org
	// level.
	if first.Number != second.Number {
		t.Errorf("opened a second PR (%d then %d), want the existing one returned", first.Number, second.Number)
	}
}

// TestApplyRefusesAStaleBase pins the safety property that matters most:
// Apply must pass oldYAML -- the content it actually rendered newYAML
// from -- as FileChange.BaseContent, never newYAML, an empty string, or a
// fresh re-read. Here the remote (BaseFiles, what FakeProvider treats as
// the base branch's current content) has moved on to something neither
// oldYAML nor newYAML: a human edit Apply never saw. CommitFiles must
// refuse, and nothing must land on the branch.
func TestApplyRefusesAStaleBase(t *testing.T) {
	p := forge.NewFakeProvider()
	ctx := context.Background()
	branch := forge.BranchName("new")
	p.BaseFiles["o/r/prometheus.yml"] = "someone else's edit, made after this checkout"

	_, err := Apply(ctx, p, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err == nil {
		t.Fatal("Apply succeeded against a moved base, want a refusal")
	}
	if _, committed := p.Files["o/r/"+branch+"/prometheus.yml"]; committed {
		t.Error("Apply committed onto the branch despite the stale base, want nothing written")
	}
	if _, opened := p.PRs["o/r/"+branch]; opened {
		t.Error("Apply opened a PR despite the stale base, want nothing opened")
	}
}

// TestApplyPassesOldYAMLNotNewYAMLAsBaseContent is the direct version of
// the same property. If Apply passed newYAML (or "") as BaseContent
// instead of oldYAML, this commit would be accepted by a remote that
// already moved on to newYAML by some other means -- exactly the silent
// overwrite BaseContent exists to prevent. Asserting the refusal here
// means Apply is provably using oldYAML, and only oldYAML, as the
// precondition.
func TestApplyPassesOldYAMLNotNewYAMLAsBaseContent(t *testing.T) {
	p := forge.NewFakeProvider()
	ctx := context.Background()
	p.BaseFiles["o/r/prometheus.yml"] = "new" // remote already holds newYAML, not oldYAML

	if _, err := Apply(ctx, p, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b"); err == nil {
		t.Fatal("Apply succeeded when the remote held newYAML instead of oldYAML as base, " +
			"want a refusal (this would only pass if Apply used newYAML as BaseContent)")
	}
}
