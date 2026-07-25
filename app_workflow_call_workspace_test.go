package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

// A called run provisions nothing of its own (§9): it executes where its tree's
// root does. These tests cover the resolution path directly, because the fact
// they protect — one worktree per tree — is invisible in a run that happens to
// have been stamped at call time and only shows up in the cases where it was
// not: a call that is the root's *first* phase (nothing to copy yet) and a
// deeper descendant of it.

func callWorkspaceRunner(t *testing.T, app *App) *workflowAppRunner {
	t.Helper()
	return newWorkflowAppRunner(app, t.TempDir(), staticWorkflowProfileSource{
		value: &profile.Profile{BaseBranch: "main"},
	})
}

// callWorkspaceItem persists one run row. Callers hand it the workspace need to
// freeze; the frozen definition deliberately contains only a call phase, so a
// re-derivation from the definition alone would answer "project root" and the
// test would catch it.
func callWorkspaceItem(t *testing.T, app *App, id, projectID, parentID string, need def.WorkspaceNeed) store.WorkItem {
	t.Helper()
	snapshot, err := json.Marshal(engine.Snapshot{
		Workflow: def.Workflow{
			ID: "caller", Phases: []def.Phase{{ID: "audit", Shape: def.ShapeCall, Call: "child"}},
		},
		WorkspaceNeed: need,
	})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: id, ProjectID: projectID, Goal: "goal", WorkflowID: "caller",
		WorkflowScope: "shared", Snapshot: snapshot, State: string(engine.StateRunning),
		Source: "manual", CreatedAt: 1,
	}
	if parentID != "" {
		item.ParentItemID, item.ParentPhaseID, item.ParentAttempt = parentID, "audit", 1
		item.Source, item.SourceRef = engine.WorkItemSourceCall, parentID+"/audit/1"
		item.CallDepth = 1
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func callWorkspaceRequest(item store.WorkItem, need def.WorkspaceNeed) engine.RunRequest {
	return engine.RunRequest{
		Key:           engine.RunKey{ItemID: item.ID, PhaseID: "work", Attempt: 1},
		Item:          item,
		Workflow:      def.Workflow{ID: item.WorkflowID},
		WorkspaceNeed: need,
		Phase:         def.Phase{ID: "work", Driver: def.DriverAgent},
	}
}

func TestCalledRunProvisionsItsRootWorktreeRatherThanItsOwn(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	runner := callWorkspaceRunner(t, app)

	// The root's first phase is the call, so nothing has been provisioned when
	// the child starts: the child is the one that has to cut the tree's worktree,
	// and it must cut the ROOT's.
	root := callWorkspaceItem(t, app, "call-root", projectRow.ID, "", def.WorkspaceWorktree)
	child := callWorkspaceItem(t, app, "call-child", projectRow.ID, root.ID, def.WorkspaceProjectRoot)

	// The child's own request asks for the project root; the root's frozen need
	// is what decides, because a child never gets a say in its workspace.
	prepared, err := runner.prepareWorkspace(context.Background(), callWorkspaceRequest(child, def.WorkspaceProjectRoot))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, prepared.path, true) })
	if prepared.path == repo || prepared.path == "" || prepared.branch == "" {
		t.Fatalf("called run resolved to %+v, want a worktree outside %q", prepared, repo)
	}
	if info, statErr := os.Stat(prepared.path); statErr != nil || !info.IsDir() {
		t.Fatalf("stat resolved worktree %q = %v", prepared.path, statErr)
	}

	// Both rows record it: the root because it owns the worktree, the child so
	// its run record shows where it ran and its later phases take the fast path.
	storedRoot := mustWorkItem(t, app, root.ID)
	storedChild := mustWorkItem(t, app, child.ID)
	if storedRoot.WorktreePath != prepared.path || storedRoot.Branch != prepared.branch {
		t.Fatalf("root workspace = %q/%q, want %q/%q",
			storedRoot.WorktreePath, storedRoot.Branch, prepared.path, prepared.branch)
	}
	if storedChild.WorktreePath != prepared.path || storedChild.Branch != prepared.branch {
		t.Fatalf("child workspace = %q/%q, want the root's %q/%q",
			storedChild.WorktreePath, storedChild.Branch, prepared.path, prepared.branch)
	}
	if storedChild.BaseBranch != storedRoot.BaseBranch || storedRoot.BaseBranch == "" {
		t.Fatalf("child base branch = %q, want the root's %q", storedChild.BaseBranch, storedRoot.BaseBranch)
	}

	// The branch name is the ROOT's: a worktree named after the child would be a
	// second one waiting to happen on the next crash-recovery adoption scan.
	if prefix := workflowWorktreeBranchPrefix(app.worktreeBranchPrefix(), root.WorkflowID, root.ID); !strings.HasPrefix(prepared.branch, prefix) {
		t.Fatalf("worktree branch %q is not the root's (%q)", prepared.branch, prefix)
	}

	// A grandchild walks the whole chain, and the root — arriving last, as it
	// does when its call phase returns — adopts what its child cut. One worktree
	// for the tree, whichever member of it runs first.
	grandchild := callWorkspaceItem(t, app, "call-grandchild", projectRow.ID, child.ID, def.WorkspaceProjectRoot)
	grandPrepared, err := runner.prepareWorkspace(context.Background(), callWorkspaceRequest(grandchild, def.WorkspaceProjectRoot))
	if err != nil {
		t.Fatal(err)
	}
	if grandPrepared.path != prepared.path {
		t.Fatalf("grandchild workspace = %q, want the tree's %q", grandPrepared.path, prepared.path)
	}
	rootPrepared, err := runner.prepareWorkspace(context.Background(), callWorkspaceRequest(storedRoot, def.WorkspaceWorktree))
	if err != nil {
		t.Fatal(err)
	}
	if rootPrepared.path != prepared.path {
		t.Fatalf("root workspace = %q, want the one its child cut %q", rootPrepared.path, prepared.path)
	}
}

