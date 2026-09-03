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

func TestGetGitStatusUsesWorkspacePath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	thread := testThread("thread-status")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	status, err := app.GetGitStatus(workspaceRefForThread(thread))
	if err != nil {
		t.Fatalf("GetGitStatus() error = %v", err)
	}
	if !status.IsRepo {
		t.Fatal("expected workspace git status to report a repo")
	}
	if status.Branch != "main" {
		t.Fatalf("Branch = %q, want main", status.Branch)
	}
}

func TestGetGitStatusBypassesCachedPRLookupError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PATH override in short mode")
	}

	app := newTestAppWithStore(t)
	app.git = gitops.NewCore()
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "calls")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\ncount=0\nif [ -f " + counterFile + " ]; then count=$(wc -c < " + counterFile + "); fi\nprintf x >> " + counterFile + "\nif [ \"$count\" = \"0\" ]; then echo 'auth required' 1>&2; exit 1; fi\necho '[{\"url\":\"https://github.com/owner/repo/pull/9\",\"number\":9,\"title\":\"Demo\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	seeded, err := app.git.Status(repo)
	if err != nil {
		t.Fatalf("seed Status() error = %v", err)
	}
	if seeded.OpenPRLookupError == "" {
		t.Fatal("seed Status() did not cache a PR lookup error")
	}

	thread := testThread("thread-status-pr-error")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	status, err := app.GetGitStatus(workspaceRefForThread(thread))
	if err != nil {
		t.Fatalf("GetGitStatus() error = %v", err)
	}
	if status.OpenPRLookupError != "" {
		t.Fatalf("OpenPRLookupError = %q, want retried success", status.OpenPRLookupError)
	}
	if status.OpenPRURL != "https://github.com/owner/repo/pull/9" || status.OpenPRNumber != 9 {
		t.Fatalf("open PR = (%q, %d), want PR #9", status.OpenPRURL, status.OpenPRNumber)
	}
}

// Branches are a repository fact, so a ref that names a WORKTREE still lists
// the project's branches — the workspace only decides where the read runs.
func TestGitListBranchesUsesProjectPath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "branches-wt")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/demo", worktreePath)
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true) })

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	branches, err := app.GitListBranches(WorkspaceRef{ProjectID: project.ID, WorkspacePath: worktreePath})
	if err != nil {
		t.Fatalf("GitListBranches() error = %v", err)
	}

	if !containsBranch(branches, "feature/demo") {
		t.Fatalf("expected feature/demo in branches: %+v", branches)
	}
}

// A workspace path that is neither the project root nor one of its registered
// worktrees is refused rather than silently resolved to the project.
func TestGitListBranchesRefusesForeignWorkspacePath(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	if _, err := app.GitListBranches(
		WorkspaceRef{ProjectID: project.ID, WorkspacePath: t.TempDir()},
	); err == nil {
		t.Fatal("GitListBranches accepted a workspace outside the project")
	}
}

