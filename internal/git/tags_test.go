package git

import (
	"errors"
	"testing"

	"agent-overflow/internal/testutil"
)

// A tag moved upstream is updated, a new upstream tag arrives, and a tag
// only this repository has survives. Without --force the moved tag fails
// the whole fetch and stores nothing.
// Pruning configured in any of its forms never reaches the refresh.
func TestRefreshTagsForcesMovedTagsAndKeepsLocalOnes(t *testing.T) {
	t.Parallel()
	for name, config := range map[string][]string{
		"no config":           nil,
		"fetch":               {"fetch.prune", "fetch.pruneTags"},
		"remote.origin":       {"remote.origin.prune", "remote.origin.pruneTags"},
		"fetch prune, remote": {"fetch.prune", "remote.origin.pruneTags"},
		"remote prune, fetch": {"remote.origin.prune", "fetch.pruneTags"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testRefreshTagsKeepsLocalOnes(t, config)
		})
	}
}

func testRefreshTagsKeepsLocalOnes(t *testing.T, config []string) {
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	for _, key := range config {
		testutil.RunGit(t, repo, "config", key, "true")
	}
	testutil.RunGit(t, repo, "tag", "moved")
	testutil.RunGit(t, repo, "push", "-q", "origin", "moved")
	testutil.RunGit(t, repo, "tag", "local-only")

	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", "-q", bare, ".")
	testutil.RunGit(t, sibling, "-c", "user.email=s@example.com", "-c", "user.name=S", "commit", "-q", "--allow-empty", "-m", "upstream")
	testutil.RunGit(t, sibling, "tag", "-f", "moved")
	testutil.RunGit(t, sibling, "tag", "fresh")
	testutil.RunGit(t, sibling, "push", "-q", "-f", "origin", "moved", "fresh")
	upstream := revParse(t, sibling, "HEAD")

	core := NewCore()
	if err := core.RefreshTags(repo); err != nil {
		t.Fatalf("RefreshTags: %v", err)
	}
	if got := revParse(t, repo, "refs/tags/moved"); got != upstream {
		t.Fatalf("moved tag at %s, want upstream %s", got, upstream)
	}
	for _, name := range []string{"fresh", "local-only"} {
		if exists, err := core.TagExists(repo, name); err != nil || !exists {
			t.Fatalf("tag %s exists = %v, %v; want present", name, exists, err)
		}
	}
}

func TestRefreshTagsWithoutOriginFails(t *testing.T) {
	t.Parallel()
	if err := NewCore().RefreshTags(initGitRepo(t)); !errors.Is(err, ErrNoOriginRemote) {
		t.Fatalf("RefreshTags without origin = %v, want ErrNoOriginRemote", err)
	}
}
