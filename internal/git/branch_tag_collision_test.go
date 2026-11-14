package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// Every operation that names a branch to git reads the branch, never a tag
// of the same name: git resolves a bare name to refs/tags/ before
// refs/heads/, and a refspec that matches both is refused.

// collisionRepo is a clone on main whose branch dup has one pushed and one
// unpushed commit over main, tracking origin/dup, with a local tag dup at
// main's commit.
func sharedNameRepo(t *testing.T) (repo, bare, root string) {
	t.Helper()
	repo, bare = testutil.InitGitRepoWithOrigin(t)
	root = revParse(t, repo, "refs/heads/main")
	testutil.RunGit(t, repo, "checkout", "-q", "-b", "dup")
	testutil.RunGit(t, repo, "commit", "-q", "--allow-empty", "-m", "pushed")
	testutil.RunGit(t, repo, "push", "-q", "-u", "origin", "dup")
	testutil.RunGit(t, repo, "commit", "-q", "--allow-empty", "-m", "unpushed")
	testutil.RunGit(t, repo, "tag", "dup", root)
	testutil.RunGit(t, repo, "checkout", "-q", "main")
	return repo, bare, root
}

func TestCountsReadTheBranchNotATagOfItsName(t *testing.T) {
	t.Parallel()
	repo, _, _ := sharedNameRepo(t)
	core := NewCore()

	if upstream, ok := core.upstreamFor(repo, "dup"); !ok || upstream != "origin/dup" {
		t.Errorf("upstreamFor(dup) = %q, %v; want origin/dup", upstream, ok)
	}
	if count, hasUpstream, err := core.CountUnpushedCommits(repo, "dup"); err != nil || !hasUpstream || count != 1 {
		t.Errorf("CountUnpushedCommits(dup) = %d, %v, %v; want 1 unpushed", count, hasUpstream, err)
	}
	if ahead, err := core.CountCommitsAhead(repo, "dup", "main"); err != nil || ahead != 2 {
		t.Errorf("CountCommitsAhead(dup, main) = %d, %v; want 2", ahead, err)
	}
	testutil.RunGit(t, repo, "tag", "main", revParse(t, repo, "refs/heads/dup"))
	if ahead, err := core.CountCommitsAhead(repo, "dup", "main"); err != nil || ahead != 2 {
		t.Errorf("CountCommitsAhead(dup, main) with a tag main at dup = %d, %v; want 2", ahead, err)
	}
}

func TestFirstPushOfABranchThatSharesATagsName(t *testing.T) {
	t.Parallel()
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	root := revParse(t, repo, "refs/heads/main")
	testutil.RunGit(t, repo, "checkout", "-q", "-b", "fresh")
	testutil.RunGit(t, repo, "commit", "-q", "--allow-empty", "-m", "fresh work")
	testutil.RunGit(t, repo, "tag", "fresh", root)
	tip := revParse(t, repo, "refs/heads/fresh")

	core := NewCore()
	if err := core.Push(repo); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := revParse(t, bare, "refs/heads/fresh"); got != tip {
		t.Errorf("origin's fresh = %s, want %s", got, tip)
	}
	if upstream, ok := core.upstreamFor(repo, "fresh"); !ok || upstream != "origin/fresh" {
		t.Errorf("upstream after the first push = %q, %v; want origin/fresh", upstream, ok)
	}
	if testutil.RunGitAllowError(bare, "rev-parse", "--verify", "--quiet", "refs/tags/fresh") == nil {
		t.Error("the first push published the tag")
	}
}

func TestSyncBranchUpdatesTheBranchNotATagOfItsName(t *testing.T) {
	t.Parallel()
	repo, bare, root := sharedNameRepo(t)
	testutil.RunGit(t, repo, "branch", "-q", "behind", "refs/heads/dup")
	testutil.RunGit(t, repo, "push", "-q", "-u", "origin", "behind")
	testutil.RunGit(t, repo, "tag", "behind", root)
	testutil.RunGit(t, repo, "push", "-q", "origin", "refs/tags/behind")
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", "-q", "-b", "behind", bare, ".")
	testutil.RunGit(t, sibling, "-c", "user.email=s@example.com", "-c", "user.name=S", "commit", "-q", "--allow-empty", "-m", "upstream")
	testutil.RunGit(t, sibling, "push", "-q", "origin", "HEAD:refs/heads/behind")
	upstreamTip := revParse(t, sibling, "HEAD")

	if err := NewCore().SyncBranch(repo, "behind"); err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}
	if got := revParse(t, repo, "refs/heads/behind"); got != upstreamTip {
		t.Errorf("branch behind = %s, want origin's %s", got, upstreamTip)
	}
	if got := revParse(t, repo, "refs/tags/behind"); got != root {
		t.Errorf("tag behind moved to %s, want %s", got, root)
	}
}