func TestGitCommitReturnsCommitSHA(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	thread := testThread("thread-commit")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	readme := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readme, []byte("hello\nupdated\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := app.GitCommit(workspaceRefForThread(thread), "update readme", "body")
	if err != nil {
		t.Fatalf("GitCommit() error = %v", err)
	}
	if result.Action != "commit" {
		t.Fatalf("Action = %q, want commit", result.Action)
	}
	if len(result.Commit) != 40 {
		t.Fatalf("Commit length = %d, want 40", len(result.Commit))
	}
	if result.Branch != "main" {
		t.Fatalf("Branch = %q, want main", result.Branch)
	}
}

func TestGitCheckoutUpdatesStoredBranch(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "feature/checkout")

	thread := testThread("thread-checkout")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	state, err := app.GitCheckout(workspaceRefForThread(thread), "feature/checkout")
	if err != nil {
		t.Fatalf("GitCheckout() error = %v", err)
	}
	if state.Branch != "feature/checkout" {
		t.Fatalf("state Branch = %q, want feature/checkout", state.Branch)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "feature/checkout" {
		t.Fatalf("stored Branch = %q, want feature/checkout", stored.Branch)
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("stored UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
	}
}

// initRepoWithUpstreamFeature sets up a bare repo with main and a
// feature branch that's behind by one commit on the working clone.
// Returns (workingRepo, barePath). The working clone is checked out
// on main; its local `feature` ref is set up to track origin/feature.
func initRepoWithUpstreamFeature(t *testing.T) (string, string) {
	t.Helper()
	bare := t.TempDir()
	if err := testutil.RunGitAllowError(bare, "init", "--bare", "-b", "main"); err != nil {
		testutil.RunGit(t, bare, "init", "--bare")
	}
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", bare)
	testutil.RunGit(t, repo, "push", "-u", "origin", "main")

	// Sibling clone pushes a feature branch + one extra commit so the
	// working repo's local feature is one commit behind upstream.
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", bare, ".")
	testutil.RunGit(t, sibling, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(sibling, "feature.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	testutil.RunGit(t, sibling, "add", "feature.txt")
	testutil.RunGit(t, sibling, "-c", "user.email=sib@example.com", "-c", "user.name=Sib",
		"commit", "-m", "feature v1")
	testutil.RunGit(t, sibling, "push", "-u", "origin", "feature")

	// Working repo learns about origin/feature and creates a tracking
	// local feature ref pointed at the current upstream tip.
	testutil.RunGit(t, repo, "fetch", "origin")
	testutil.RunGit(t, repo, "branch", "--track", "feature", "origin/feature")

	// Sibling advances feature once more — now working repo's feature
	// ref is behind by one commit.
	if err := os.WriteFile(filepath.Join(sibling, "feature2.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("write feature2.txt: %v", err)
	}
	testutil.RunGit(t, sibling, "add", "feature2.txt")
	testutil.RunGit(t, sibling, "-c", "user.email=sib@example.com", "-c", "user.name=Sib",
		"commit", "-m", "feature v2")
	testutil.RunGit(t, sibling, "push", "origin", "feature")
	return repo, bare
}

func pushRemoteMainUpdate(t *testing.T, app *App, repo, filename string) {
	t.Helper()
	mainSibling := t.TempDir()
	bare, _, err := app.gitCore().Execute(repo, "config", "--get", "remote.origin.url")
	if err != nil {
		t.Fatalf("read origin url: %v", err)
	}
	testutil.RunGit(t, mainSibling, "clone", strings.TrimSpace(bare), ".")
	if err := os.WriteFile(filepath.Join(mainSibling, filename), []byte("u"), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	testutil.RunGit(t, mainSibling, "add", filename)
	testutil.RunGit(t, mainSibling, "-c", "user.email=ms@example.com", "-c", "user.name=MS",
		"commit", "-m", "main update")
	testutil.RunGit(t, mainSibling, "push", "origin", "main")
	testutil.RunGit(t, repo, "fetch", "origin")
}

func TestGitSyncBranchCurrentBranchAllowedDuringActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo, _ := initRepoWithUpstreamFeature(t)

	// Sibling pushes a commit on main so main is behind by 1.
	pushRemoteMainUpdate(t, app, repo, "main-update.txt")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-sync-current-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-sync-current",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	beforeTip, _, err := app.gitCore().Execute(repo, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse main pre-sync: %v", err)
	}

	branches, err := app.GitSyncBranch(workspaceRefForThread(thread), "main")
	if err != nil {
		t.Fatalf("GitSyncBranch(main) during active turn error = %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected refreshed branch list, got empty")
	}
	afterTip, _, err := app.gitCore().Execute(repo, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse main post-sync: %v", err)
	}
	if strings.TrimSpace(afterTip) == strings.TrimSpace(beforeTip) {
		t.Fatal("expected current branch to advance after sync")
	}
}

func TestGitSyncBranchNonCurrentBranchAllowedWithActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo, _ := initRepoWithUpstreamFeature(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-sync-noncurrent-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-sync-noncurrent",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	// Read the working repo's feature tip before sync so we can prove
	// it advanced.
	beforeTip, _, err := app.gitCore().Execute(repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse feature pre-sync: %v", err)
	}

	branches, err := app.GitSyncBranch(workspaceRefForThread(thread), "feature")
	if err != nil {
		t.Fatalf("GitSyncBranch(feature) error = %v (should be allowed with active turn for non-current branch)", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected refreshed branch list, got empty")
	}

	afterTip, _, err := app.gitCore().Execute(repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse feature post-sync: %v", err)
	}
	if strings.TrimSpace(afterTip) == strings.TrimSpace(beforeTip) {
		t.Fatal("expected feature ref to advance after sync")
	}

	// HEAD must still be on main — non-current sync should never touch
	// the working tree.
	head, _, err := app.gitCore().Execute(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if strings.TrimSpace(head) != "main" {
		t.Fatalf("expected HEAD to stay on main, got %q", strings.TrimSpace(head))
	}
}

func TestGitSyncBranchWithoutThreadRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo, _ := initRepoWithUpstreamFeature(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	beforeTip, _, err := app.gitCore().Execute(repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse feature pre-sync: %v", err)
	}

	branches, err := app.GitSyncBranch(WorkspaceRef{ProjectID: project.ID, WorkspacePath: repo}, "feature")
	if err != nil {
		t.Fatalf("GitSyncBranch(feature) error = %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected refreshed branch list, got empty")
	}

	afterTip, _, err := app.gitCore().Execute(repo, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse feature post-sync: %v", err)
	}
	if strings.TrimSpace(afterTip) == strings.TrimSpace(beforeTip) {
		t.Fatal("expected feature ref to advance after project sync")
	}

	head, _, err := app.gitCore().Execute(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if strings.TrimSpace(head) != "main" {
		t.Fatalf("expected HEAD to stay on main, got %q", strings.TrimSpace(head))
	}
}

func TestGitSyncBranchSyncsCurrentWorktreeWithoutTouchingProjectRoot(t *testing.T) {
	app := newTestAppWithStore(t)
	repo, _ := initRepoWithUpstreamFeature(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-feature-sync")
	testutil.RunGit(t, repo, "worktree", "add", worktreePath, "feature")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	beforeTip, _, err := app.gitCore().Execute(worktreePath, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse worktree feature pre-sync: %v", err)
	}
	branches, err := app.GitSyncBranch(WorkspaceRef{ProjectID: project.ID, WorkspacePath: worktreePath}, "feature")
	if err != nil {
		t.Fatalf("GitSyncBranch(feature worktree) error = %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected refreshed branch list, got empty")
	}
	afterTip, _, err := app.gitCore().Execute(worktreePath, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse worktree feature post-sync: %v", err)
	}
	if strings.TrimSpace(afterTip) == strings.TrimSpace(beforeTip) {
		t.Fatal("expected worktree feature branch to advance after project sync")
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project root branch = %q, want main", got)
	}
}

func TestGitSyncBranchDoesNotSyncBranchCheckedOutInOtherWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo, _ := initRepoWithUpstreamFeature(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-feature-no-root-sync")
	testutil.RunGit(t, repo, "worktree", "add", worktreePath, "feature")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})
	pushRemoteMainUpdate(t, app, repo, "main-hidden-sync.txt")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	beforeMain, _, err := app.gitCore().Execute(repo, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse root main pre-sync: %v", err)
	}

	_, err = app.GitSyncBranch(WorkspaceRef{ProjectID: project.ID, WorkspacePath: worktreePath}, "main")
	if err == nil {
		t.Fatal("expected syncing main from the feature worktree to fail because main is checked out at the project root")
	}
	afterMain, _, err := app.gitCore().Execute(repo, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse root main post-sync: %v", err)
	}
	if strings.TrimSpace(afterMain) != strings.TrimSpace(beforeMain) {
		t.Fatalf("project root main moved from %s to %s", strings.TrimSpace(beforeMain), strings.TrimSpace(afterMain))
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project root branch = %q, want main", got)
	}
	if got := app.gitCore().CurrentBranch(worktreePath); got != "feature" {
		t.Fatalf("worktree branch = %q, want feature", got)
	}
}

func TestGitSyncBranchCurrentBranchAllowsActiveWorkspaceThread(t *testing.T) {
	app := newTestAppWithStore(t)
	repo, _ := initRepoWithUpstreamFeature(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-active-sync")
	testutil.RunGit(t, repo, "worktree", "add", worktreePath, "feature")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-project-sync-active")
	thread.ProjectID = project.ID
	thread.WorkspacePath = worktreePath
	thread.WorktreePath = worktreePath
	thread.Branch = "feature"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-project-sync-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	beforeTip, _, err := app.gitCore().Execute(worktreePath, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse worktree feature pre-sync: %v", err)
	}
	branches, err := app.GitSyncBranch(WorkspaceRef{ProjectID: project.ID, WorkspacePath: worktreePath}, "feature")
	if err != nil {
		t.Fatalf("GitSyncBranch current branch during active workspace thread error = %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected refreshed branch list, got empty")
	}
	afterTip, _, err := app.gitCore().Execute(worktreePath, "rev-parse", "feature")
	if err != nil {
		t.Fatalf("rev-parse worktree feature post-sync: %v", err)
	}
	if strings.TrimSpace(afterTip) == strings.TrimSpace(beforeTip) {
		t.Fatal("expected current worktree branch to advance after project sync")
	}
}

// A workspace change is refused while ANY thread in that directory is busy —
// including a SIBLING thread the caller never named. The ref addresses the
// directory, so the directory's occupants are what the safety check consults.
func TestGitCheckoutAllowedWhileSiblingThreadHasActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/checkout")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-project-checkout-active")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-project-checkout-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	// The caller holds no thread of its own: the busy row is a sibling, and
	// a branch switch is never gated on agent activity (see the rule above).
	state, err := app.GitCheckout(WorkspaceRef{ProjectID: project.ID, WorkspacePath: repo}, "feature/checkout")
	if err != nil {
		t.Fatalf("GitCheckout() during a sibling turn error = %v", err)
	}
	if state.Branch != "feature/checkout" {
		t.Fatalf("Branch = %q, want feature/checkout", state.Branch)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "feature/checkout" {
		t.Fatalf("project root branch = %q, want feature/checkout", got)
	}
}

// The thread's OWN active turn does not gate it either.
func TestGitCheckoutAllowedDuringActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "feature/checkout")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-checkout-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-checkout-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	if _, err := app.GitCheckout(workspaceRefForThread(thread), "feature/checkout"); err != nil {
		t.Fatalf("GitCheckout() during own active turn error = %v", err)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "feature/checkout" {
		t.Fatalf("branch = %q, want feature/checkout", got)
	}
}

// GitCheckout straddles app_git.go and app_worktree.go. Checking out a branch
// from a worktree must mutate that selected workspace only; moving to the
// project root is an explicit EnvPicker action.
func TestGitCheckoutDefaultBranchFromWorktreeKeepsCurrentWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-feature")

	testutil.RunGit(t, repo, "branch", "dev")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/worktree", worktreePath, "main")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})
	testutil.RunGit(t, repo, "checkout", "dev")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-checkout-default-from-worktree")
	thread.ProjectID = project.ID
	thread.WorkspacePath = worktreePath
	thread.WorktreePath = worktreePath
	thread.Branch = "feature/worktree"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.GitCheckout(workspaceRefForThread(thread), "main"); err != nil {
		t.Fatalf("GitCheckout(main) error = %v", err)
	}

	if got := app.gitCore().CurrentBranch(repo); got != "dev" {
		t.Fatalf("project root branch = %q, want unchanged dev", got)
	}
	if got := app.gitCore().CurrentBranch(worktreePath); got != "main" {
		t.Fatalf("worktree branch = %q, want main", got)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if !samePath(stored.WorkspacePath, worktreePath) {
		t.Fatalf("WorkspacePath = %q, want %q", stored.WorkspacePath, worktreePath)
	}
	if !samePath(stored.WorktreePath, worktreePath) {
		t.Fatalf("WorktreePath = %q, want %q", stored.WorktreePath, worktreePath)
	}
	if stored.Branch != "main" {
		t.Fatalf("Branch = %q, want main", stored.Branch)
	}
}

// The same checkout addressed by ref alone, with no thread row in the picture.
func TestGitCheckoutFromWorktreeWithoutThreadRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-project-feature")

	testutil.RunGit(t, repo, "branch", "dev")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/project-worktree", worktreePath, "main")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})
	testutil.RunGit(t, repo, "checkout", "dev")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	state, err := app.GitCheckout(WorkspaceRef{ProjectID: project.ID, WorkspacePath: worktreePath}, "main")
	if err != nil {
		t.Fatalf("GitCheckout(main) error = %v", err)
	}
	if !samePath(state.WorkspacePath, worktreePath) {
		t.Fatalf("WorkspacePath = %q, want %q", state.WorkspacePath, worktreePath)
	}
	if !samePath(state.WorktreePath, worktreePath) {
		t.Fatalf("WorktreePath = %q, want %q", state.WorktreePath, worktreePath)
	}
	if state.Branch != "main" {
		t.Fatalf("Branch = %q, want main", state.Branch)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "dev" {
		t.Fatalf("project root branch = %q, want unchanged dev", got)
	}
	if got := app.gitCore().CurrentBranch(worktreePath); got != "main" {
		t.Fatalf("worktree branch = %q, want main", got)
	}
}

func TestGitCheckoutDefaultBranchCheckedOutElsewhereFailsInCurrentWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-checked-out-elsewhere")

	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/checked-out-elsewhere", worktreePath, "main")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-checkout-default-elsewhere")
	thread.ProjectID = project.ID
	thread.WorkspacePath = worktreePath
	thread.WorktreePath = worktreePath
	thread.Branch = "feature/checked-out-elsewhere"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.GitCheckout(workspaceRefForThread(thread), "main"); err == nil {
		t.Fatal("expected GitCheckout(main) to fail because main is checked out in the project root")
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project root branch = %q, want unchanged main", got)
	}
	if got := app.gitCore().CurrentBranch(worktreePath); got != "feature/checked-out-elsewhere" {
		t.Fatalf("worktree branch = %q, want unchanged feature/checked-out-elsewhere", got)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if !samePath(stored.WorkspacePath, worktreePath) {
		t.Fatalf("WorkspacePath = %q, want %q", stored.WorkspacePath, worktreePath)
	}
	if !samePath(stored.WorktreePath, worktreePath) {
		t.Fatalf("WorktreePath = %q, want %q", stored.WorktreePath, worktreePath)
	}
	if stored.Branch != "feature/checked-out-elsewhere" {
		t.Fatalf("Branch = %q, want unchanged feature/checked-out-elsewhere", stored.Branch)
	}
}

func TestGitCheckoutCheckedOutElsewhereFailsWithoutThreadRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	worktreePath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-project-checked-out-elsewhere")

	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/project-checked-out-elsewhere", worktreePath, "main")
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, worktreePath, true)
	})

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	if _, err := app.GitCheckout(
		WorkspaceRef{ProjectID: project.ID, WorkspacePath: worktreePath}, "main",
	); err == nil {
		t.Fatal("expected GitCheckout(main) to fail because main is checked out in the project root")
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project root branch = %q, want unchanged main", got)
	}
	if got := app.gitCore().CurrentBranch(worktreePath); got != "feature/project-checked-out-elsewhere" {
		t.Fatalf("worktree branch = %q, want unchanged feature/project-checked-out-elsewhere", got)
	}
}

