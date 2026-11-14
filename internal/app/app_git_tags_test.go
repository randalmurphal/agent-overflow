package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func gitRev(t *testing.T, repo, rev string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, repo, "rev-parse", "--verify", rev+"^{commit}"))
}

func gitCommitEmpty(t *testing.T, repo, message string) string {
	t.Helper()
	testutil.RunGit(t, repo, "commit", "-q", "--allow-empty", "-m", message)
	return gitRev(t, repo, "HEAD")
}

// tagFixture is a repository with tag v1 one commit behind main, a branch
// and a tag both named "dup" at different commits, and a thread in the
// project root on main.
type tagFixture struct {
	app     *App
	repo    string
	project store.Project
	thread  store.Thread
	ref     WorkspaceRef
	v1      string
	main    string
	dupTag  string
	dupHead string
}

func newTagFixture(t *testing.T, repo string) tagFixture {
	t.Helper()
	app := newTestAppWithStore(t)
	testutil.RunGit(t, repo, "tag", "v1")
	v1 := gitRev(t, repo, "HEAD")
	testutil.RunGit(t, repo, "branch", "dup")
	dupTag := gitCommitEmpty(t, repo, "tagged dup")
	testutil.RunGit(t, repo, "tag", "dup")
	main := gitCommitEmpty(t, repo, "main tip")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	thread := testThread("thread-" + strings.ReplaceAll(t.Name(), "/", "-"))
	thread.ProjectID = project.ID
	thread.WorkspacePath = project.Path
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return tagFixture{
		app: app, repo: project.Path, project: project, thread: thread,
		ref: WorkspaceRef{ProjectID: project.ID, WorkspacePath: project.Path},
		v1:  v1, main: main, dupTag: dupTag, dupHead: v1,
	}
}

func (f tagFixture) storedBranch(t *testing.T, id string) string {
	t.Helper()
	stored, err := f.app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return stored.Branch
}

func TestGitCheckoutOfATagDetachesAndClearsThreadBranches(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))

	state, err := f.app.GitCheckout(f.ref, "refs/tags/v1")
	if err != nil {
		t.Fatalf("GitCheckout(tag): %v", err)
	}
	if state.Branch != "" || state.Status == nil || state.Status.Tag != "v1" || state.Status.HeadCommit != f.v1 || state.LeftBehind != nil {
		t.Fatalf("state = %+v, want no branch at tag v1 (%s) and nothing left behind", state, f.v1)
	}
	if head := gitRev(t, f.repo, "HEAD"); head != f.v1 {
		t.Fatalf("HEAD = %s, want v1 %s", head, f.v1)
	}
	if branch := f.storedBranch(t, f.thread.ID); branch != "" {
		t.Fatalf("thread branch = %q, want empty after a tag checkout", branch)
	}

	// A branch and a tag of one name: the bare name is the branch, the
	// refs/tags/ spelling the tag.
	if state, err = f.app.GitCheckout(f.ref, "refs/tags/dup"); err != nil || state.Status == nil || state.Status.Tag != "dup" || gitRev(t, f.repo, "HEAD") != f.dupTag {
		t.Fatalf("GitCheckout(refs/tags/dup) = %+v, %v; want detached at the tag", state, err)
	}
	if state, err = f.app.GitCheckout(f.ref, "dup"); err != nil || state.Branch != "dup" || state.Status == nil || state.Status.Tag != "" || state.Status.HeadCommit != "" || gitRev(t, f.repo, "HEAD") != f.dupHead {
		t.Fatalf("GitCheckout(dup) = %+v, %v; want the branch", state, err)
	}
	if branch := f.storedBranch(t, f.thread.ID); branch != "dup" {
		t.Fatalf("thread branch = %q, want dup", branch)
	}
}

