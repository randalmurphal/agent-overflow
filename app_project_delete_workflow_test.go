package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
)

// projectDeleteFixture is a project that owns real workflow work: a run tree
// with checkouts and branches of its own, a phase thread, an effect row, an
// automation, and an ordinary chat thread alongside them. Deleting it is the
// only way to test a flow whose whole job is destroying that set.
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

func (f *projectDeleteFixture) assertWorkflowWorkIntact(t *testing.T) {
	t.Helper()
	for _, id := range []string{f.root.ID, f.child.ID} {
		if _, err := f.app.store.GetWorkItem(id); err != nil {
			t.Fatalf("run %s was removed: %v", id, err)
		}
	}
	for _, path := range []string{f.root.WorktreePath, f.unit.WorktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("checkout %q was removed: %v", path, err)
		}
	}
	for _, branch := range []string{f.root.Branch, f.unit.Branch} {
		if !f.branchExists(t, branch) {
			t.Fatalf("branch %q was deleted", branch)
		}
	}
	automations, err := f.app.store.ListAutomations(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(automations) != 1 {
		t.Fatalf("automations = %+v, want the seeded one intact", automations)
	}
	if _, err := f.app.store.GetProject(f.project.ID); err != nil {
		t.Fatalf("project was removed: %v", err)
	}
	if _, err := f.app.store.GetThread(f.chatThread.ID); err != nil {
		t.Fatalf("thread was removed: %v", err)
	}
}

// Without consent the deletion is refused outright and nothing at all moves —
// not a row, not a checkout, not a branch.
func TestDeleteProjectRefusesWorkflowWorkWithoutConsent(t *testing.T) {
	f := newProjectDeleteFixture(t, "refuse-delete")

	_, err := f.app.DeleteProject(f.project.ID)
	if !errors.Is(err, ErrProjectOwnsWorkflowWork) {
		t.Fatalf("DeleteProject without consent = %v, want ErrProjectOwnsWorkflowWork", err)
	}
	for _, want := range []string{
		"2 workflow runs", "1 automation",
		"ProjectDeletionPreview", "DeleteProjectDiscardingWorkflowWork",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err, want)
		}
	}
	f.assertWorkflowWorkIntact(t)
}

// The consent is a separate method precisely so the transport can refuse it
// from a LAN peer, which it cannot do for an argument. If someone folds the two
// back into one method with a flag, this is what fails.
func TestProjectDeletionDestructiveFormIsLocalOnly(t *testing.T) {
	if !transport.LocalOnlyMethods["DeleteProjectDiscardingWorkflowWork"] {
		t.Fatal("DeleteProjectDiscardingWorkflowWork removes worktrees and deletes branches; it must be LocalOnly")
	}
	// Plain deletion destroys no git state and stays reachable from a remote
	// client, as it was before D25.
	if transport.LocalOnlyMethods["DeleteProject"] {
		t.Fatal("DeleteProject destroys no git state; classifying it LocalOnly removes remote project management for no gain")
	}
}

// An automation on its own is workflow work too: deleting the project destroys
// a standing instruction the human wrote, so it needs the same consent.
func TestDeleteProjectRefusesAutomationsWithoutConsent(t *testing.T) {
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

	if _, err := app.DeleteProject(project.ID); !errors.Is(err, ErrProjectOwnsWorkflowWork) {
		t.Fatalf("DeleteProject with an automation = %v, want ErrProjectOwnsWorkflowWork", err)
	}
	if _, err := app.store.GetProject(project.ID); err != nil {
		t.Fatalf("refused deletion removed the project: %v", err)
	}

	if _, err := app.DeleteProjectDiscardingWorkflowWork(project.ID); err != nil {
		t.Fatalf("DeleteProject with consent: %v", err)
	}
	automations, err := app.store.ListAutomations(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(automations) != 0 {
		t.Fatalf("automations = %+v, want them deleted with the project", automations)
	}
}

// With consent the deletion is total: checkouts removed, branches deleted,
// every workflow row dropped, threads torn down, project gone.
func TestDeleteProjectCascadesWorkflowWorkWithConsent(t *testing.T) {
	f := newProjectDeleteFixture(t, "cascade-delete")
	writeDispositionFile(t, f.root.WorktreePath, "dirty.txt", "unsaved\n")

	threadIDs, err := f.app.DeleteProjectDiscardingWorkflowWork(f.project.ID)
	if err != nil {
		t.Fatalf("DeleteProject with consent: %v", err)
	}
	if len(threadIDs) != 2 {
		t.Fatalf("returned thread ids = %v, want the chat thread and the phase thread", threadIDs)
	}

	for _, path := range []string{f.root.WorktreePath, f.unit.WorktreePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("checkout %q survived the deletion (stat err = %v)", path, err)
		}
	}
	for _, branch := range []string{f.root.Branch, f.unit.Branch} {
		if f.branchExists(t, branch) {
			t.Fatalf("branch %q survived the deletion", branch)
		}
	}
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
	for _, threadID := range threadIDs {
		if _, err := f.app.store.GetThread(threadID); err == nil {
			t.Fatalf("thread %s survived the deletion", threadID)
		}
	}
	if _, err := f.app.store.GetProject(f.project.ID); err == nil {
		t.Fatal("project row survived the deletion")
	}
}