func TestGitCreateBranchFromCurrentBaseKeepsDirtyTree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-current")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	updated, err := app.GitCreateBranchFrom(workspaceRefForThread(thread), "feature/keep", "main", true)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom() error = %v", err)
	}
	if updated.Branch != "feature/keep" {
		t.Fatalf("Branch = %q, want feature/keep", updated.Branch)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
	}
	contents, err := os.ReadFile(filepath.Join(repo, "README.txt"))
	if err != nil {
		t.Fatalf("read repo file: %v", err)
	}
	if !strings.Contains(string(contents), "dirty edit") {
		t.Fatalf("dirty edit was lost; got %q", string(contents))
	}
}

func TestGitCreateBranchFromCurrentBaseRejectsExistingBranch(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "BLITZ-187")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-existing")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	readmePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readmePath, []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	_, err = app.GitCreateBranchFrom(workspaceRefForThread(thread), "BLITZ-187", "main", true)
	if err == nil {
		t.Fatal("expected duplicate branch error")
	}
	if !strings.Contains(err.Error(), `create branch: branch "BLITZ-187" already exists`) {
		t.Fatalf("GitCreateBranchFrom() error = %v, want duplicate branch message", err)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("current branch = %q, want unchanged main", got)
	}
	contents, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(contents) != "dirty edit\n" {
		t.Fatalf("dirty tree changed; README = %q", string(contents))
	}
}

