package emit

import (
	"context"

	"github.com/SaiPisey2/jetsam/internal/forge"
)

// Apply opens (or finds) the pull request carrying newYAML.
//
// The branch name is derived from newYAML, so an unchanged proposal reuses
// its branch and finds its own open PR instead of opening another. oldYAML
// is passed as FileChange.BaseContent: the provider must refuse the commit
// if the remote file has moved since, because Content is a whole file and
// writing it blind would silently revert whatever else changed there.
//
// Apply never sees a credential. Whatever p is -- the real GitHubProvider
// or forge.NewFakeProvider() in a test -- authentication is p's own
// business, decided before Apply is ever called; nothing here reads an
// environment variable, a flag, or a config field to get one.
func Apply(ctx context.Context, p forge.Provider, owner, repo, base, path, oldYAML, newYAML, title, body string) (*forge.PullRequest, error) {
	branch := forge.BranchName(newYAML)

	existing, err := p.FindPR(ctx, owner, repo, base, branch)
	if err != nil {
		return nil, err
	}
	// Any PR FindPR returns for this branch -- open, closed, or merged --
	// is returned as-is, without touching the branch. FindPR's own doc
	// comment explains why closed and merged PRs are returned at all
	// rather than filtered out: a closed PR is a decision (a maintainer
	// looked at this exact proposal and declined it) and a merged one
	// means the change already landed. Recreating the branch and opening
	// another PR in either case would silently reopen a rejected proposal
	// -- or restate a merged one -- on every subsequent run, which is
	// exactly the failure mode that gets a bot blocked at the org level.
	//
	// This needs no escape hatch to re-propose after a rejection: branch
	// is forge.BranchName(newYAML), deterministic in the proposal's own
	// content, so a genuinely CHANGED proposal already gets a different
	// branch and therefore a fresh PR here. The only way to get a new PR
	// after "no" is for the set of metrics being dropped to actually
	// change -- never by resubmitting the identical proposal. Do not
	// "fix" this by reopening closed PRs.
	if existing != nil {
		return existing, nil
	}
	if err := p.EnsureBranch(ctx, owner, repo, branch, base); err != nil {
		return nil, err
	}
	if err := p.CommitFiles(ctx, owner, repo, base, branch, []forge.FileChange{{
		Path:        path,
		Content:     newYAML,
		BaseContent: oldYAML,
		Message:     title,
	}}); err != nil {
		return nil, err
	}
	return p.OpenPR(ctx, forge.PRSpec{Owner: owner, Repo: repo, Base: base, Branch: branch, Title: title, Body: body})
}