// The project's own checkout is never a casualty, even when a run recorded it
// as its workspace. Removing it, or deleting the branch it holds, is the one
// mistake here that cannot be undone.
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
		t.Fatalf("preview offers the project checkout as a loss: %+v", preview.Worktrees)
	}
	if _, err := f.app.DeleteProjectDiscardingWorkflowWork(f.project.ID); err != nil {
		t.Fatal(err)
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

func TestProjectDeletionPreviewReportsLossAndMutatesNothing(t *testing.T) {
	f := newProjectDeleteFixture(t, "preview-delete")
	writeDispositionFile(t, f.root.WorktreePath, "dirty.txt", "unsaved\n")

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
	if len(preview.RootRunIDs) != 1 || preview.RootRunIDs[0] != f.root.ID {
		t.Fatalf("root runs = %v, want just %s", preview.RootRunIDs, f.root.ID)
	}
	if len(preview.LiveRunIDs) != 0 {
		t.Fatalf("live runs = %v, want none", preview.LiveRunIDs)
	}
	if preview.AutomationCount != 1 {
		t.Fatalf("automation count = %d, want 1", preview.AutomationCount)
	}
	// Root and callee share one checkout, so the loss is reported once, next to
	// the fan-out unit's own.
	if len(preview.Worktrees) != 2 {
		t.Fatalf("worktree rows = %+v, want the shared run checkout and the unit", preview.Worktrees)
	}
	row := findDiscardWorktree(t, WorkflowDiscardPreview{Worktrees: preview.Worktrees}, f.root.WorktreePath)
	if row.Branch != f.root.Branch || !row.Present || !row.Registered {
		t.Fatalf("run row = %+v, want a present registered checkout on %s", row, f.root.Branch)
	}
	if row.DirtyFileCount != 1 || row.UnmergedCommitCount != 1 {
		t.Fatalf("run row = %+v, want one dirty file and one unmerged commit", row)
	}

	f.assertWorkflowWorkIntact(t)
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
	if preview.Worktrees == nil || preview.RootRunIDs == nil || preview.LiveRunIDs == nil {
		t.Fatalf("preview = %+v, want allocated slices so the wire carries [] not null", preview)
	}
	// A project with no workflow work deletes the same way it always has,
	// whichever way the consent flag is set.
	if _, err := app.DeleteProject(project.ID); err != nil {
		t.Fatalf("DeleteProject on a plain project: %v", err)
	}
}

// A project whose checkout has been deleted from disk is still previewable and
// still deletable. The repository that registered those checkouts and held
// their branches went with it, so there is nothing left for git to destroy —
// and refusing here would strand the project, and its runs, forever.
func TestDeleteProjectWithAMissingCheckoutStillCascades(t *testing.T) {
	f := newProjectDeleteFixture(t, "missing-repo-delete")
	if err := os.RemoveAll(f.project.Path); err != nil {
		t.Fatal(err)
	}

	preview, err := f.app.ProjectDeletionPreview(f.project.ID)
	if err != nil {
		t.Fatalf("preview with the project checkout gone: %v", err)
	}
	if !preview.HasWork || len(preview.Worktrees) != 2 {
		t.Fatalf("preview = %+v, want both run checkouts still reported", preview)
	}
	for _, row := range preview.Worktrees {
		if row.Registered {
			t.Fatalf("row %+v claims a registration no repository is left to hold", row)
		}
		// The branch graph went with the repository, so the comparison is not
		// attempted rather than reported as a per-row failure.
		if row.UnmergedCommitCount != 0 || strings.Contains(row.Error, "unmerged") {
			t.Fatalf("row %+v, want no branch comparison against a repository that is gone", row)
		}
	}

	if _, err := f.app.DeleteProjectDiscardingWorkflowWork(f.project.ID); err != nil {
		t.Fatalf("DeleteProject with the project checkout gone: %v", err)
	}
	if _, err := f.app.store.GetProject(f.project.ID); err == nil {
		t.Fatal("project row survived the deletion")
	}
	for _, id := range []string{f.root.ID, f.child.ID} {
		if _, err := f.app.store.GetWorkItem(id); err == nil {
			t.Fatalf("run %s survived the deletion", id)
		}
	}
	automations, err := f.app.store.ListAutomations(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(automations) != 0 {
		t.Fatalf("automations = %+v, want none", automations)
	}
}