func TestGitCreateBranchFromOtherBaseDiscardsDirtyTree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "checkout", "-b", "release")
	if err := os.WriteFile(filepath.Join(repo, "RELEASE.txt"), []byte("release marker\n"), 0o644); err != nil {
		t.Fatalf("release write: %v", err)
	}
	testutil.RunGit(t, repo, "add", "RELEASE.txt")
	testutil.RunGit(t, repo, "commit", "-m", "release marker")
	testutil.RunGit(t, repo, "checkout", "main")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-discard")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	updated, err := app.GitCreateBranchFrom(workspaceRefForThread(thread), "feature/discard", "release", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom() error = %v", err)
	}
	if updated.Branch != "feature/discard" {
		t.Fatalf("Branch = %q, want feature/discard", updated.Branch)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", stored.UpdatedAt, thread.UpdatedAt)
	}

	// README should be back to the release-base content. RELEASE.txt
	// should exist (it comes from the release branch).
	contents, err := os.ReadFile(filepath.Join(repo, "README.txt"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if strings.Contains(string(contents), "dirty edit") {
		t.Fatalf("expected dirty edit discarded; still present: %q", string(contents))
	}
	if _, err := os.Stat(filepath.Join(repo, "RELEASE.txt")); err != nil {
		t.Fatalf("expected base content RELEASE.txt: %v", err)
	}

	// The stash stack should be empty — the destructive path drops the
	// stash after a clean checkout.
	stdout, _, err := app.gitCore().Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty stash; got %q", stdout)
	}
}

