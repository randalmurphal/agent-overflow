package app

import (
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWorkflowMergeItemPersistsReceiptAndEmitsRefresh(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "merge-item")

	receipt, err := app.WorkflowMergeItem(item.ID)
	if err != nil {
		t.Fatalf("WorkflowMergeItem: %v", err)
	}
	if receipt.Action != "merged" || receipt.Mode != "ff" || receipt.Policy != "manual" || receipt.SHA == "" || receipt.Base != "main" {
		t.Fatalf("receipt = %+v", receipt)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted WorkflowDispositionReceipt
	if err := json.Unmarshal(stored.Disposition, &persisted); err != nil || persisted != receipt {
		t.Fatalf("persisted receipt = %+v err=%v, want %+v", persisted, err, receipt)
	}
	if got := app.gitCore().CurrentBranch(repo); got != "main" {
		t.Fatalf("project branch = %q, want main", got)
	}
	foundRefresh := false
	for _, event := range bus.allEvents() {
		state, ok := event.Data.(engine.StateEvent)
		if event.Name == "workflow:item-state" && ok && state.ItemID == item.ID && state.From == engine.StateDone && state.To == engine.StateDone {
			foundRefresh = true
		}
	}
	if !foundRefresh {
		t.Fatal("receipt persistence did not emit item-state refresh")
	}
}

func TestWorkflowMergeRefusalParksDispositionWithoutReceipt(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	configRoot := t.TempDir()
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "conflict-item")
	writeDispositionFile(t, repo, "README.txt", "base conflict\n")
	testutil.RunGit(t, repo, "add", "README.txt")
	testutil.RunGit(t, repo, "commit", "-m", "base conflict")

	if _, err := app.WorkflowMergeItem(item.ID); err == nil {
		t.Fatal("conflicted merge unexpectedly succeeded")
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateNeedsHuman) || stored.Reason != string(engine.ReasonDisposition) {
		t.Fatalf("refused item = %+v", stored)
	}
	if len(stored.Disposition) != 0 {
		t.Fatalf("refusal persisted receipt %s", stored.Disposition)
	}
	if len(stored.Digest) == 0 {
		t.Fatal("disposition park did not persist template digest")
	}
	foundPark := false
	for _, event := range bus.allEvents() {
		state, ok := event.Data.(engine.StateEvent)
		if event.Name == "workflow:item-state" && ok && state.ItemID == item.ID &&
			state.From == engine.StateDone && state.To == engine.StateNeedsHuman && state.Reason == engine.ReasonDisposition {
			foundPark = true
		}
	}
	if !foundPark {
		t.Fatal("disposition refusal did not emit exact done -> needs-human(disposition) event")
	}
}

func TestWorkflowMergeResolvesDispositionPark(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "parked-merge-item")
	parkedAt := time.Now().UnixMilli()
	if err := app.store.UpdateWorkItemState(item.ID, string(engine.StateNeedsHuman), string(engine.ReasonDisposition), parkedAt); err != nil {
		t.Fatal(err)
	}

	receipt, err := app.WorkflowMergeItem(item.ID)
	if err != nil {
		t.Fatalf("WorkflowMergeItem parked disposition: %v", err)
	}
	if receipt.Action != "merged" || receipt.SHA == "" {
		t.Fatalf("parked merge receipt = %+v", receipt)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateDone) || stored.Reason != "" || stored.EndedAt != parkedAt || len(stored.Disposition) == 0 {
		t.Fatalf("resolved parked merge = %+v", stored)
	}
}

func TestWorkflowMergeRefusesDirtyItemWorktree(t *testing.T) {
	app, _ := setupE2EApp(t)
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "dirty-item")
	writeDispositionFile(t, item.WorktreePath, "uncommitted.txt", "must not be lost\n")
	if _, err := app.WorkflowMergeItem(item.ID); err == nil {
		t.Fatal("dirty item worktree unexpectedly merged")
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateNeedsHuman) || stored.Reason != string(engine.ReasonDisposition) || len(stored.Disposition) != 0 {
		t.Fatalf("dirty refusal item = %+v", stored)
	}
	if content, err := os.ReadFile(filepath.Join(item.WorktreePath, "uncommitted.txt")); err != nil || string(content) != "must not be lost\n" {
		t.Fatalf("dirty refusal changed item worktree: content=%q err=%v", content, err)
	}
}

