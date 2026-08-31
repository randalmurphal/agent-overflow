package app

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
)

// projectDeleteFixture is a project that owns real workflow work: a run tree
// with checkouts and branches of its own, a phase thread, an effect row, an
// automation, and an ordinary chat thread alongside them. Deleting it is the
// only way to test a flow whose whole job is tearing that set down.
type projectDeleteFixture struct {
	*discardFixture
	child      store.WorkItem
	unit       store.WorkItemUnit
	phaseID    string
	chatThread store.Thread
	automation store.Automation
}

func newProjectDeleteFixture(t *testing.T, itemID string) *projectDeleteFixture {
	t.Helper()
	base := newDiscardFixture(t, itemID)
	fixture := &projectDeleteFixture{
		discardFixture: base,
		child:          base.addChild(t, base.root, itemID+"-child", engine.StateDone),
		unit:           base.addUnitWorktree(t, base.root, "unit-a"),
		phaseID:        "run",
	}

	chatThread, err := base.app.CreateThread(CreateThreadOptions{
		ProjectID: base.project.ID, Provider: "claude", Model: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.chatThread = chatThread

	phaseThread, err := base.app.CreateThread(CreateThreadOptions{
		ProjectID: base.project.ID, Provider: "claude", Model: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: base.root.ID, PhaseID: fixture.phaseID, Attempt: 1,
		Status: "completed", StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := base.app.store.AttachWorkItemPhaseRun(
		base.root.ID, fixture.phaseID, 1, phaseThread.ID, "",
	); err != nil {
		t.Fatal(err)
	}
	if err := base.app.store.RecordWorkItemEffect(store.WorkItemEffect{
		ItemID: base.root.ID, PhaseID: fixture.phaseID, Tool: "start-run",
		PayloadHash: "hash", Payload: json.RawMessage(`{}`), CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	fixture.automation = store.Automation{
		ID: itemID + "-automation", ProjectID: base.project.ID, WorkflowID: "wf",
		WorkflowScope: "shared", Name: "Nightly", Enabled: true,
		Trigger:   json.RawMessage(`{"kind":"cron","cron":"0 3 * * *"}`),
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}
	if err := base.app.store.CreateAutomation(fixture.automation); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// branchNames is the whole branch set of the fixture repository, sorted. The
// cleanup rule is asserted against this rather than against a list of branches
// the test thought to name: "no branch was deleted" is a statement about the
// repository, and a per-branch check would miss one deleted by a path the test
// did not anticipate.
func (f *projectDeleteFixture) branchNames(t *testing.T) []string {
	t.Helper()
	branches, err := f.app.gitCore().ListBranches(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(branches))
	for _, branch := range branches {
		names = append(names, branch.Name)
	}
	slices.Sort(names)
	return names
}

func (f *projectDeleteFixture) assertRowsAndThreadsGone(t *testing.T, threadIDs []string) {
	t.Helper()
	for _, id := range []string{f.root.ID, f.child.ID} {
		if _, err := f.app.store.GetWorkItem(id); err == nil {
			t.Fatalf("run %s survived the deletion", id)
		}
	}
	phases, err := f.app.store.ListWorkItemPhases(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 0 {
		t.Fatalf("phase rows = %+v, want none", phases)
	}
	units, err := f.app.store.ListWorkItemUnits(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 0 {
		t.Fatalf("unit rows = %+v, want none", units)
	}
	if _, found, err := f.app.store.GetWorkItemEffect(
		f.root.ID, f.phaseID, "start-run", "hash",
	); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("effect row survived the deletion")
	}
	automations, err := f.app.store.ListAutomations(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(automations) != 0 {
		t.Fatalf("automations = %+v, want none", automations)
	}
	if len(threadIDs) != 2 {
		t.Fatalf("returned thread ids = %v, want the chat thread and the phase thread", threadIDs)
	}
	for _, threadID := range threadIDs {
		if _, err := f.app.store.GetThread(threadID); err == nil {
			t.Fatalf("thread %s survived the deletion", threadID)
		}
	}
	if _, err := f.app.store.GetProject(f.project.ID); err == nil {
		t.Fatal("project row survived the deletion")
	}
}

// The whole point of the rework: deletion is cleanup. The checkouts the app
// created go, every row goes, and the branches those checkouts held are still
// there afterwards — the commits the runs produced remain reachable in the
// user's repository, which the app does not get to rewrite on its way out.
func TestDeleteProjectCleansUpCheckoutsAndKeepsBranches(t *testing.T) {
	f := newProjectDeleteFixture(t, "cleanup-delete")
	before := f.branchNames(t)
	if !slices.Contains(before, f.root.Branch) || !slices.Contains(before, f.unit.Branch) {
		t.Fatalf("fixture branches = %v, want the run and unit branches present before deletion", before)
	}

	result, err := f.app.DeleteProject(f.project.ID)
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if len(result.RetainedWorktrees) != 0 {
		t.Fatalf("retained = %+v, want none — both checkouts were clean", result.RetainedWorktrees)
	}
	for _, path := range []string{f.root.WorktreePath, f.unit.WorktreePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("checkout %q survived the cleanup (stat err = %v)", path, err)
		}
	}
	if after := f.branchNames(t); !slices.Equal(before, after) {
		t.Fatalf("branch set changed: before %v, after %v — project deletion must delete no branch", before, after)
	}
	f.assertRowsAndThreadsGone(t, result.ThreadIDs)
}

// A checkout carrying uncommitted work is git's to refuse, and the app does not
// override it. The deletion still succeeds — the project is gone either way —
// and the checkout is reported so the outcome is never a silent partial one.
func TestDeleteProjectRetainsDirtyCheckoutsAndStillSucceeds(t *testing.T) {
	f := newProjectDeleteFixture(t, "dirty-delete")
	writeDispositionFile(t, f.root.WorktreePath, "dirty.txt", "unsaved\n")
	before := f.branchNames(t)

	result, err := f.app.DeleteProject(f.project.ID)
	if err != nil {
		t.Fatalf("DeleteProject with a dirty checkout: %v", err)
	}
	if len(result.RetainedWorktrees) != 1 {
		t.Fatalf("retained = %+v, want the one dirty checkout", result.RetainedWorktrees)
	}
	retained := result.RetainedWorktrees[0]
	if !gitops.SameFilesystemPath(retained.Path, f.root.WorktreePath) {
		t.Fatalf("retained %+v, want the run checkout %q", retained, f.root.WorktreePath)
	}
	if retained.Branch != f.root.Branch {
		t.Fatalf("retained branch = %q, want %q", retained.Branch, f.root.Branch)
	}
	if !strings.Contains(retained.Reason, "uncommitted") {
		t.Fatalf("retained reason = %q, want it to explain the uncommitted work", retained.Reason)
	}
	if _, err := os.Stat(f.root.WorktreePath); err != nil {
		t.Fatalf("the retained checkout was removed anyway: %v", err)
	}
	// The clean sibling still went: one refusal does not abandon the cleanup.
	if _, err := os.Stat(f.unit.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("clean unit checkout survived (stat err = %v)", err)
	}
	if after := f.branchNames(t); !slices.Equal(before, after) {
		t.Fatalf("branch set changed: before %v, after %v", before, after)
	}
	f.assertRowsAndThreadsGone(t, result.ThreadIDs)
}

// The permanent rule, pinned at the source rather than only at one fixture's
// outcome: nothing on the project-deletion path may call a branch deletion, or
// the forced worktree removal that would step over git's refusal. A behavioural
// test only covers the paths its fixture reaches; this covers every line in the
// files that own the flow, including the ones no test drives.
func TestProjectDeletionSourceCallsNoBranchDeletion(t *testing.T) {
	// D23's discard legitimately calls both, and lives in its own files. Renaming
	// or splitting a file below fails this test loudly rather than silently
	// dropping the coverage: the list is the flow, and it has to be maintained
	// with it.
	files := []string{
		"internal/app/app_projects.go",
		"internal/app/app_project_delete_workflow.go",
		"internal/app/app_project_delete_cleanup.go",
	}
	forbidden := map[string]string{
		"DeleteBranch":        "project deletion is cleanup; deleting a branch is D23's discard alone",
		"RemoveWorktreeForce": "git's refusal of a dirty checkout is the safety valve; do not force past it",
	}
	for _, name := range files {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, banned := forbidden[selector.Sel.Name]; banned {
				t.Errorf(
					"%s:%d calls %s — %s",
					name, fileSet.Position(selector.Pos()).Line, selector.Sel.Name, why,
				)
			}
			return true
		})
	}
}

// The transport classification, restated for the shape that exists now: the
// preview reads local checkouts, so it stays LocalOnly; the deletion destroys
// nothing git cannot still reach, so it stays reachable from a remote client
// like every other project-management call.
func TestProjectDeletionTransportClassification(t *testing.T) {
	if !transport.LocalOnlyMethods["ProjectDeletionPreview"] {
		t.Fatal("ProjectDeletionPreview reads local checkouts and their uncommitted paths; it must be LocalOnly")
	}
	if transport.LocalOnlyMethods["DeleteProject"] {
		t.Fatal("DeleteProject deletes no branch; classifying it LocalOnly removes remote project management for no gain")
	}
}

// An automation on its own is workflow work: the project owns a standing
// instruction and it goes with the project, checkouts or not.
func TestDeleteProjectRemovesAutomations(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.CreateProject(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateAutomation(store.Automation{
		ID: "lonely-automation", ProjectID: project.ID, WorkflowID: "wf",
		WorkflowScope: "shared", Name: "Nightly", Enabled: true,
		Trigger:   json.RawMessage(`{"kind":"cron","cron":"0 3 * * *"}`),
		CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := app.DeleteProject(project.ID); err != nil {
		t.Fatalf("DeleteProject with an automation: %v", err)
	}
	automations, err := app.store.ListAutomations(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(automations) != 0 {
		t.Fatalf("automations = %+v, want them deleted with the project", automations)
	}
	if _, err := app.store.GetProject(project.ID); err == nil {
		t.Fatal("project row survived the deletion")
	}
}

// The project's own checkout is never a casualty, even when a run recorded it
// as its workspace. Removing it, or deleting the branch it holds, is the one
// mistake here that could not be undone.
func TestDeleteProjectNeverRemovesTheProjectCheckout(t *testing.T) {
	f := newDiscardFixture(t, "project-workspace-delete")
	if err := f.app.store.UpdateWorkItemWorkspace(f.root.ID, f.project.Path, "main", "main"); err != nil {
		t.Fatal(err)
	}

	preview, err := f.app.ProjectDeletionPreview(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Worktrees) != 0 {
		t.Fatalf("preview offers the project checkout for cleanup: %+v", preview.Worktrees)
	}
	result, err := f.app.DeleteProject(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RetainedWorktrees) != 0 {
		t.Fatalf("retained = %+v, want none: the project checkout is not the cleanup's to report", result.RetainedWorktrees)
	}
	if _, err := os.Stat(filepath.Join(f.project.Path, ".git")); err != nil {
		t.Fatalf("deletion damaged the project checkout: %v", err)
	}
	branches, err := f.app.gitCore().ListBranches(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, branch := range branches {
		if branch.Name == "main" {
			found = true
		}
	}
	if !found {
		t.Fatal("deletion removed the project's own branch")
	}
}

func TestProjectDeletionPreviewDescribesTheCleanupAndMutatesNothing(t *testing.T) {
	f := newProjectDeleteFixture(t, "preview-delete")
	writeDispositionFile(t, f.root.WorktreePath, "dirty.txt", "unsaved\n")
	before := f.branchNames(t)

	preview, err := f.app.ProjectDeletionPreview(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.HasWork {
		t.Fatal("preview reports no work for a project that owns two runs")
	}
	if preview.RunCount != 2 {
		t.Fatalf("run count = %d, want the root and its callee", preview.RunCount)
	}
	if len(preview.LiveRunIDs) != 0 {
		t.Fatalf("live runs = %v, want none", preview.LiveRunIDs)
	}
	if preview.AutomationCount != 1 {
		t.Fatalf("automation count = %d, want 1", preview.AutomationCount)
	}
	// Root and callee share one checkout, so it is described once, next to the
	// fan-out unit's own.
	if len(preview.Worktrees) != 2 {
		t.Fatalf("worktree rows = %+v, want the shared run checkout and the unit", preview.Worktrees)
	}
	run := findCleanupWorktree(t, preview, f.root.WorktreePath)
	if run.Branch != f.root.Branch {
		t.Fatalf("run row = %+v, want it on %s", run, f.root.Branch)
	}
	if run.DirtyFileCount != 1 || !run.Retained || !strings.Contains(run.Reason, "uncommitted") {
		t.Fatalf("run row = %+v, want one uncommitted file and a retention that says so", run)
	}
	unit := findCleanupWorktree(t, preview, f.unit.WorktreePath)
	if unit.Retained || unit.DirtyFileCount != 0 || unit.Reason != "" {
		t.Fatalf("unit row = %+v, want a clean checkout the cleanup will remove", unit)
	}

	// Nothing moved: not a row, not a checkout, not a branch.
	for _, id := range []string{f.root.ID, f.child.ID} {
		if _, err := f.app.store.GetWorkItem(id); err != nil {
			t.Fatalf("run %s was removed by the preview: %v", id, err)
		}
	}
	for _, path := range []string{f.root.WorktreePath, f.unit.WorktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("checkout %q was removed by the preview: %v", path, err)
		}
	}
	if after := f.branchNames(t); !slices.Equal(before, after) {
		t.Fatalf("preview changed the branch set: before %v, after %v", before, after)
	}
	if _, err := f.app.store.GetProject(f.project.ID); err != nil {
		t.Fatalf("project was removed by the preview: %v", err)
	}
}

func TestProjectDeletionPreviewReportsNoWorkForAPlainProject(t *testing.T) {
	app := newTestAppWithStore(t)
	project, err := app.CreateProject(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := app.ProjectDeletionPreview(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.HasWork || preview.RunCount != 0 || preview.AutomationCount != 0 {
		t.Fatalf("preview = %+v, want an empty report", preview)
	}
	if preview.Worktrees == nil || preview.LiveRunIDs == nil {
		t.Fatalf("preview = %+v, want allocated slices so the wire carries [] not null", preview)
	}
	result, err := app.DeleteProject(project.ID)
	if err != nil {
		t.Fatalf("DeleteProject on a plain project: %v", err)
	}
	if result.ThreadIDs == nil || result.RetainedWorktrees == nil {
		t.Fatalf("result = %+v, want allocated slices so the wire carries [] not null", result)
	}
}

// A project whose checkout has been deleted from disk is still previewable and
// still deletable. There is no repository left to ask, so the checkouts the runs
// left behind cannot be removed through git — they are reported rather than
// silently abandoned, and refusing the deletion would strand the project and its
// runs forever.
func TestDeleteProjectWithAMissingCheckoutStillCleansUp(t *testing.T) {
	f := newProjectDeleteFixture(t, "missing-repo-delete")
	if err := os.RemoveAll(f.project.Path); err != nil {
		t.Fatal(err)
	}

	preview, err := f.app.ProjectDeletionPreview(f.project.ID)
	if err != nil {
		t.Fatalf("preview with the project checkout gone: %v", err)
	}
	if !preview.HasWork || len(preview.Worktrees) != 2 {
		t.Fatalf("preview = %+v, want both run checkouts still described", preview)
	}
	for _, row := range preview.Worktrees {
		if !row.Retained || !strings.Contains(row.Reason, "repository") {
			t.Fatalf("row %+v, want it retained because the repository is gone", row)
		}
	}

	result, err := f.app.DeleteProject(f.project.ID)
	if err != nil {
		t.Fatalf("DeleteProject with the project checkout gone: %v", err)
	}
	if len(result.RetainedWorktrees) != 2 {
		t.Fatalf("retained = %+v, want both checkouts reported as left behind", result.RetainedWorktrees)
	}
	f.assertRowsAndThreadsGone(t, result.ThreadIDs)
}

func findCleanupWorktree(
	t *testing.T, preview ProjectDeletionPreview, path string,
) ProjectCleanupWorktree {
	t.Helper()
	for _, worktree := range preview.Worktrees {
		if gitops.SameFilesystemPath(worktree.Path, path) {
			return worktree
		}
	}
	t.Fatalf("preview has no row for %q; rows = %+v", path, preview.Worktrees)
	return ProjectCleanupWorktree{}
}
