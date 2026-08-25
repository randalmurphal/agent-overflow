package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/worktreesetup"
)

// projectWorkspaceTestApp is the shared fixture: an isolated App, a real git
// repo, and the project row pointing at it. No threads — the whole point of
// this surface is that a draft can provision a workspace without one.
func projectWorkspaceTestApp(t *testing.T) (*App, string, store.Project) {
	t.Helper()
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	return app, repo, project
}

func assertNoThreadRows(t *testing.T, app *App) {
	t.Helper()
	refs, err := app.store.ListThreadWorkspaceRefs()
	if err != nil {
		t.Fatalf("ListThreadWorkspaceRefs() error = %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("project-scoped workspace ops must not materialize a thread; got %d rows", len(refs))
	}
}

// --- PrepareProjectWorktree ---

func TestPrepareProjectWorktreeCutsWithoutAThreadRow(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/draft", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}
	if result.Branch != "feature/draft" {
		t.Fatalf("Branch = %q, want feature/draft", result.Branch)
	}
	if result.WorktreePath == "" {
		t.Fatal("WorktreePath empty")
	}
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Fatalf("expected worktree on disk: %v", err)
	}
	if _, ok, err := app.findWorktree(repo, result.WorktreePath); err != nil || !ok {
		t.Fatalf("worktree not registered with the repo (ok=%v err=%v)", ok, err)
	}
	assertNoThreadRows(t, app)
}

func TestPrepareProjectWorktreeCarriesLocalChanges(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	// A tracked edit AND an untracked file, so the stash push -u path is the
	// one under test (stash create would miss the untracked file).
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nedited\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	result, err := app.PrepareProjectWorktree(project.ID, "main", "feature/draft-carry", true, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}
	carried, err := os.ReadFile(filepath.Join(result.WorktreePath, "README.txt"))
	if err != nil {
		t.Fatalf("read carried tracked file: %v", err)
	}
	if !strings.Contains(string(carried), "edited") {
		t.Fatalf("tracked edit did not carry; got %q", string(carried))
	}
	if _, err := os.Stat(filepath.Join(result.WorktreePath, "scratch.txt")); err != nil {
		t.Fatalf("untracked file did not carry: %v", err)
	}
	// The carry drops the entry once applied, so the project root's stash
	// stack must be empty — a leftover entry is silent data the user has to
	// find themselves.
	stdout, _, err := app.gitCore().Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty stash; got %q", stdout)
	}
	assertNoThreadRows(t, app)
}

func TestPrepareProjectWorktreeRejectsCarryFromDifferentBase(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)
	testutil.RunGit(t, repo, "branch", "release")

	if _, err := app.PrepareProjectWorktree(project.ID, "release", "feature/mismatch", true, ""); err == nil {
		t.Fatal("expected a refusal when carryLocalChanges=true but base != current branch")
	}
}