func TestGitCreateBranchFromOtherBaseRejectsExistingBranchBeforeWorkspaceMutation(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "checkout", "-b", "release")
	if err := os.WriteFile(filepath.Join(repo, "RELEASE.txt"), []byte("release marker\n"), 0o644); err != nil {
		t.Fatalf("release write: %v", err)
	}
	testutil.RunGit(t, repo, "add", "RELEASE.txt")
	testutil.RunGit(t, repo, "commit", "-m", "release marker")
	testutil.RunGit(t, repo, "checkout", "main")
	testutil.RunGit(t, repo, "branch", "feature/existing")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-existing-other-base")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	readmePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readmePath, []byte("dirty edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("staged write: %v", err)
	}
	testutil.RunGit(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("untracked write: %v", err)
	}
	statusBeforeOut, _, err := app.gitCore().Execute(repo, "status", "--short")
	if err != nil {
		t.Fatalf("status before: %v", err)
	}
	statusBefore := strings.TrimSpace(statusBeforeOut)

	_, err = app.GitCreateBranchFrom(workspaceRefForThread(thread), "feature/existing", "release", false)
	if err == nil {
		t.Fatal("expected duplicate branch error")
	}
	if !strings.Contains(err.Error(), `create branch: branch "feature/existing" already exists`) {
		t.Fatalf("GitCreateBranchFrom() error = %v, want duplicate branch message", err)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("current branch = %q, want unchanged main", got)
	}
	contents, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if string(contents) != "dirty edit\n" {
		t.Fatalf("dirty tree changed; README = %q", string(contents))
	}
	statusAfterOut, _, err := app.gitCore().Execute(repo, "status", "--short")
	if err != nil {
		t.Fatalf("status after: %v", err)
	}
	if got := strings.TrimSpace(statusAfterOut); got != statusBefore {
		t.Fatalf("status changed\ngot:  %q\nwant: %q", got, statusBefore)
	}
	stashOut, _, err := app.gitCore().Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if got := strings.TrimSpace(stashOut); got != "" {
		t.Fatalf("stash list = %q, want empty", got)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "main" {
		t.Fatalf("stored Branch = %q, want unchanged main", stored.Branch)
	}
	if stored.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("stored UpdatedAt = %d, want unchanged %d", stored.UpdatedAt, thread.UpdatedAt)
	}
}