func TestWorkflowAutoMergeHonorsCleanupAutoAfterReceipt(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowProfileForDisposition(t, configRoot, projectRow.Slug, "auto-merge")
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	item := createDoneWorkflowWorktree(t, app, projectRow, "auto-item")
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{ID: "wf", Cleanup: def.CleanupAuto}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpdateWorkItemRunStart(item.ID, snapshot, item.WorktreePath, item.Branch, item.BaseBranch, item.StartedAt); err != nil {
		t.Fatal(err)
	}

	workflowEmitter{app: app, emit: func(eventchan.Channel, any) {}}.Emit(
		"workflow:item-state", engine.StateEvent{ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone},
	)
	app.workflowApplication().WaitAutoDisposition()
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt WorkflowDispositionReceipt
	if err := json.Unmarshal(stored.Disposition, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Policy != string(profile.DispositionAutoMerge) || receipt.Action != "merged" {
		t.Fatalf("auto receipt = %+v", receipt)
	}
	if stored.WorktreePath != "" || stored.Branch != "" || stored.BaseBranch != "" {
		t.Fatalf("cleanup-auto retained workspace fields: %+v", stored)
	}
	if _, err := os.Stat(item.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("cleanup-auto worktree stat error = %v, want not-exist", err)
	}
}

func TestWorkflowDispositionCleanupFailureReturnsMarkedReceiptAndEmits(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowProfileForDisposition(t, configRoot, projectRow.Slug, "manual")
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := testutil.RunGitAllowError(t.TempDir(), "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "remote", "add", "origin", remote)
	testutil.RunGit(t, repo, "push", "-u", "origin", "main")
	item := createDoneWorkflowWorktree(t, app, projectRow, "cleanup-failure")
	testutil.RunGit(t, item.WorktreePath, "branch", "--set-upstream-to", "origin/main", item.Branch)
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{ID: "wf", Cleanup: def.CleanupAuto}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpdateWorkItemRunStart(item.ID, snapshot, item.WorktreePath, item.Branch, item.BaseBranch, item.StartedAt); err != nil {
		t.Fatal(err)
	}

	receipt, err := app.WorkflowMergeItem(item.ID)
	if err != nil {
		t.Fatalf("landed disposition returned cleanup error: %v", err)
	}
	if !receipt.CleanupFailed || receipt.Action != "merged" || receipt.SHA == "" {
		t.Fatalf("cleanup-failed receipt = %+v", receipt)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted WorkflowDispositionReceipt
	if err := json.Unmarshal(stored.Disposition, &persisted); err != nil || persisted != receipt {
		t.Fatalf("persisted cleanup-failed receipt = %+v err=%v, want %+v", persisted, err, receipt)
	}
	if stored.WorktreePath == "" {
		t.Fatal("failed cleanup cleared persisted worktree")
	}
	foundError := false
	for _, event := range bus.allEvents() {
		workflowErr, ok := event.Data.(engine.ErrorEvent)
		if event.Name == "workflow:error" && ok && workflowErr.ItemID == item.ID {
			foundError = true
		}
	}
	if !foundError {
		t.Fatal("cleanup failure did not emit workflow:error")
	}
}

func TestWorkflowDiscardUsesGuardedRemovalAndKeepsRecord(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "discard-item")
	receipt, err := app.WorkflowDiscardItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != "discarded" || receipt.Policy != "manual" {
		t.Fatalf("discard receipt = %+v", receipt)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != item.ID || stored.WorktreePath != "" {
		t.Fatalf("discarded record = %+v", stored)
	}
}

func TestWorkflowDiscardWithoutWorktreeRecordsReceiptAndResolvesPark(t *testing.T) {
	app, _ := setupE2EApp(t)
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	item := store.WorkItem{
		ID: "record-only-discard", ProjectID: defaultTestProjectID, Goal: "Read only",
		WorkflowID: "wf", WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonDisposition), Source: "manual", CreatedAt: 1, EndedAt: 2,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	receipt, err := app.WorkflowDiscardItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != "discarded" || receipt.Policy != "manual" {
		t.Fatalf("record-only discard receipt = %+v", receipt)
	}
	stored, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(engine.StateDone) || stored.Reason != "" || stored.EndedAt != item.EndedAt || len(stored.Disposition) == 0 {
		t.Fatalf("record-only discarded item = %+v", stored)
	}
}

func TestWorkflowDiscardRemovesDirtyWorktree(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "dirty-discard")
	writeDispositionFile(t, item.WorktreePath, "uncommitted.txt", "authorized loss\n")
	if _, err := app.WorkflowDiscardItem(item.ID); err != nil {
		t.Fatalf("dirty discard: %v", err)
	}
	if _, err := os.Stat(item.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("dirty worktree still exists: %v", err)
	}
}

func TestWorkflowDiscardAcceptsEverySpecifiedTerminalAndParkedState(t *testing.T) {
	for _, state := range []engine.State{engine.StateFailed, engine.StateCancelled, engine.StateNeedsHuman} {
		t.Run(string(state), func(t *testing.T) {
			app, _ := setupE2EApp(t)
			// A parked member is settled by the discard, so the engine has to be
			// there for it: the discard is what stops it.
			if err := app.initWorkflowEngine(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
			repo := testutil.InitGitRepo(t)
			projectRow := testutil.EnsureProject(t, app.store, repo)
			item := createDoneWorkflowWorktree(t, app, projectRow, "discard-"+string(state))
			reason := ""
			if state == engine.StateNeedsHuman {
				reason = string(engine.ReasonAgentError)
			}
			if err := app.store.UpdateWorkItemState(item.ID, string(state), reason, time.Now().UnixMilli()); err != nil {
				t.Fatal(err)
			}
			receipt, err := app.WorkflowDiscardItem(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Action != "discarded" || receipt.Policy != "manual" {
				t.Fatalf("receipt = %+v", receipt)
			}
			stored, err := app.store.GetWorkItem(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			// A discarded run must not still be resting on a checkout that no
			// longer exists: the park is settled, the terminal states are left as
			// they are.
			want := state
			if state == engine.StateNeedsHuman {
				want = engine.StateCancelled
			}
			if stored.State != string(want) {
				t.Fatalf("discarded %s item = state %q, want %q", state, stored.State, want)
			}
		})
	}
}

// The tree's PARKED members are settled by the discard, not only its running
// ones. A member left needing a human after its worktree was removed and its
// branch deleted has no move left that can succeed — every repair verb it
// carries reads a checkout that is gone.
func TestWorkflowDiscardSettlesParkedTreeMembers(t *testing.T) {
	app, _ := setupE2EApp(t)
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	root := createDoneWorkflowWorktree(t, app, projectRow, "discard-tree-root")
	child := store.WorkItem{
		ID: "discard-tree-child", ProjectID: projectRow.ID, Goal: "wave",
		WorkflowID: "wf", WorkflowScope: "shared", State: string(engine.StateNeedsHuman),
		Reason: string(engine.ReasonUnitFailed), Source: engine.WorkItemSourceCall,
		ParentItemID: root.ID, ParentPhaseID: "wave", ParentAttempt: 1, CallDepth: 1,
		CreatedAt: time.Now().UnixMilli(), StartedAt: time.Now().UnixMilli(),
	}
	if err := app.store.CreateWorkItem(child); err != nil {
		t.Fatal(err)
	}

	receipt, err := app.WorkflowDiscardItem(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != "discarded" {
		t.Fatalf("receipt = %+v", receipt)
	}
	stored, err := app.store.GetWorkItem(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if engine.State(stored.State) != engine.StateCancelled {
		t.Fatalf("parked tree member after discard = state %q reason %q, want cancelled",
			stored.State, stored.Reason)
	}
}

func TestWorkflowCreateItemPRPushesAndPersistsReference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	item := createDoneWorkflowWorktree(t, app, projectRow, "pr-item")
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := testutil.RunGitAllowError(t.TempDir(), "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	remoteURL := "https://github.com/example/agent-overflow.git"
	testutil.RunGit(t, repo, "remote", "add", "origin", remoteURL)
	if forge := app.gitCore().DetectForge(item.WorktreePath); forge != "github" {
		t.Fatalf("detected forge = %q", forge)
	}
	testutil.RunGit(t, repo, "config", "url."+remote+".insteadOf", remoteURL)
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf '%s\\n' 'https://github.com/example/agent-overflow/pull/42'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	receipt, err := app.WorkflowCreateItemPR(item.ID)
	if err != nil {
		t.Fatalf("WorkflowCreateItemPR: %v", err)
	}
	if receipt.Action != "pr" || receipt.PRRef != "https://github.com/example/agent-overflow/pull/42" || receipt.SHA == "" || receipt.Base != "main" {
		t.Fatalf("PR receipt = %+v", receipt)
	}
	stdout, _, err := app.gitCore().Execute(item.WorktreePath, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil || strings.TrimSpace(stdout) != "origin/"+item.Branch {
		t.Fatalf("upstream = %q err=%v", strings.TrimSpace(stdout), err)
	}

	configRoot := t.TempDir()
	projectRow, err = app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkflowProfileForDisposition(t, configRoot, projectRow.Slug, "auto-pr")
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowApplication().Engine().Close() })
	autoItem := createDoneWorkflowWorktree(t, app, projectRow, "auto-pr-item")
	testutil.RunGit(t, repo, "config", "--unset-all", "url."+remote+".insteadOf")
	if forge := app.gitCore().DetectForge(autoItem.WorktreePath); forge != "github" {
		t.Fatalf("auto detected forge = %q", forge)
	}
	testutil.RunGit(t, repo, "config", "url."+remote+".insteadOf", remoteURL)
	workflowEmitter{app: app, emit: func(eventchan.Channel, any) {}}.Emit(
		"workflow:item-state", engine.StateEvent{ItemID: autoItem.ID, ProjectID: autoItem.ProjectID, From: engine.StateRunning, To: engine.StateDone},
	)
	app.workflowApplication().WaitAutoDisposition()
	autoStored, err := app.store.GetWorkItem(autoItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	var autoReceipt WorkflowDispositionReceipt
	if err := json.Unmarshal(autoStored.Disposition, &autoReceipt); err != nil {
		t.Fatal(err)
	}
	if autoReceipt.Action != "pr" || autoReceipt.Policy != string(profile.DispositionAutoPR) || autoReceipt.PRRef == "" {
		t.Fatalf("auto PR receipt = %+v", autoReceipt)
	}
}

func createDoneWorkflowWorktree(t *testing.T, app *App, projectRow store.Project, itemID string) store.WorkItem {
	t.Helper()
	branch := "workflow-" + itemID
	worktreePath := filepath.Join(t.TempDir(), itemID)
	core := gitops.NewCore()
	app.git = core
	if err := core.CreateWorktreeFromBranch(projectRow.Path, worktreePath, "main", branch); err != nil {
		t.Fatal(err)
	}
	writeDispositionFile(t, worktreePath, "README.txt", "item "+itemID+"\n")
	testutil.RunGit(t, worktreePath, "add", "README.txt")
	testutil.RunGit(t, worktreePath, "commit", "-m", itemID)
	item := store.WorkItem{
		ID: itemID, ProjectID: projectRow.ID, Goal: "Land " + itemID,
		WorkflowID: "wf", WorkflowScope: "shared", State: string(engine.StateDone),
		WorktreePath: worktreePath, Branch: branch, BaseBranch: "main",
		Source: "manual", CreatedAt: time.Now().UnixMilli(), StartedAt: time.Now().UnixMilli(), EndedAt: time.Now().UnixMilli(),
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func writeDispositionFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeWorkflowProfileForDisposition(t *testing.T, configRoot, slug, disposition string) {
	t.Helper()
	dir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "base_branch: main\ndisposition: " + disposition + "\n"
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// discardFixture is a project whose run tree owns real checkouts, which is the
// only way to test a flow whose whole job is destroying them.
type discardFixture struct {
	app     *App
	bus     *capturedEventBus
	repo    string
	project store.Project
	root    store.WorkItem
}

func newDiscardFixture(t *testing.T, itemID string) *discardFixture {
	t.Helper()
	app, bus := setupE2EApp(t)
	// Discard is the disposition that MOVES the run it disposes of (it cancels
	// the tree), so what the app emits about the row afterwards is part of this
	// flow's contract, not incidental.
	app.testEmitHook = bus.emit
	// startWorkflowEngineForTest, not a bare initWorkflowEngine: engine startup
	// also arms the §11 scheduler, and only this helper stops it again.
	startWorkflowEngineForTest(t, app, t.TempDir())
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	return &discardFixture{
		app: app, bus: bus, repo: repo, project: projectRow,
		root: createDoneWorkflowWorktree(t, app, projectRow, itemID),
	}
}

// lastItemState returns the final `workflow:item-state` payload emitted for an
// item, and whether one was emitted at all.
func (f *discardFixture) lastItemState(t *testing.T, itemID string) (engine.StateEvent, bool) {
	t.Helper()
	var last engine.StateEvent
	found := false
	for _, event := range f.bus.allEvents() {
		if event.Name != "workflow:item-state" {
			continue
		}
		state, ok := event.Data.(engine.StateEvent)
		if !ok || state.ItemID != itemID {
			continue
		}
		last = state
		found = true
	}
	return last, found
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

// The disposition's own state emit is authoritative for the frontend's run-map
// patcher, which writes the payload's state and reason into the view verbatim.
// Discard CANCELS the tree on its way through, so echoing the copy loaded at
// the top of the call republished a state the run had already left — and a
// payload with no Reason field reads as "the reason was cleared", wiping the
// live one off the row until an unrelated refetch happened by.
func TestWorkflowDiscardEmitsTheRowsActualStateAndReason(t *testing.T) {
	f := newDiscardFixture(t, "discard-parked-item")
	if err := f.app.store.UpdateWorkItemState(
		f.root.ID, string(engine.StateNeedsHuman), string(engine.ReasonGate), time.Now().UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}

	if _, err := f.app.WorkflowDiscardItem(f.root.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := f.app.store.GetWorkItem(f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State == string(engine.StateNeedsHuman) {
		t.Fatalf("discard left the run parked (%+v); the case this pins never happened", stored)
	}
	state, found := f.lastItemState(t, f.root.ID)
	if !found {
		t.Fatal("discard emitted no item-state for the row it disposed of")
	}
	if string(state.To) != stored.State || string(state.From) != stored.State {
		t.Fatalf("emitted state = %s→%s, want the row's own %s", state.From, state.To, stored.State)
	}
	if string(state.Reason) != stored.Reason {
		t.Fatalf("emitted reason = %q, want the row's own %q", state.Reason, stored.Reason)
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