func TestGitCheckoutReportsCommitsLeftOnADetachedHead(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	if _, err := f.app.GitCheckout(f.ref, "refs/tags/v1"); err != nil {
		t.Fatal(err)
	}
	gitCommitEmpty(t, f.repo, "detached one")
	left := gitCommitEmpty(t, f.repo, "detached two")

	state, err := f.app.GitCheckout(f.ref, "main")
	if err != nil {
		t.Fatalf("GitCheckout(main) from a detached HEAD with commits: %v", err)
	}
	if state.Branch != "main" || state.LeftBehind == nil || *state.LeftBehind != (gitops.LeftBehind{Count: 2, Commit: left}) {
		t.Fatalf("state = %+v (leftBehind %+v), want main with 2 commits left at %s", state, state.LeftBehind, left)
	}

	// The create-branch path that checks out another base reports it too.
	if _, err := f.app.GitCheckout(f.ref, "refs/tags/v1"); err != nil {
		t.Fatal(err)
	}
	left = gitCommitEmpty(t, f.repo, "detached again")
	state, err = f.app.GitCreateBranchFrom(f.ref, "off-main", "main", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom(main) from a detached HEAD: %v", err)
	}
	if state.Branch != "off-main" || state.LeftBehind == nil || *state.LeftBehind != (gitops.LeftBehind{Count: 1, Commit: left}) {
		t.Fatalf("state = %+v (leftBehind %+v), want off-main with 1 commit left at %s", state, state.LeftBehind, left)
	}
}

func TestGitCreateBranchFromADetachedHeadKeepsItsCommits(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	if _, err := f.app.GitCheckout(f.ref, "refs/tags/v1"); err != nil {
		t.Fatal(err)
	}
	head := gitCommitEmpty(t, f.repo, "work on no branch")

	state, err := f.app.GitCreateBranchFrom(f.ref, "rescue", "", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom(empty base) on a detached HEAD: %v", err)
	}
	if state.Branch != "rescue" || state.LeftBehind != nil || gitRev(t, f.repo, "refs/heads/rescue") != head {
		t.Fatalf("state = %+v; want rescue at %s with nothing left behind", state, head)
	}
	if branch := f.storedBranch(t, f.thread.ID); branch != "rescue" {
		t.Fatalf("thread branch = %q, want rescue", branch)
	}
}

func TestGitCreateBranchFromATagBase(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))

	state, err := f.app.GitCreateBranchFrom(f.ref, "from-tag", "refs/tags/dup", false)
	if err != nil || state.Branch != "from-tag" || gitRev(t, f.repo, "refs/heads/from-tag") != f.dupTag {
		t.Fatalf("GitCreateBranchFrom(tag dup) = %+v, %v; want from-tag at the tag", state, err)
	}
	state, err = f.app.GitCreateBranchFrom(f.ref, "from-branch", "dup", false)
	if err != nil || gitRev(t, f.repo, "refs/heads/from-branch") != f.dupHead {
		t.Fatalf("GitCreateBranchFrom(branch dup) = %+v, %v; want from-branch at the branch", state, err)
	}

	// At a tag, that tag is the current ref, so carrying changes is allowed
	// and they stay in the tree.
	if _, err := f.app.GitCheckout(f.ref, "refs/tags/v1"); err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(f.repo, "README.txt")
	if err := os.WriteFile(dirty, []byte("carried\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.GitCreateBranchFrom(f.ref, "carry", "refs/tags/v1", true); err != nil {
		t.Fatalf("GitCreateBranchFrom(current tag, carry): %v", err)
	}
	if data, err := os.ReadFile(dirty); err != nil || string(data) != "carried\n" {
		t.Fatalf("carried change = %q, %v", data, err)
	}
	if _, err := f.app.GitCreateBranchFrom(f.ref, "carry-elsewhere", "refs/tags/dup", true); err == nil {
		t.Fatal("carry onto a tag that is not the current ref was accepted")
	}
}

func TestGitListAndRefreshTags(t *testing.T) {
	t.Parallel()
	repo, bare := initGitRepoWithOrigin(t)
	f := newTagFixture(t, repo)
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", "-q", bare, ".")
	testutil.RunGit(t, sibling, "tag", "upstream-only")
	testutil.RunGit(t, sibling, "push", "-q", "origin", "upstream-only")

	names := func(tags []gitops.GitTag) map[string]bool {
		set := map[string]bool{}
		for _, tag := range tags {
			set[tag.Name] = true
		}
		return set
	}
	tags, err := f.app.GitListTags(f.ref)
	if err != nil || !names(tags)["v1"] || names(tags)["upstream-only"] {
		t.Fatalf("GitListTags = %+v, %v; want local tags only", tags, err)
	}
	tags, err = f.app.GitRefreshTags(f.ref)
	if err != nil || !names(tags)["v1"] || !names(tags)["upstream-only"] {
		t.Fatalf("GitRefreshTags = %+v, %v; want the upstream tag and the local ones", tags, err)
	}

	noOrigin := newTagFixture(t, initMainGitRepo(t))
	if _, err := noOrigin.app.GitRefreshTags(noOrigin.ref); err == nil || !strings.Contains(err.Error(), "no origin") {
		t.Fatalf("GitRefreshTags without origin = %v, want a clear error", err)
	}
}

