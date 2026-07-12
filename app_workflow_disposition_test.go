package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	if receipt.Action != "merged" || receipt.Mode != "ff" || receipt.Policy != "manual" || receipt.SHA == "" {
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

	workflowEmitter{app: app, emit: func(string, any) {}}.Emit(
		"workflow:item-state", engine.StateEvent{ItemID: item.ID, From: engine.StateRunning, To: engine.StateDone},
	)
	app.workflowAutoDispositionWG.Wait()
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

func TestWorkflowDiscardAcceptsEverySpecifiedTerminalAndParkedState(t *testing.T) {
	for _, state := range []engine.State{engine.StateFailed, engine.StateCancelled, engine.StateNeedsHuman} {
		t.Run(string(state), func(t *testing.T) {
			app, _ := setupE2EApp(t)
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
		})
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
	if receipt.Action != "pr" || receipt.PRRef != "https://github.com/example/agent-overflow/pull/42" || receipt.SHA == "" {
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
	workflowEmitter{app: app, emit: func(string, any) {}}.Emit(
		"workflow:item-state", engine.StateEvent{ItemID: autoItem.ID, From: engine.StateRunning, To: engine.StateDone},
	)
	app.workflowAutoDispositionWG.Wait()
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
