package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// discardFixture is a project whose run tree owns real checkouts, which is the
// only way to test a flow whose whole job is destroying them.
type discardFixture struct {
	app     *App
	repo    string
	project store.Project
	root    store.WorkItem
}

func newDiscardFixture(t *testing.T, itemID string) *discardFixture {
	t.Helper()
	app, _ := setupE2EApp(t)
	// startWorkflowEngineForTest, not a bare initWorkflowEngine: engine startup
	// also arms the §11 scheduler, and only this helper stops it again.
	startWorkflowEngineForTest(t, app, t.TempDir())
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	return &discardFixture{
		app: app, repo: repo, project: projectRow,
		root: createDoneWorkflowWorktree(t, app, projectRow, itemID),
	}
}

// addUnitWorktree gives a run a fan-out unit checkout branched off the run's own
// branch, with one commit of its own.
func (f *discardFixture) addUnitWorktree(t *testing.T, item store.WorkItem, unitID string) store.WorkItemUnit {
	t.Helper()
	branch := "workflow-" + item.ID + "-" + unitID
	path := filepath.Join(t.TempDir(), unitID)
	if err := f.app.gitCore().CreateWorktreeFromBranch(f.project.Path, path, item.Branch, branch); err != nil {
		t.Fatal(err)
	}
	writeDispositionFile(t, path, unitID+".txt", "unit work\n")
	testutil.RunGit(t, path, "add", ".")
	testutil.RunGit(t, path, "commit", "-m", "unit "+unitID)
	unit := store.WorkItemUnit{
		ItemID: item.ID, PhaseID: "fan", Attempt: 1, UnitID: unitID, UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitDone,
		Branch: branch, WorktreePath: path,
	}
	if err := f.app.store.CreateWorkItemUnits([]store.WorkItemUnit{unit}); err != nil {
		t.Fatal(err)
	}
	return unit
}

// addChild links a called run that shares its caller's checkout, the way §9
// workspace propagation leaves it.
func (f *discardFixture) addChild(t *testing.T, parent store.WorkItem, id string, state engine.State) store.WorkItem {
	t.Helper()
	child := store.WorkItem{
		ID: id, ProjectID: f.project.ID, Goal: "called " + id, WorkflowID: "child",
		WorkflowScope: "shared", State: string(state), Source: "call",
		ParentItemID: parent.ID, ParentPhaseID: "call", ParentAttempt: 1,
		CallDepth: parent.CallDepth + 1, WorktreePath: parent.WorktreePath,
		Branch: parent.Branch, BaseBranch: parent.BaseBranch,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := f.app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}
	return child
}