func TestAttachThreadWorktreeAtATag(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	second := testThread("thread-attach-tag-second")
	second.ProjectID = f.project.ID
	second.WorkspacePath = f.repo
	if err := f.app.store.CreateThread(second); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, id := range []string{f.thread.ID, second.ID} {
		updated, err := f.app.AttachThreadWorktree(id, "refs/tags/dup")
		if err != nil {
			t.Fatalf("AttachThreadWorktree(%s, tag): %v", id, err)
		}
		t.Cleanup(func() { _ = f.app.gitCore().RemoveWorktreeForce(f.repo, updated.WorktreePath, true) })
		if updated.Branch != "" || !strings.HasPrefix(filepath.Base(updated.WorktreePath), "dup") {
			t.Fatalf("thread = branch %q in %s; want no branch in a directory named after the tag", updated.Branch, updated.WorktreePath)
		}
		if head := gitRev(t, updated.WorktreePath, "HEAD"); head != f.dupTag || f.app.gitCore().CurrentBranch(updated.WorktreePath) != "" {
			t.Fatalf("worktree HEAD = %s on %q, want detached at %s", head, f.app.gitCore().CurrentBranch(updated.WorktreePath), f.dupTag)
		}
		paths = append(paths, updated.WorktreePath)
	}
	if samePath(paths[0], paths[1]) {
		t.Fatalf("both tag worktrees at %s", paths[0])
	}
	items, err := f.app.GitListWorktrees(f.ref)
	if err != nil {
		t.Fatal(err)
	}
	tagged := 0
	for _, item := range items {
		if item.Tag == "dup" {
			tagged++
		}
	}
	if tagged != 2 {
		t.Fatalf("worktree list = %+v, want two worktrees at tag dup", items)
	}
}

func TestPrepareThreadWorktreeFromATagBase(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	updated, err := f.app.PrepareThreadWorktree(f.thread.ID, "refs/tags/dup", "from-tag", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree(tag base): %v", err)
	}
	t.Cleanup(func() { _ = f.app.gitCore().RemoveWorktreeForce(f.repo, updated.WorktreePath, true) })
	if updated.Branch != "from-tag" || gitRev(t, f.repo, "refs/heads/from-tag") != f.dupTag {
		t.Fatalf("thread = %+v; want from-tag cut at the tag %s", updated, f.dupTag)
	}
	if err := testutil.RunGitAllowError(f.repo, "config", "--get", "branch.from-tag.merge"); err == nil {
		t.Fatal("a branch cut from a tag has an upstream")
	}
}