// A draft pane can already be parked in a worktree. The carry must stash from
// THAT checkout, not from the project root — stashing the root would take work
// the user never pointed at and leave the worktree's own edits behind.
func TestPrepareProjectWorktreeCarriesFromTheSourceWorktree(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	source, err := app.PrepareProjectWorktree(project.ID, "", "feature/source", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree(source) error = %v", err)
	}
	// Dirty BOTH checkouts, with different content, so the assertion can only
	// pass if the right one was stashed.
	if err := os.WriteFile(filepath.Join(source.WorktreePath, "carry.txt"), []byte("from the worktree\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "carry.txt"), []byte("from the root\n"), 0o644); err != nil {
		t.Fatalf("write root file: %v", err)
	}

	// baseBranch empty resolves against the SOURCE's current branch, which is
	// feature/source — the project root is still on main, so a root-resolved
	// base would refuse this call outright.
	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/carried", true, source.WorktreePath)
	if err != nil {
		t.Fatalf("PrepareProjectWorktree(carry from worktree) error = %v", err)
	}
	carried, err := os.ReadFile(filepath.Join(result.WorktreePath, "carry.txt"))
	if err != nil {
		t.Fatalf("read carried file: %v", err)
	}
	if strings.TrimSpace(string(carried)) != "from the worktree" {
		t.Fatalf("carried content = %q, want the source worktree's copy", string(carried))
	}
	// The root was never touched.
	root, err := os.ReadFile(filepath.Join(repo, "carry.txt"))
	if err != nil {
		t.Fatalf("project root file went missing: %v", err)
	}
	if strings.TrimSpace(string(root)) != "from the root" {
		t.Fatalf("project root content = %q; the carry stashed the wrong tree", string(root))
	}
	assertNoThreadRows(t, app)
}

// A carry empties the source checkout, so it takes the same occupancy refusal
// moving a HEAD does — otherwise a draft's "Local with changes" would yank
// uncommitted work out from under a sibling thread's running turn.
func TestPrepareProjectWorktreeRefusesCarryFromBusyWorkspace(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	occupant := testThread("thread-carry-busy")
	occupant.ProjectID = project.ID
	occupant.WorkspacePath = repo
	occupant.Branch = "main"
	if err := app.store.CreateThread(occupant); err != nil {
		t.Fatalf("CreateThread(occupant): %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-carry-busy",
		ThreadID:  occupant.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("mid-turn work\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	if _, err := app.PrepareProjectWorktree(project.ID, "", "feature/carry-blocked", true, ""); err == nil {
		t.Fatal("expected the carry path to refuse while an occupant is mid-turn")
	} else if !strings.Contains(err.Error(), occupant.ID) {
		t.Fatalf("error should name the busy occupant: got %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "scratch.txt")); err != nil {
		t.Fatalf("the refusal must not have stashed anything: %v", err)
	}

	// A cut WITHOUT carry touches nobody's working tree, so it stays available.
	if _, err := app.PrepareProjectWorktree(project.ID, "", "feature/carry-free", false, ""); err != nil {
		t.Fatalf("a non-carrying cut must stay available: %v", err)
	}
}

// The source workspace is a cwd for destructive git. Anything that is not the
// project root or one of its registered worktrees must never reach it.
func TestProjectWorkspaceOpsRefuseForeignSourceWorkspace(t *testing.T) {
	app, _, project := projectWorkspaceTestApp(t)
	foreign := t.TempDir()

	if _, err := app.PrepareProjectWorktree(project.ID, "", "feature/foreign", false, foreign); err == nil {
		t.Fatal("expected PrepareProjectWorktree to refuse a foreign source workspace")
	}
	if _, err := app.CreateProjectBranch(project.ID, "feature/foreign-branch", "", false, foreign); err == nil {
		t.Fatal("expected CreateProjectBranch to refuse a foreign source workspace")
	}
}

// CreateProjectBranch checks out in the SOURCE workspace: a draft in a worktree
// branching "here" must move that worktree's HEAD, not the project root's.
func TestCreateProjectBranchChecksOutInTheSourceWorktree(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	source, err := app.PrepareProjectWorktree(project.ID, "", "feature/branch-source", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree(source) error = %v", err)
	}

	result, err := app.CreateProjectBranch(project.ID, "feature/in-worktree", "", false, source.WorktreePath)
	if err != nil {
		t.Fatalf("CreateProjectBranch() error = %v", err)
	}
	if result.Branch != "feature/in-worktree" {
		t.Fatalf("Branch = %q, want feature/in-worktree", result.Branch)
	}
	if got := app.gitCore().CurrentBranch(source.WorktreePath); got != "feature/in-worktree" {
		t.Fatalf("source worktree is on %q, want feature/in-worktree", got)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project root moved to %q; the branch was checked out in the wrong tree", got)
	}
	assertNoThreadRows(t, app)
}

// --- AttachProjectWorktree ---

func TestAttachProjectWorktreeAttachesExistingBranch(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)
	testutil.RunGit(t, repo, "branch", "feature/attach-draft")

	result, err := app.AttachProjectWorktree(project.ID, "feature/attach-draft")
	if err != nil {
		t.Fatalf("AttachProjectWorktree() error = %v", err)
	}
	if result.Branch != "feature/attach-draft" {
		t.Fatalf("Branch = %q, want feature/attach-draft", result.Branch)
	}
	worktree, ok, err := app.findWorktree(repo, result.WorktreePath)
	if err != nil || !ok {
		t.Fatalf("worktree not registered with the repo (ok=%v err=%v)", ok, err)
	}
	if worktree.Branch != "feature/attach-draft" {
		t.Fatalf("registered branch = %q, want feature/attach-draft", worktree.Branch)
	}
	assertNoThreadRows(t, app)
}

// git's one-branch-one-worktree invariant is the refusal, not a check of ours:
// the project root already has `main` checked out.
func TestAttachProjectWorktreeRefusesBranchCheckedOutElsewhere(t *testing.T) {
	app, _, project := projectWorkspaceTestApp(t)

	if _, err := app.AttachProjectWorktree(project.ID, "main"); err == nil {
		t.Fatal("expected attach to refuse a branch already checked out in the project root")
	}
}

// --- CreateProjectBranch ---

func TestCreateProjectBranchFromCurrentKeepsWorkingTree(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	result, err := app.CreateProjectBranch(project.ID, "feature/from-current", "", false, "")
	if err != nil {
		t.Fatalf("CreateProjectBranch() error = %v", err)
	}
	if result.Branch != "feature/from-current" {
		t.Fatalf("Branch = %q, want feature/from-current", result.Branch)
	}
	if result.WorktreePath != "" {
		t.Fatalf("WorktreePath = %q, want empty for a branch in the project root", result.WorktreePath)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "feature/from-current" {
		t.Fatalf("project root is on %q, want feature/from-current", got)
	}
	// base == current means the working tree comes along.
	if _, err := os.Stat(filepath.Join(repo, "scratch.txt")); err != nil {
		t.Fatalf("uncommitted file should stay attached: %v", err)
	}
	assertNoThreadRows(t, app)
}

func TestCreateProjectBranchFromOtherBaseDiscardsWorkingTree(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)

	testutil.RunGit(t, repo, "checkout", "-b", "release")
	if err := os.WriteFile(filepath.Join(repo, "BASE.txt"), []byte("release\n"), 0o644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	testutil.RunGit(t, repo, "add", "BASE.txt")
	testutil.RunGit(t, repo, "commit", "-m", "release base")
	testutil.RunGit(t, repo, "checkout", "main")

	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("discard me\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	result, err := app.CreateProjectBranch(project.ID, "feature/from-release", "release", false, "")
	if err != nil {
		t.Fatalf("CreateProjectBranch() error = %v", err)
	}
	if result.Branch != "feature/from-release" {
		t.Fatalf("Branch = %q, want feature/from-release", result.Branch)
	}
	// Branched off release, so release's commit is present...
	if _, err := os.Stat(filepath.Join(repo, "BASE.txt")); err != nil {
		t.Fatalf("expected the new branch to start at release: %v", err)
	}
	// ...and the stash the destructive path pushed was dropped, not left behind.
	stdout, _, err := app.gitCore().Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected the discarded stash to be dropped; got %q", stdout)
	}
}

// Moving the SHARED project root's HEAD under a thread that is mid-turn is the
// one thing this path must refuse — the provider session is bound to that cwd.
func TestCreateProjectBranchRefusesBusyWorkspace(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)
	testutil.RunGit(t, repo, "branch", "release")

	occupant := testThread("thread-project-root-busy")
	occupant.ProjectID = project.ID
	occupant.WorkspacePath = repo
	occupant.Branch = "main"
	if err := app.store.CreateThread(occupant); err != nil {
		t.Fatalf("CreateThread(occupant): %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "turn-project-root-busy",
		ThreadID:  occupant.ID,
		TurnIndex: 0,
		StartedAt: 1,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	_, err := app.CreateProjectBranch(project.ID, "feature/blocked", "release", false, "")
	if err == nil {
		t.Fatal("expected the destructive branch path to refuse while an occupant is mid-turn")
	}
	if !strings.Contains(err.Error(), occupant.ID) {
		t.Fatalf("error should name the busy occupant: got %v", err)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project root moved to %q despite the refusal", got)
	}

	// The non-destructive path does NOT move anyone's base out from under
	// them, so it stays available while the same thread is busy.
	if _, err := app.CreateProjectBranch(project.ID, "feature/allowed", "", false, ""); err != nil {
		t.Fatalf("branching off the current branch must stay available: %v", err)
	}
}

// --- Workspace-keyed setup runs ---

// seedProjectRecipe points the project's worktree setup at config.
func seedProjectRecipe(t *testing.T, app *App, projectID string, config *worktreesetup.Config) {
	t.Helper()
	if err := app.store.UpdateProjectWorktreeSetup(projectID, config); err != nil {
		t.Fatalf("UpdateProjectWorktreeSetup: %v", err)
	}
}

func waitForWorkspaceSetupState(t *testing.T, app *App, worktreePath, want string) WorktreeSetupRunState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last WorktreeSetupRunState
	for time.Now().Before(deadline) {
		app.worktreeSetup.mu.Lock()
		run, _ := app.findWorkspaceSetupRunLocked(worktreePath)
		app.worktreeSetup.mu.Unlock()
		if run != nil {
			select {
			case <-run.done:
				last = app.worktreeSetupRunState(run)
				if last.State == want {
					return last
				}
			default:
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workspace setup state = %q, want %q", last.State, want)
	return WorktreeSetupRunState{}
}

// A failed run with no thread to report against is RETAINED under its workspace
// key: the draft that asked for the worktree still exists in the composer, and
// the panel has to be able to show it (and offer Retry) before any row does.
func TestUnboundWorktreeSetupFailureIsRetainedAndRetryable(t *testing.T) {
	app, _, project := projectWorkspaceTestApp(t)
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "echo diagnosis >&2; exit 3"}},
	})

	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/unbound-fail", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}
	waitForWorkspaceSetupState(t, app, result.WorktreePath, worktreeSetupRunFailed)

	snapshot, err := app.GetWorkspaceWorktreeSetup(project.ID, result.WorktreePath)
	if err != nil {
		t.Fatalf("GetWorkspaceWorktreeSetup() error = %v", err)
	}
	if snapshot.State != worktreeSetupRunFailed {
		t.Fatalf("snapshot state = %q, want failed", snapshot.State)
	}
	if snapshot.ThreadID != "" {
		t.Fatalf("snapshot threadId = %q, want empty for an unbound run", snapshot.ThreadID)
	}
	if snapshot.WorktreePath == "" {
		t.Fatal("snapshot worktreePath is empty; it is the only key an unbound client has")
	}
	if !strings.Contains(snapshot.Output, "diagnosis") {
		t.Fatalf("snapshot output = %q, want the failing command's stderr", snapshot.Output)
	}
	assertNoThreadRows(t, app)

	// Retry re-reads the recipe, so fixing it in Settings and retrying works.
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "true"}},
	})
	if err := app.RetryWorkspaceWorktreeSetup(project.ID, result.WorktreePath); err != nil {
		t.Fatalf("RetryWorkspaceWorktreeSetup() error = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot, err := app.GetWorkspaceWorktreeSetup(project.ID, result.WorktreePath)
		if err != nil {
			t.Fatalf("GetWorkspaceWorktreeSetup() error = %v", err)
		}
		if snapshot.State == worktreeSetupRunIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retried run never settled; state = %q", snapshot.State)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A path that is not one of the project's worktrees must never become a cwd
	// for the project's argv recipe.
	if err := app.RetryWorkspaceWorktreeSetup(project.ID, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Fatal("expected retry to refuse a path that is not a worktree of the project")
	}
}

// The adoption handoff: a run that failed before any thread existed is re-keyed
// onto the thread created into its worktree, and the durable column is stamped
// from the state the run had already reached.
func TestCreateThreadAdoptsUnboundWorktreeSetup(t *testing.T) {
	app, _, project := projectWorkspaceTestApp(t)
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "echo diagnosis >&2; exit 3"}},
	})

	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/adopt", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}
	before := waitForWorkspaceSetupState(t, app, result.WorktreePath, worktreeSetupRunFailed)

	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID:    project.ID,
		WorktreePath: result.WorktreePath,
		Branch:       result.Branch,
	})
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.WorktreeSetupState != store.WorktreeSetupStateFailed {
		t.Fatalf("worktree_setup_state = %q, want failed after adoption", thread.WorktreeSetupState)
	}

	// The record moved: the workspace key is empty and the thread key answers.
	app.worktreeSetup.mu.Lock()
	stranded, _ := app.findWorkspaceSetupRunLocked(result.WorktreePath)
	adopted := app.worktreeSetup.runs[thread.ID]
	app.worktreeSetup.mu.Unlock()
	if stranded != nil {
		t.Fatal("a run stayed registered under the workspace key after adoption")
	}
	if adopted == nil {
		t.Fatal("no run registered under the adopting thread's id")
	}

	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup() error = %v", err)
	}
	if snapshot.State != worktreeSetupRunFailed {
		t.Fatalf("thread snapshot state = %q, want failed", snapshot.State)
	}
	if snapshot.RunID != before.RunID {
		t.Fatalf("thread snapshot runId = %q, want the adopted run %q", snapshot.RunID, before.RunID)
	}
	if snapshot.ThreadID != thread.ID {
		t.Fatalf("thread snapshot threadId = %q, want %q", snapshot.ThreadID, thread.ID)
	}
	if !strings.Contains(snapshot.Output, "diagnosis") {
		t.Fatalf("adopted output = %q, want the failing command's stderr", snapshot.Output)
	}

	// And the workspace-keyed RPC no longer claims it — the run has an owner.
	workspaceSnapshot, err := app.GetWorkspaceWorktreeSetup(project.ID, result.WorktreePath)
	if err != nil {
		t.Fatalf("GetWorkspaceWorktreeSetup() error = %v", err)
	}
	if workspaceSnapshot.State != worktreeSetupRunIdle {
		t.Fatalf("workspace snapshot state = %q, want idle after adoption", workspaceSnapshot.State)
	}
}