func (f *discardFixture) branchExists(t *testing.T, branch string) bool {
	t.Helper()
	branches, err := f.app.gitCore().ListBranches(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range branches {
		if candidate.Name == branch {
			return true
		}
	}
	return false
}

func findDiscardWorktree(t *testing.T, preview WorkflowDiscardPreview, path string) WorkflowDiscardWorktree {
	t.Helper()
	for _, worktree := range preview.Worktrees {
		if gitops.SameFilesystemPath(worktree.Path, path) {
			return worktree
		}
	}
	t.Fatalf("preview has no row for %q; rows = %+v", path, preview.Worktrees)
	return WorkflowDiscardWorktree{}
}

func TestWorkflowDiscardPreviewReportsWhatWouldBeLost(t *testing.T) {
	f := newDiscardFixture(t, "preview-item")
	// One committed change (unmerged against main) plus uncommitted work that
	// exists nowhere but this checkout.
	writeDispositionFile(t, f.root.WorktreePath, "dirty.txt", "unsaved\n")
	writeDispositionFile(t, f.root.WorktreePath, "README.txt", "edited\n")

	preview, err := f.app.WorkflowDiscardPreview(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Members) != 1 || preview.Members[0] != f.root.ID {
		t.Fatalf("members = %v, want just the root", preview.Members)
	}
	if len(preview.LiveMembers) != 0 {
		t.Fatalf("live members = %v, want none", preview.LiveMembers)
	}
	row := findDiscardWorktree(t, preview, f.root.WorktreePath)
	if !row.Present || !row.Registered {
		t.Fatalf("row = %+v, want a present registered worktree", row)
	}
	if row.Branch != f.root.Branch || row.Base != "main" {
		t.Fatalf("row branch/base = %q/%q, want %q/main", row.Branch, row.Base, f.root.Branch)
	}
	if row.DirtyFileCount != 2 {
		t.Fatalf("dirty count = %d, want 2 (one new, one edited): %v", row.DirtyFileCount, row.DirtyFiles)
	}
	if !strings.Contains(strings.Join(row.DirtyFiles, " "), "dirty.txt") {
		t.Fatalf("dirty files = %v, want dirty.txt named", row.DirtyFiles)
	}
	if row.UnmergedCommitCount != 1 || len(row.UnmergedCommits) != 1 {
		t.Fatalf("unmerged commits = %d (%d listed), want 1", row.UnmergedCommitCount, len(row.UnmergedCommits))
	}
	if row.Error != "" {
		t.Fatalf("preview row reported an inspection error: %s", row.Error)
	}
	// A preview mutates nothing.
	if _, err := os.Stat(f.root.WorktreePath); err != nil {
		t.Fatalf("preview removed the worktree: %v", err)
	}
	if !f.branchExists(t, f.root.Branch) {
		t.Fatal("preview deleted the branch")
	}
}

func TestWorkflowDiscardPreviewDedupesSharedCheckoutsAndScopesUnitCommits(t *testing.T) {
	f := newDiscardFixture(t, "tree-item")
	child := f.addChild(t, f.root, "tree-child", engine.StateDone)
	f.addChild(t, child, "tree-grandchild", engine.StateDone)
	unit := f.addUnitWorktree(t, f.root, "unit-a")

	preview, err := f.app.WorkflowDiscardPreview(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Members) != 3 {
		t.Fatalf("members = %v, want the whole tree", preview.Members)
	}
	// Three runs share one checkout; the loss is reported once.
	if len(preview.Worktrees) != 2 {
		t.Fatalf("worktree rows = %d, want 2 (shared run checkout + unit): %+v",
			len(preview.Worktrees), preview.Worktrees)
	}
	unitRow := findDiscardWorktree(t, preview, unit.WorktreePath)
	if unitRow.UnitID != unit.UnitID || unitRow.Base != f.root.Branch {
		t.Fatalf("unit row = %+v, want it measured against the run branch %q", unitRow, f.root.Branch)
	}
	// The unit's own commit only — the run's commit is already on the unit's
	// base, so counting it again would overstate the loss.
	if unitRow.UnmergedCommitCount != 1 {
		t.Fatalf("unit unmerged commits = %d, want just the unit's own", unitRow.UnmergedCommitCount)
	}
}

func TestWorkflowDiscardRefusesACalledRun(t *testing.T) {
	f := newDiscardFixture(t, "refuse-item")
	child := f.addChild(t, f.root, "refuse-child", engine.StateDone)

	if _, err := f.app.WorkflowDiscardPreview(child.ID); err == nil ||
		!strings.Contains(err.Error(), "discard the run that called it") {
		t.Fatalf("preview of a called run = %v, want a refusal", err)
	}
	if _, err := f.app.WorkflowDiscardItem(child.ID); err == nil {
		t.Fatal("discarding a called run succeeded; it would delete its caller's branch")
	}
	if !f.branchExists(t, f.root.Branch) {
		t.Fatal("a refused discard deleted the caller's branch")
	}
}

func TestWorkflowDiscardRemovesEveryCheckoutAndBranchInTheTree(t *testing.T) {
	f := newDiscardFixture(t, "discard-item")
	child := f.addChild(t, f.root, "discard-child", engine.StateDone)
	unit := f.addUnitWorktree(t, f.root, "unit-a")
	writeDispositionFile(t, f.root.WorktreePath, "dirty.txt", "unsaved\n")

	receipt, err := f.app.WorkflowDiscardItem(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != "discarded" || receipt.Discarded == nil {
		t.Fatalf("receipt = %+v, want a discard result", receipt)
	}
	if len(receipt.Discarded.Members) != 2 {
		t.Fatalf("receipt members = %v, want root and child", receipt.Discarded.Members)
	}
	if len(receipt.Discarded.Cancelled) != 0 {
		t.Fatalf("receipt cancelled = %v, want none", receipt.Discarded.Cancelled)
	}
	if len(receipt.Discarded.RemovedWorktrees) != 2 || len(receipt.Discarded.DeletedBranches) != 2 {
		t.Fatalf("receipt removals = %+v, want both checkouts and both branches", receipt.Discarded)
	}
	for _, path := range []string{f.root.WorktreePath, unit.WorktreePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("checkout %q survived the discard (stat err = %v)", path, err)
		}
	}
	for _, branch := range []string{f.root.Branch, unit.Branch} {
		if f.branchExists(t, branch) {
			t.Fatalf("branch %q survived the discard", branch)
		}
	}
	// The run record is durable; only its workspace pointers are dropped.
	for _, id := range []string{f.root.ID, child.ID} {
		stored, err := f.app.store.GetWorkItem(id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.WorktreePath != "" {
			t.Fatalf("run %s still points at %q", id, stored.WorktreePath)
		}
	}
	units, err := f.app.store.ListWorkItemUnits(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].WorktreePath != "" {
		t.Fatalf("unit rows = %+v, want the worktree pointer cleared", units)
	}
	stored, err := f.app.store.GetWorkItem(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted WorkflowDispositionReceipt
	if err := json.Unmarshal(stored.Disposition, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Discarded == nil || len(persisted.Discarded.DeletedBranches) != 2 {
		t.Fatalf("persisted receipt = %+v, want the branch deletions recorded", persisted)
	}
}

// The project root is never a discard target: a run that worked directly in the
// user's checkout has nothing of its own to destroy.
func TestWorkflowDiscardNeverTouchesTheProjectCheckout(t *testing.T) {
	f := newDiscardFixture(t, "root-workspace-item")
	if err := f.app.store.UpdateWorkItemWorkspace(f.root.ID, f.project.Path, "main", "main"); err != nil {
		t.Fatal(err)
	}
	item, err := f.app.store.GetWorkItem(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}

	preview, err := f.app.WorkflowDiscardPreview(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Offering the user's own checkout as a loss would be consent for something
	// the discard will not (and must not) do.
	if len(preview.Worktrees) != 0 {
		t.Fatalf("preview lists the project checkout as a loss: %+v", preview.Worktrees)
	}
	if _, err := f.app.WorkflowDiscardItem(item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.project.Path, ".git")); err != nil {
		t.Fatalf("discard damaged the project checkout: %v", err)
	}
	if !f.branchExists(t, "main") {
		t.Fatal("discard deleted the project's own branch")
	}
}

// A member the engine cannot stop is a refusal, not a best-effort deletion:
// removing a checkout out from under a live provider process is exactly the
// failure discard's preview-then-commit shape exists to prevent.
func TestWorkflowDiscardRefusesWhenALiveMemberCannotBeStopped(t *testing.T) {
	f := newDiscardFixture(t, "inflight-item")
	child := f.addChild(t, f.root, "inflight-child", engine.StateRunning)

	if _, err := f.app.WorkflowDiscardItem(f.root.ID); err == nil {
		t.Fatal("discard succeeded with a live member in the tree")
	} else if !strings.Contains(err.Error(), child.ID) {
		t.Fatalf("error = %v, want it to name the live run %s", err, child.ID)
	}
	if _, err := os.Stat(f.root.WorktreePath); err != nil {
		t.Fatalf("refused discard removed the checkout anyway: %v", err)
	}
	if !f.branchExists(t, f.root.Branch) {
		t.Fatal("refused discard deleted the branch anyway")
	}
	stored, err := f.app.store.GetWorkItem(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Disposition) != 0 {
		t.Fatalf("refused discard persisted a receipt: %s", stored.Disposition)
	}
}