// An empty base is the thread's own workspace's ref, never the project
// root's branch: a thread detached at a tag in a worktree cuts from the tag.
func TestPrepareThreadWorktreeEmptyBaseUsesTheSourceWorkspacesRef(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	attached, err := f.app.AttachThreadWorktree(f.thread.ID, "refs/tags/v1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.app.gitCore().RemoveWorktreeForce(f.repo, attached.WorktreePath, true) })

	updated, err := f.app.PrepareThreadWorktree(f.thread.ID, "", "from-source", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree(empty base): %v", err)
	}
	t.Cleanup(func() { _ = f.app.gitCore().RemoveWorktreeForce(f.repo, updated.WorktreePath, true) })
	if head := gitRev(t, f.repo, "refs/heads/from-source"); head != f.v1 {
		t.Fatalf("from-source at %s, want the source worktree's tag v1 %s (root main is %s)", head, f.v1, f.main)
	}
}

// With an empty base the thread row's branch answers first, so a thread
// whose worktree directory is gone still cuts from its branch (the branch,
// not the same-named tag).
func TestPrepareThreadWorktreeEmptyBaseUsesTheRowBranchWhenTheWorkspaceIsGone(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	gone := filepath.Join(t.TempDir(), "gone")
	testutil.RunGit(t, f.repo, "worktree", "add", "-q", "-b", "side", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.app.store.UpdateThreadCheckout(f.thread.ID, gone, gone, "dup"); err != nil {
		t.Fatal(err)
	}

	updated, err := f.app.PrepareThreadWorktree(f.thread.ID, "", "from-row", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree(empty base, workspace gone): %v", err)
	}
	t.Cleanup(func() { _ = f.app.gitCore().RemoveWorktreeForce(f.repo, updated.WorktreePath, true) })
	if head := gitRev(t, f.repo, "refs/heads/from-row"); head != f.dupHead {
		t.Fatalf("from-row at %s, want branch dup %s", head, f.dupHead)
	}
}

// Commits on no branch are unsaved work: removal refuses them unless
// forced, and keeping a branch does not waive them because there is none.
func TestRemoveWorktreeRefusesOrphanCommits(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	attached, err := f.app.AttachThreadWorktree(f.thread.ID, "refs/tags/v1")
	if err != nil {
		t.Fatal(err)
	}
	worktree := attached.WorktreePath
	gitCommitEmpty(t, worktree, "on no branch")

	status, err := f.app.GitWorktreeStatus(f.ref, worktree)
	if err != nil || status.OrphanCommits != 1 {
		t.Fatalf("GitWorktreeStatus = %+v, %v; want one orphan commit", status, err)
	}
	if _, err := f.app.RemoveOtherWorktree(f.ref, worktree, false); err == nil || !strings.Contains(err.Error(), "orphan=1") {
		t.Fatalf("unforced removal = %v, want the orphan-commit refusal", err)
	}
	if _, err := f.app.removeProjectWorktree(f.project.ID, f.repo, worktree, worktreeRemovalPolicy{keepsBranch: true}); err == nil || !strings.Contains(err.Error(), "orphan=1") {
		t.Fatalf("keepsBranch removal = %v, want the orphan-commit refusal", err)
	}
	if _, err := f.app.RemoveOtherWorktree(f.ref, worktree, true); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if _, registered, err := f.app.findWorktree(f.repo, worktree); err != nil || registered {
		t.Fatalf("after forced removal: registered=%v err=%v", registered, err)
	}
}

// On a detached HEAD the current base is a commit: another tag at that
// commit is the same base, so carrying changes onto it is allowed and they
// stay in the tree, for a fresh worktree and for a branch made in place.
func TestATagAtTheDetachedCommitIsTheCurrentBase(t *testing.T) {
	t.Parallel()
	f := newTagFixture(t, initMainGitRepo(t))
	testutil.RunGit(t, f.repo, "tag", "pair-a", f.v1)
	testutil.RunGit(t, f.repo, "tag", "pair-b", f.v1)
	if _, err := f.app.GitCheckout(f.ref, "refs/tags/pair-a"); err != nil {
		t.Fatal(err)
	}
	if tag := strings.TrimSpace(gitOutput(t, f.repo, "for-each-ref", "--count=1", "--points-at=HEAD", "--sort=refname", "--sort=-creatordate", "--format=%(refname:lstrip=2)", "refs/tags")); tag == "pair-b" {
		t.Fatal("the fixture needs a current ref other than pair-b")
	}
	dirty := filepath.Join(f.repo, "README.txt")
	if err := os.WriteFile(dirty, []byte("carried\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, err := f.app.PrepareThreadWorktree(f.thread.ID, "refs/tags/pair-b", "carried-worktree", true)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree(other tag at HEAD, carry): %v", err)
	}
	t.Cleanup(func() { _ = f.app.gitCore().RemoveWorktreeForce(f.repo, updated.WorktreePath, true) })
	if data, err := os.ReadFile(filepath.Join(updated.WorktreePath, "README.txt")); err != nil || string(data) != "carried\n" {
		t.Fatalf("carried change in the worktree = %q, %v", data, err)
	}

	if err := os.WriteFile(dirty, []byte("carried\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := f.app.GitCreateBranchFrom(f.ref, "carried-branch", "refs/tags/pair-b", true)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom(other tag at HEAD, carry): %v", err)
	}
	if state.Branch != "carried-branch" || gitRev(t, f.repo, "refs/heads/carried-branch") != f.v1 {
		t.Fatalf("state = %+v, want carried-branch at %s", state, f.v1)
	}
	if data, err := os.ReadFile(dirty); err != nil || string(data) != "carried\n" {
		t.Fatalf("carried change = %q, %v", data, err)
	}

	// Without carry the same base keeps the tree too: it is the current
	// base, so nothing is stashed and discarded.
	if _, err := f.app.GitCheckout(f.ref, "refs/tags/pair-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.GitCreateBranchFrom(f.ref, "kept-branch", "refs/tags/pair-b", false); err != nil {
		t.Fatalf("GitCreateBranchFrom(other tag at HEAD): %v", err)
	}
	if data, err := os.ReadFile(dirty); err != nil || string(data) != "carried\n" {
		t.Fatalf("change after a create from the current base = %q, %v; want it kept", data, err)
	}
}