// A still-running unbound run is adopted mid-flight: the column says "running"
// the moment the row exists, so a client reading the thread never sees a gap.
func TestCreateThreadAdoptsRunningUnboundWorktreeSetup(t *testing.T) {
	app, _, project := projectWorkspaceTestApp(t)
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "sleep 30"}},
	})

	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/adopt-running", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}

	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID:    project.ID,
		WorktreePath: result.WorktreePath,
		Branch:       result.Branch,
	})
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.WorktreeSetupState != store.WorktreeSetupStateRunning {
		t.Fatalf("worktree_setup_state = %q, want running after adopting a live run", thread.WorktreeSetupState)
	}
	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup() error = %v", err)
	}
	if snapshot.State != worktreeSetupRunRunning || snapshot.ThreadID != thread.ID {
		t.Fatalf("thread snapshot = %+v, want a running run bound to %s", snapshot, thread.ID)
	}

	// Adoption freed the WORKSPACE key, but the recipe is still executing in
	// that directory. A retry through the workspace RPC must refuse on the
	// directory, not on the key — two recipes in one checkout race each other's
	// writes.
	err = app.RetryWorkspaceWorktreeSetup(project.ID, result.WorktreePath)
	if err == nil {
		t.Fatal("expected the workspace retry to refuse while the adopted run is still executing there")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("refusal = %v, want an already-running refusal", err)
	}

	// Cancelling through the ordinary thread path proves the record is fully
	// an ordinary bound run now, and keeps the sleep from outliving the test.
	app.cancelThreadWorktreeSetup(thread.ID)
}