func TestMergeBranchMergesTheBranchNotATagOfItsName(t *testing.T) {
	t.Parallel()
	repo, _, _ := sharedNameRepo(t)
	tip := revParse(t, repo, "refs/heads/dup")
	result, err := NewCore().MergeBranch(repo, "main", "dup")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	if result.SHA != tip || revParse(t, repo, "refs/heads/main") != tip {
		t.Errorf("main after merging dup = %s (%+v), want dup's tip %s", revParse(t, repo, "refs/heads/main"), result, tip)
	}
}

// A branch that does not exist is never checked out as whatever else its
// name resolves to: `git worktree add <path> <name>` detaches at a tag or
// commit of that name.
func TestAttachWorktreeRefusesABranchNameThatOnlyATagHas(t *testing.T) {
	t.Parallel()
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "tag", "gone")
	core := NewCore()
	for _, branch := range []string{"gone", "refs/tags/gone"} {
		path := filepath.Join(t.TempDir(), "wt")
		created, err := core.AttachWorktreeAt(repo, path, BranchRef(branch))
		if err == nil || created || !strings.Contains(err.Error(), "not found") {
			t.Errorf("AttachWorktreeAt(branch %q) = %v, %v; want a branch not found error", branch, created, err)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("AttachWorktreeAt(branch %q) left %s behind (%v)", branch, path, statErr)
		}
	}
}

// A branch only a remote other than origin has is checked out as a local
// branch tracking it, by the rule an in-place checkout uses: the one remote
// that has it, and a refusal when several do.
func TestAttachWorktreeTracksTheOneRemoteThatHasTheBranch(t *testing.T) {
	t.Parallel()
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "branch", "feature/fork")
	tip := revParse(t, repo, "refs/heads/feature/fork")
	for _, remote := range []string{"fork", "mirror"} {
		bare := t.TempDir()
		testutil.RunGit(t, bare, "init", "-q", "--bare")
		testutil.RunGit(t, repo, "remote", "add", remote, bare)
	}
	testutil.RunGit(t, repo, "push", "-q", "fork", "feature/fork")
	testutil.RunGit(t, repo, "branch", "-q", "-D", "feature/fork")
	// A tag of the same name must not answer for the branch.
	testutil.RunGit(t, repo, "tag", "feature/fork", "HEAD")

	core := NewCore()
	path := filepath.Join(t.TempDir(), "wt")
	created, err := core.AttachWorktreeAt(repo, path, BranchRef("feature/fork"))
	if err != nil || !created {
		t.Fatalf("AttachWorktreeAt = %v, %v; want the local branch created", created, err)
	}
	if ref, err := core.CurrentRef(path); err != nil || ref != BranchRef("feature/fork") {
		t.Errorf("worktree is on %+v (%v), want branch feature/fork", ref, err)
	}
	if got := revParse(t, repo, "refs/heads/feature/fork"); got != tip {
		t.Errorf("feature/fork = %s, want the fork's %s", got, tip)
	}
	if upstream, ok := core.upstreamFor(repo, "feature/fork"); !ok || upstream != "fork/feature/fork" {
		t.Errorf("upstream = %q, %v; want fork/feature/fork", upstream, ok)
	}

	testutil.RunGit(t, repo, "branch", "feature/both")
	testutil.RunGit(t, repo, "push", "-q", "fork", "feature/both")
	testutil.RunGit(t, repo, "push", "-q", "mirror", "feature/both")
	testutil.RunGit(t, repo, "branch", "-q", "-D", "feature/both")
	both := filepath.Join(t.TempDir(), "wt")
	if created, err := core.AttachWorktreeAt(repo, both, BranchRef("feature/both")); err == nil || created || !strings.Contains(err.Error(), "several remotes") {
		t.Errorf("AttachWorktreeAt(branch on two remotes) = %v, %v; want a several remotes error", created, err)
	}
	if testutil.RunGitAllowError(repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature/both") == nil {
		t.Error("a refused attach left the local branch feature/both")
	}
}
