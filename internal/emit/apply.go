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

	if existing, err := p.FindPR(ctx, owner, repo, base, branch); err != nil {
		return nil, err
	} else if existing != nil && existing.State == "open" {
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