// The project root is one of git's registered worktrees, so membership alone
// would let the recipe run over the user's primary checkout.
func TestRetryWorkspaceWorktreeSetupRefusesProjectRoot(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "true"}},
	})

	if err := app.RetryWorkspaceWorktreeSetup(project.ID, repo); err == nil {
		t.Fatal("expected retry to refuse the project root")
	}
}

// projectID is not decoration on the snapshot RPC either: without the
// membership check the pair is an oracle for any directory a caller names.
func TestGetWorkspaceWorktreeSetupRefusesForeignPath(t *testing.T) {
	app, _, project := projectWorkspaceTestApp(t)

	if _, err := app.GetWorkspaceWorktreeSetup(project.ID, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Fatal("expected the snapshot RPC to refuse a path outside the project")
	}
}

// Binding at SEND time moves an existing row into the worktree instead of
// creating one, so the switch has to adopt the unbound run exactly as
// CreateThread does. It is also what a manual EnvPicker switch into a
// pre-cut worktree goes through.
func TestSwitchThreadWorkspaceAdoptsUnboundWorktreeSetup(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "sleep 30"}},
	})

	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/switch-adopt", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}

	thread := testThread("thread-switch-adopt")
	thread.ProjectID = project.ID
	thread.ProjectPath = repo
	thread.WorkspacePath = repo
	thread.Branch = "main"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	switched, err := app.switchThreadWorkspace(thread.ID, result.WorktreePath)
	if err != nil {
		t.Fatalf("switchThreadWorkspace() error = %v", err)
	}
	if switched.WorktreeSetupState != store.WorktreeSetupStateRunning {
		t.Fatalf("worktree_setup_state = %q, want running after adopting a live run", switched.WorktreeSetupState)
	}

	app.worktreeSetup.mu.Lock()
	stranded, _ := app.findWorkspaceSetupRunLocked(result.WorktreePath)
	adopted := app.worktreeSetup.runs[thread.ID]
	app.worktreeSetup.mu.Unlock()
	if stranded != nil {
		t.Fatal("a run stayed registered under the workspace key after the switch adopted it")
	}
	if adopted == nil {
		t.Fatal("no run registered under the switching thread's id")
	}
	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup() error = %v", err)
	}
	if snapshot.State != worktreeSetupRunRunning || snapshot.ThreadID != thread.ID {
		t.Fatalf("thread snapshot = %+v, want a running run bound to %s", snapshot, thread.ID)
	}

	app.cancelThreadWorktreeSetup(thread.ID)
}

