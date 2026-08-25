package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	app.workflowAutoDisposition.Wait()
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
			t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
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
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	autoItem := createDoneWorkflowWorktree(t, app, projectRow, "auto-pr-item")
	testutil.RunGit(t, repo, "config", "--unset-all", "url."+remote+".insteadOf")
	if forge := app.gitCore().DetectForge(autoItem.WorktreePath); forge != "github" {
		t.Fatalf("auto detected forge = %q", forge)
	}
	testutil.RunGit(t, repo, "config", "url."+remote+".insteadOf", remoteURL)
	workflowEmitter{app: app, emit: func(eventchan.Channel, any) {}}.Emit(
		"workflow:item-state", engine.StateEvent{ItemID: autoItem.ID, ProjectID: autoItem.ProjectID, From: engine.StateRunning, To: engine.StateDone},
	)
	app.workflowAutoDisposition.Wait()
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
