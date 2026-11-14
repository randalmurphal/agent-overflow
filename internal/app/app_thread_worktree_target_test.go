package app

import (
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// otherRemoteRepo is a clone with remotes fork and mirror besides origin:
// branch fork-only only fork has, at a commit no local branch has, and
// branch on-both both fork and mirror have.
func otherRemoteRepo(t *testing.T) (repo, forkTip string) {
	t.Helper()
	repo, _ = initGitRepoWithOrigin(t)
	for _, remote := range []string{"fork", "mirror"} {
		bare := t.TempDir()
		runGit(t, bare, "init", "-q", "--bare")
		runGit(t, repo, "remote", "add", remote, bare)
	}
	runGit(t, repo, "checkout", "-q", "-b", "fork-only")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on the fork")
	forkTip = gitRevParse(t, repo, "HEAD")
	runGit(t, repo, "push", "-q", "fork", "fork-only")
	runGit(t, repo, "branch", "on-both")
	runGit(t, repo, "push", "-q", "fork", "on-both")
	runGit(t, repo, "push", "-q", "mirror", "on-both")
	runGit(t, repo, "checkout", "-q", "main")
	runGit(t, repo, "branch", "-q", "-D", "fork-only", "on-both")
	return repo, forkTip
}

// A branch only a remote other than origin has is checked out by every
// fresh-worktree path, tracking that remote, as AttachThreadWorktree does;
// one several remotes have is refused rather than created anew.
func TestFreshWorktreeChecksOutABranchOnlyAnotherRemoteHas(t *testing.T) {
	t.Parallel()
	repo, forkTip := otherRemoteRepo(t)
	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "look", WorktreeBranch: "fork-only"})
	if err != nil {
		t.Fatalf("Spawn(fork-only): %v", err)
	}
	spawned, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if spawned.Branch != "fork-only" || spawned.WorktreePath == "" || gitRevParse(t, spawned.WorktreePath, "HEAD") != forkTip {
		t.Fatalf("spawn: worktree %q branch %q, want branch fork-only at the fork's %s", spawned.WorktreePath, spawned.Branch, forkTip)
	}
	if upstream := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "--abbrev-ref", "fork-only@{upstream}")); upstream != "fork/fork-only" {
		t.Errorf("fork-only upstream = %q, want fork/fork-only", upstream)
	}

	_, err = f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "look", WorktreeBranch: "on-both"})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest || !strings.Contains(err.Error(), "several remotes") {
		t.Errorf("Spawn(on-both) = code %q, %v; want an invalid request naming several remotes", code, err)
	}
	if branchExists(repo, "on-both") {
		t.Error("a refused spawn created branch on-both")
	}

	other, _ := otherRemoteRepo(t)
	app, _ := setupE2EAppOn(t, storetest.ClonePath(t), t.TempDir())
	project, err := app.ensureProjectForWorkspace(other)
	if err != nil {
		t.Fatal(err)
	}
	created, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, WorktreeBranch: "fork-only",
	})
	if err != nil {
		t.Fatalf("CreateThread(fork-only): %v", err)
	}
	if created.Branch != "fork-only" || gitRevParse(t, created.WorktreePath, "HEAD") != gitRevParse(t, other, "refs/remotes/fork/fork-only") {
		t.Errorf("CreateThread: worktree %q branch %q, want branch fork-only at the fork's tip", created.WorktreePath, created.Branch)
	}
	if _, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, WorktreeBranch: "on-both",
	}); err == nil || !strings.Contains(err.Error(), "several remotes") {
		t.Errorf("CreateThread(on-both) = %v, want a several remotes error", err)
	}
	if branchExists(other, "on-both") {
		t.Error("a refused create made branch on-both")
	}
}

// Thread creation resolves a typed worktree name as a spawn does: a bare
// name that is only a tag checks the tag out detached instead of creating a
// branch named after it.
func TestCreateThreadChecksOutABareTagName(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EAppOn(t, storetest.ClonePath(t), t.TempDir())
	repo, _, tagged, _ := tagSpawnRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, WorktreeBranch: "v1.0",
	})
	if err != nil {
		t.Fatalf("CreateThread(v1.0): %v", err)
	}
	if thread.Branch != "" || thread.WorktreePath == "" || !detached(thread.WorktreePath) || gitRevParse(t, thread.WorktreePath, "HEAD") != tagged {
		t.Errorf("thread = worktree %q branch %q, want detached at the tag's %s", thread.WorktreePath, thread.Branch, tagged)
	}
	if branchExists(repo, "v1.0") {
		t.Error("CreateThread made a branch named after the tag")
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil || stored.Branch != "" {
		t.Errorf("stored branch = %q (%v), want none", stored.Branch, err)
	}
}