// Removing a worktree must join the recipe still writing into it, even when
// that recipe belongs to a draft nothing has attached to yet.
func TestRemoveProjectWorktreeCancelsUnboundSetup(t *testing.T) {
	app, repo, project := projectWorkspaceTestApp(t)
	seedProjectRecipe(t, app, project.ID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "sleep 30"}},
	})

	result, err := app.PrepareProjectWorktree(project.ID, "", "feature/unbound-remove", false, "")
	if err != nil {
		t.Fatalf("PrepareProjectWorktree() error = %v", err)
	}
	app.worktreeSetup.mu.Lock()
	run, _ := app.findWorkspaceSetupRunLocked(result.WorktreePath)
	app.worktreeSetup.mu.Unlock()
	if run == nil {
		t.Fatal("no unbound run registered for the freshly cut worktree")
	}

	if err := app.removeProjectWorktree(repo, "", result.WorktreePath, true); err != nil {
		t.Fatalf("removeProjectWorktree() error = %v", err)
	}
	select {
	case <-run.done:
	default:
		t.Fatal("removal returned while the unbound recipe was still running in the deleted directory")
	}
	app.worktreeSetup.mu.Lock()
	leftover, _ := app.findWorkspaceSetupRunLocked(result.WorktreePath)
	app.worktreeSetup.mu.Unlock()
	if leftover != nil {
		t.Fatal("the unbound run's record outlived the worktree it describes")
	}
}