func TestGitCreateBranchFromOtherBaseAllowedDuringActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "release")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-create-branch-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	state, err := app.GitCreateBranchFrom(workspaceRefForThread(thread), "feature/allowed-other", "release", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom(other base) during active turn error = %v", err)
	}
	if state.Branch != "feature/allowed-other" {
		t.Fatalf("Branch = %q, want feature/allowed-other", state.Branch)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "feature/allowed-other" {
		t.Fatalf("branch = %q, want feature/allowed-other", got)
	}
}

// A SIBLING's turn does not gate the destructive create-branch path either:
// the user owns the branch, whatever any agent in the directory is doing.
func TestGitCreateBranchFromOtherBaseAllowedWithSiblingActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "release")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	sibling := testThread("thread-create-branch-sibling")
	sibling.ProjectID = project.ID
	sibling.WorkspacePath = repo
	sibling.Branch = "main"
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-create-branch-sibling",
		ThreadID:  sibling.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	// The caller names the directory, not the busy thread.
	state, err := app.GitCreateBranchFrom(
		WorkspaceRef{ProjectID: project.ID, WorkspacePath: repo}, "feature/allowed-sibling", "release", false)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom() during a sibling turn error = %v", err)
	}
	if state.Branch != "feature/allowed-sibling" {
		t.Fatalf("Branch = %q, want feature/allowed-sibling", state.Branch)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "feature/allowed-sibling" {
		t.Fatalf("branch = %q, want feature/allowed-sibling", got)
	}
}