func TestCalledRunOfAReadOnlyTreeRunsOnTheProjectRoot(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	runner := callWorkspaceRunner(t, app)

	root := callWorkspaceItem(t, app, "ro-root", projectRow.ID, "", def.WorkspaceProjectRoot)
	child := callWorkspaceItem(t, app, "ro-child", projectRow.ID, root.ID, def.WorkspaceWorktree)

	// Even asking for a worktree, the child gets the root's project-root
	// workspace: a child's write-need never cuts one (isolation comes from
	// fan-out), and the need it would cut against was propagated into the root
	// at start.
	prepared, err := runner.prepareWorkspace(context.Background(), callWorkspaceRequest(child, def.WorkspaceWorktree))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.path != repo || prepared.branch != "" {
		t.Fatalf("called run of a read-only tree resolved to %+v, want the project root %q", prepared, repo)
	}
	if stored := mustWorkItem(t, app, child.ID); stored.WorktreePath != "" || stored.Branch != "" {
		t.Fatalf("child stamped workspace %q/%q, want none", stored.WorktreePath, stored.Branch)
	}
}

func TestRunWithoutAFrozenWorkspaceNeedIsRefused(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	runner := callWorkspaceRunner(t, app)

	item := callWorkspaceItem(t, app, "unfrozen", projectRow.ID, "", def.WorkspaceWorktree)
	_, err := runner.prepareWorkspace(context.Background(), callWorkspaceRequest(item, ""))
	if err == nil {
		t.Fatal("a run request with no frozen workspace need was accepted")
	}
}

func mustWorkItem(t *testing.T, app *App, id string) store.WorkItem {
	t.Helper()
	item, err := app.store.GetWorkItem(id)
	if err != nil {
		t.Fatal(err)
	}
	return item
}
