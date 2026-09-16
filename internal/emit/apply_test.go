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

// countingProvider wraps a forge.Provider and counts calls to the three
// methods that write anything: EnsureBranch, CommitFiles, OpenPR. It exists
// so a test can assert those methods were never invoked at all -- not just
// that the end result looked right -- which is the only way to actually
// catch Apply recreating a branch (or worse) for a PR it should have left
// alone.
type countingProvider struct {
	forge.Provider
	ensureBranchCalls int
	commitFilesCalls  int
	openPRCalls       int
}

func (c *countingProvider) EnsureBranch(ctx context.Context, owner, repo, branch, base string) error {
	c.ensureBranchCalls++
	return c.Provider.EnsureBranch(ctx, owner, repo, branch, base)
}

func (c *countingProvider) CommitFiles(ctx context.Context, owner, repo, base, branch string, changes []forge.FileChange) error {
	c.commitFilesCalls++
	return c.Provider.CommitFiles(ctx, owner, repo, base, branch, changes)
}

func (c *countingProvider) OpenPR(ctx context.Context, spec forge.PRSpec) (*forge.PullRequest, error) {
	c.openPRCalls++
	return c.Provider.OpenPR(ctx, spec)
}

// TestApplyOpensAPRWhenNoneExists is the baseline for the four
// existing-PR-state tests below: with no PR on this branch at all, Apply
// must create the branch, commit, and open a PR -- each exactly once.
func TestApplyOpensAPRWhenNoneExists(t *testing.T) {
	cp := &countingProvider{Provider: forge.NewFakeProvider()}
	ctx := context.Background()

	pr, err := Apply(ctx, cp, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if pr.State != "open" {
		t.Errorf("State = %q, want %q", pr.State, "open")
	}
	if cp.ensureBranchCalls != 1 || cp.commitFilesCalls != 1 || cp.openPRCalls != 1 {
		t.Errorf("EnsureBranch=%d CommitFiles=%d OpenPR=%d, want 1 each",
			cp.ensureBranchCalls, cp.commitFilesCalls, cp.openPRCalls)
	}
}

// TestApplyReturnsAnOpenPRWithoutTouchingTheForge is the ordinary
// idempotence case, but asserted the strong way: not merely that the PR
// number comes back unchanged (TestApplyIsIdempotent already covers that)
// but that EnsureBranch, CommitFiles and OpenPR are never called a second
// time at all.
func TestApplyReturnsAnOpenPRWithoutTouchingTheForge(t *testing.T) {
	base := forge.NewFakeProvider()
	ctx := context.Background()
	first, err := Apply(ctx, base, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	cp := &countingProvider{Provider: base}
	got, err := Apply(ctx, cp, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got.Number != first.Number || got.State != "open" {
		t.Errorf("got %+v, want the existing open PR %+v", got, first)
	}
	if cp.ensureBranchCalls != 0 || cp.commitFilesCalls != 0 || cp.openPRCalls != 0 {
		t.Errorf("EnsureBranch=%d CommitFiles=%d OpenPR=%d, want 0 each for an already-open PR",
			cp.ensureBranchCalls, cp.commitFilesCalls, cp.openPRCalls)
	}
}

// TestApplyReturnsAMergedPRWithoutTouchingTheForge: a merged PR means the
// change already landed. Re-running Apply must report it, not restate it.
func TestApplyReturnsAMergedPRWithoutTouchingTheForge(t *testing.T) {
	base := forge.NewFakeProvider()
	ctx := context.Background()
	first, err := Apply(ctx, base, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	branch := forge.BranchName("new")
	base.PRs["o/r/"+branch].State = "merged"

	cp := &countingProvider{Provider: base}
	got, err := Apply(ctx, cp, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got.Number != first.Number || got.State != "merged" {
		t.Errorf("got %+v, want the existing merged PR (number %d, state merged)", got, first.Number)
	}
	if cp.ensureBranchCalls != 0 || cp.commitFilesCalls != 0 || cp.openPRCalls != 0 {
		t.Errorf("EnsureBranch=%d CommitFiles=%d OpenPR=%d, want 0 each for a merged PR",
			cp.ensureBranchCalls, cp.commitFilesCalls, cp.openPRCalls)
	}
}

// TestApplyReturnsAClosedPRWithoutTouchingTheForge is the regression test
// for the Critical finding: a closed PR is a decision a human already
// made, and Apply must return it as-is rather than treating the absence of
// an OPEN PR as an absence of any PR at all.
//
// The branch and its committed file are deleted here too, reproducing
// GitHub's own default behavior of deleting a PR's branch when it is
// closed -- exactly the scenario that let the bug through empirically: with
// the branch gone, the old code's fallthrough to EnsureBranch silently
// recreated it from base, and CommitFiles/OpenPR happily proceeded to open
// a second PR carrying the exact content a human had already rejected.
func TestApplyReturnsAClosedPRWithoutTouchingTheForge(t *testing.T) {
	base := forge.NewFakeProvider()
	ctx := context.Background()
	first, err := Apply(ctx, base, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	branch := forge.BranchName("new")
	base.PRs["o/r/"+branch].State = "closed"
	delete(base.Branches, "o/r/"+branch)
	delete(base.Files, "o/r/"+branch+"/prometheus.yml")

	cp := &countingProvider{Provider: base}
	got, err := Apply(ctx, cp, "o", "r", "main", "prometheus.yml", "old", "new", "t", "b")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got.Number != first.Number || got.State != "closed" {
		t.Errorf("got %+v, want the existing closed PR (number %d, state closed) returned untouched", got, first.Number)
	}
	if cp.ensureBranchCalls != 0 {
		t.Errorf("EnsureBranch called %d time(s), want 0: a closed PR's branch must not be recreated "+
			"(this is exactly how a rejected proposal gets silently reopened)", cp.ensureBranchCalls)
	}
	if cp.commitFilesCalls != 0 {
		t.Errorf("CommitFiles called %d time(s), want 0: nothing should be committed onto a closed PR's branch", cp.commitFilesCalls)
	}
	if cp.openPRCalls != 0 {
		t.Errorf("OpenPR called %d time(s), want 0: a closed PR must not get a second, duplicate PR", cp.openPRCalls)
	}
}