func TestGitCreateBranchFromCurrentBaseAllowedDuringActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-current-active-turn")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	thread.UpdatedAt = 1_700_000_000_000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-create-branch-current-active",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn() error = %v", err)
	}

	updated, err := app.GitCreateBranchFrom(workspaceRefForThread(thread), "feature/allowed", "main", true)
	if err != nil {
		t.Fatalf("GitCreateBranchFrom(current base) during active turn should succeed, got error = %v", err)
	}
	if updated.Branch != "feature/allowed" {
		t.Fatalf("Branch = %q, want feature/allowed", updated.Branch)
	}
}

func TestGitCreateBranchFromCarryWithOtherBaseRejected(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	testutil.RunGit(t, repo, "branch", "release")

	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-create-branch-mismatch")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.GitCreateBranchFrom(workspaceRefForThread(thread), "feature/mismatch", "release", true); err == nil {
		t.Fatal("expected error when carryLocalChanges=true but base != current branch")
	}
}

func containsBranch(branches []gitops.GitBranch, want string) bool {
	for _, branch := range branches {
		if branch.Name == want {
			return true
		}
	}
	return false
}

// pruneTestFixture wires a thread to testutil.GonePruneRepo: one gone
// branch merged into main, one gone branch with commits main doesn't
// have (the squash-merge shape), and one never-pushed branch.
func pruneTestFixture(t *testing.T, app *App) store.Thread {
	t.Helper()
	repo := testutil.GonePruneRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-prune")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return thread
}

func TestGitListBranchPruneCandidatesClassifiesAndWarns(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := pruneTestFixture(t, app)

	res, err := app.GitListBranchPruneCandidates(workspaceRefForThread(thread))
	if err != nil {
		t.Fatalf("GitListBranchPruneCandidates() error = %v", err)
	}
	byName := make(map[string]BranchPruneCandidate, len(res.Candidates))
	for _, c := range res.Candidates {
		byName[c.Branch] = c
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("expected merged-gone + squashed-gone only, got %+v", res.Candidates)
	}
	if c := byName["merged-gone"]; !c.Safe || c.Reason != "merged into the default branch" {
		t.Fatalf("merged-gone should be safe, got %+v", c)
	}
	if c := byName["squashed-gone"]; c.Safe {
		t.Fatalf("squashed-gone must not pre-check without a forge match, got %+v", c)
	}
	// A file-path origin classifies to no forge; the unmerged candidate
	// forces the merged-PR lookup, whose failure must surface as a
	// warning rather than failing the preview.
	if res.ForgeWarning == "" {
		t.Fatal("expected ForgeWarning when the forge lookup is unavailable")
	}
}

// prunePreviewTip fetches the freshly classified preview and returns the
// tip it shows for branch — the value user consent is pinned to.
func prunePreviewTip(t *testing.T, app *App, ref WorkspaceRef, branch string) string {
	t.Helper()
	preview, err := app.GitListBranchPruneCandidates(ref)
	if err != nil {
		t.Fatalf("GitListBranchPruneCandidates() error = %v", err)
	}
	for _, c := range preview.Candidates {
		if c.Branch == branch {
			return c.Tip
		}
	}
	t.Fatalf("branch %s missing from preview: %+v", branch, preview.Candidates)
	return ""
}

func TestGitPruneBranchesDeletesEligibleAndRefusesRest(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := pruneTestFixture(t, app)
	mergedTip := prunePreviewTip(t, app, workspaceRefForThread(thread), "merged-gone")

	res, err := app.GitPruneBranches(workspaceRefForThread(thread), []BranchPruneSelection{
		{Branch: "merged-gone", Tip: mergedTip},
		{Branch: "local-only", Tip: "irrelevant"},
		{Branch: "main", Tip: "irrelevant"},
		{Branch: " "},
	})
	if err != nil {
		t.Fatalf("GitPruneBranches() error = %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "merged-gone" {
		t.Fatalf("expected only merged-gone deleted, got %+v", res)
	}
	if res.Failed["local-only"] == "" {
		t.Fatalf("never-pushed branch must be refused, got %+v", res.Failed)
	}
	if res.Failed["main"] == "" {
		t.Fatalf("default branch must be refused, got %+v", res.Failed)
	}

	branches, err := app.GitListBranches(workspaceRefForThread(thread))
	if err != nil {
		t.Fatalf("GitListBranches() error = %v", err)
	}
	for _, b := range branches {
		if b.Name == "merged-gone" {
			t.Fatal("merged-gone should be deleted")
		}
	}
	if !containsBranch(branches, "local-only") {
		t.Fatal("local-only must survive the prune")
	}
}

func TestGitPruneBranchesRefusesMovedTip(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := pruneTestFixture(t, app)
	staleTip := prunePreviewTip(t, app, workspaceRefForThread(thread), "squashed-gone")

	// A commit lands on the branch between preview and confirm — the
	// branch is still gone-upstream and unattached, but the consented
	// tip no longer matches.
	repo := thread.WorkspacePath
	testutil.RunGit(t, repo, "checkout", "squashed-gone")
	if err := os.WriteFile(filepath.Join(repo, "late.txt"), []byte("late"), 0o644); err != nil {
		t.Fatalf("write late.txt: %v", err)
	}
	testutil.RunGit(t, repo, "add", "late.txt")
	testutil.RunGit(t, repo, "commit", "-m", "late work")
	testutil.RunGit(t, repo, "checkout", "main")

	res, err := app.GitPruneBranches(workspaceRefForThread(thread), []BranchPruneSelection{
		{Branch: "squashed-gone", Tip: staleTip},
	})
	if err != nil {
		t.Fatalf("GitPruneBranches() error = %v", err)
	}
	if len(res.Deleted) != 0 {
		t.Fatalf("moved-tip branch must not delete, got %+v", res)
	}
	if res.Failed["squashed-gone"] == "" {
		t.Fatalf("expected a moved-tip refusal, got %+v", res.Failed)
	}

	branches, err := app.GitListBranches(workspaceRefForThread(thread))
	if err != nil {
		t.Fatalf("GitListBranches() error = %v", err)
	}
	if !containsBranch(branches, "squashed-gone") {
		t.Fatal("squashed-gone must survive a stale-tip prune attempt")
	}
}

func TestGitPruneBranchesReportsDuplicateSelectionOnce(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := pruneTestFixture(t, app)
	mergedTip := prunePreviewTip(t, app, workspaceRefForThread(thread), "merged-gone")

	res, err := app.GitPruneBranches(workspaceRefForThread(thread), []BranchPruneSelection{
		{Branch: "merged-gone", Tip: mergedTip},
		{Branch: "merged-gone", Tip: mergedTip},
	})
	if err != nil {
		t.Fatalf("GitPruneBranches() error = %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "merged-gone" {
		t.Fatalf("duplicate selection must delete once, got %+v", res)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("a deleted branch must not also report as failed, got %+v", res.Failed)
	}
}
