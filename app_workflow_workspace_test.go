package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

func TestWorkflowWritingItemProvisionsHooksAndCapturesArtifact(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
worktree_setup:
  copy: [.env]
  run:
    - [/bin/sh, -c, "printf hook-ran > setup.txt"]
  timeout: 2s
`)
	cwdCapture := filepath.Join(t.TempDir(), "cwd")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, cwdCapture, true, "done")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "write", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	if item.WorktreePath == "" || item.Branch == "" || item.BaseBranch != "main" || !strings.Contains(item.Branch, "workflow-workspace-flow") {
		t.Fatalf("provisioned workspace = %+v", item)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	for path, want := range map[string]string{
		".env":           "TOKEN=test\n",
		"setup.txt":      "hook-ran",
		"deliverable.md": "artifact body\n",
	} {
		data, readErr := os.ReadFile(filepath.Join(item.WorktreePath, path))
		if readErr != nil || string(data) != want {
			t.Fatalf("worktree file %s = %q, %v; want %q", path, data, readErr, want)
		}
	}
	cwd, err := os.ReadFile(cwdCapture)
	if err != nil || filepath.Clean(strings.TrimSpace(string(cwd))) != filepath.Clean(item.WorktreePath) {
		t.Fatalf("provider cwd = %q, %v; want %q", cwd, err, item.WorktreePath)
	}
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Name != "report" || filepath.Ext(detail.Artifacts[0].Path) != ".md" || detail.Artifacts[0].Size != int64(len("artifact body\n")) {
		t.Fatalf("artifacts = %+v", detail.Artifacts)
	}
	artifact, err := os.ReadFile(detail.Artifacts[0].Path)
	if err != nil || string(artifact) != "artifact body\n" {
		t.Fatalf("captured artifact = %q, %v", artifact, err)
	}
	if len(detail.Phases) != 1 {
		t.Fatalf("phases = %+v", detail.Phases)
	}
	thread, err := app.store.GetThread(detail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.WorkspacePath != item.WorktreePath || thread.WorktreePath != item.WorktreePath || thread.Branch != item.Branch {
		t.Fatalf("phase thread workspace = %+v, item=%+v", thread, item)
	}
}

func TestWorkflowHookFailureAndTimeoutParkSetupFailed(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		timeout string
	}{
		{name: "exit", command: "[/bin/sh, -c, \"printf failed-output; exit 7\"]", timeout: "2s"},
		{name: "timeout", command: "[/bin/sleep, 1]", timeout: "20ms"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, bus := setupE2EApp(t)
			app.testEmitHook = bus.emit
			configRoot := t.TempDir()
			repo := testutil.InitGitRepo(t)
			projectRow := testutil.EnsureProject(t, app.store, repo)
			projectRow = mustReloadProject(t, app.store, projectRow.ID)
			writeWorkspaceWorkflow(t, configRoot, "done")
			writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\nreliability:\n  watchdog: 1h\n  backoff: [5ms]\nworktree_setup:\n  run:\n    - "+test.command+"\n  timeout: "+test.timeout+"\n")
			if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "done")}); err != nil {
				t.Fatal(err)
			}
			startWorkflowEngineForTest(t, app, configRoot)
			if err := app.WorkflowSetGlobalPause(true); err != nil {
				t.Fatal(err)
			}
			item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", test.name, json.RawMessage(`{}`), nil, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.WorkflowSetGlobalPause(false); err != nil {
				t.Fatalf("fire-and-forget unpause returned provisioning failure inline: %v", err)
			}
			item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonSetupFailed)
			assertWorkflowSetupFailureEvents(t, bus, item.ID)
			if item.WorktreePath != "" || item.Branch != "" {
				t.Fatalf("failed setup persisted workspace = %+v", item)
			}
			if item.BaseBranch != "main" {
				t.Fatalf("setup rollback lost intake base branch = %q, want main", item.BaseBranch)
			}
			worktrees, err := app.gitCore().ListWorktrees(repo)
			if err != nil || len(worktrees) != 1 {
				t.Fatalf("worktrees after rollback = %+v, %v", worktrees, err)
			}
			if test.name == "exit" {
				writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\nreliability:\n  watchdog: 1h\n  backoff: [5ms]\nworktree_setup:\n  timeout: 2s\n")
				if err := app.WorkflowResumeItem(context.Background(), item.ID, ""); err != nil {
					t.Fatal(err)
				}
				item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
				if item.WorktreePath == "" {
					t.Fatal("setup retry did not provision a new worktree")
				}
				t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
			}
		})
	}
}

func assertWorkflowSetupFailureEvents(t *testing.T, bus *capturedEventBus, itemID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	foundState, foundError := false, false
	for !foundState || !foundError {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("setup failure events incomplete: item-state=%v workflow:error=%v", foundState, foundError)
		}
		event := bus.next(t, remaining)
		switch event.Name {
		case "workflow:item-state":
			state, ok := event.Data.(engine.StateEvent)
			if ok && state.ItemID == itemID && state.From == engine.StateRunning &&
				state.To == engine.StateNeedsHuman && state.Reason == engine.ReasonSetupFailed {
				foundState = true
			}
		case "workflow:error":
			workflowErr, ok := event.Data.(engine.ErrorEvent)
			if ok && workflowErr.ItemID == itemID {
				if !errors.Is(workflowErr.Cause(), engine.ErrSetupFailed) {
					t.Fatalf("workflow:error cause = %v, want setup failure", workflowErr.Cause())
				}
				if workflowErr.Error == "" {
					t.Fatal("workflow:error omitted user-facing message")
				}
				foundError = true
			}
		}
	}
}

func TestWorkflowResumeWithMissingWorktreeParksSetupFailed(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "stuck")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\nreliability:\n  watchdog: 1h\n  backoff: [5ms]\nworktree_setup:\n  timeout: 2s\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "stuck")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "missing", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonStuck)
	if err := app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true); err != nil {
		t.Fatal(err)
	}
	if err := app.WorkflowResumeItem(context.Background(), item.ID, ""); err == nil {
		t.Fatal("resume with missing worktree succeeded")
	}
	got := waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonSetupFailed)
	if got.WorktreePath != item.WorktreePath || got.Branch != item.Branch {
		t.Fatalf("missing-worktree resume rewrote workspace: before=%+v after=%+v", item, got)
	}
}

func TestWorkflowRecoversInterruptedProvisioningWithoutSecondWorktree(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\nreliability:\n  watchdog: 1h\n  backoff: [5ms]\nworktree_setup:\n  run:\n    - [/bin/sh, -c, \"printf recovered > recovered.txt\"]\n  timeout: 2s\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), true, "done")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "recover provision", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	branch := workflowWorktreeBranch(app.worktreeBranchPrefix(), "workspace-flow", item.ID)
	interruptedPath, err := app.defaultWorktreePath(repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.gitCore().CreateWorktreeFromBranch(repo, interruptedPath, "main", branch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, interruptedPath, true) })
	if err := app.WorkflowSetGlobalPause(false); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	if item.WorktreePath != interruptedPath || item.Branch != branch {
		t.Fatalf("recovered workspace = %+v, want %q on %q", item, interruptedPath, branch)
	}
	if _, err := os.Stat(filepath.Join(interruptedPath, "recovered.txt")); err != nil {
		t.Fatalf("recovered setup hook did not run: %v", err)
	}
	worktrees, err := app.gitCore().ListWorktrees(repo)
	if err != nil || len(worktrees) != 2 {
		t.Fatalf("worktrees after recovery = %+v, %v", worktrees, err)
	}
}

func TestWorkflowStepModeBindingParksThenApprovesToDone(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    access: read-only
    outputs:
      ready:
        schema:
          type: boolean
    gate:
      routes:
        - to: done`)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, t.TempDir()).ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeBudgetWorkflowClaude(t)}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "reliability-flow", "shared", "step", json.RawMessage(`{}`), nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonGate)
	if err := app.WorkflowResolveGate(item.ID, "reject", ""); err == nil || !strings.Contains(err.Error(), "step gates support approve") {
		t.Fatalf("reject error = %v", err)
	}
	if err := app.WorkflowResolveGate(item.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
}

func TestWorkflowArtifactFailureDoesNotChangePhaseOutcome(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, "\nbase_branch: main\nreliability:\n  watchdog: 1h\n  backoff: [5ms]\nworktree_setup:\n  timeout: 2s\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaudeWithArtifactPath(t, filepath.Join(t.TempDir(), "cwd"), false, "done", "../escape.md")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "artifact failure", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Artifacts) != 0 || len(detail.Phases) != 1 || detail.Phases[0].Status != "completed" {
		t.Fatalf("artifact failure changed outcome: %+v", detail)
	}
	bus.mu.Lock()
	events := append([]capturedEvent(nil), bus.all...)
	bus.mu.Unlock()
	found := false
	for _, event := range events {
		payload, ok := event.Data.(map[string]any)
		if event.Name == "workflow:error" && ok && payload["output"] == "report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifact error naming report was not emitted: %s", summarizeEvents(events))
	}
}

func TestWorkflowWorkspacePureHelpers(t *testing.T) {
	project := t.TempDir()
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "config", "local.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkflowSetupEntries(context.Background(), project, worktree, []string{"config/*.env"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, "config", "local.env")); err != nil || string(data) != "x" {
		t.Fatalf("copied setup file = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("source-git"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("worktree-git"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkflowSetupEntries(context.Background(), project, worktree, []string{"*"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(worktree, ".git")); err != nil || string(data) != "worktree-git" {
		t.Fatalf("wildcard replaced worktree .git = %q, %v", data, err)
	}
	for _, pattern := range []string{"../outside", "/absolute", ".git/config"} {
		if err := copyWorkflowSetupEntries(context.Background(), project, worktree, []string{pattern}); err == nil {
			t.Fatalf("unsafe pattern %q succeeded", pattern)
		}
	}
	started := time.Now()
	if err := runWorkflowWorktreeSetup(context.Background(), project, worktree, profile.WorktreeSetup{
		Run: [][]string{{"/bin/sh", "-c", "sleep 10 & wait"}}, Timeout: "20ms",
	}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("process-tree timeout took %s", elapsed)
	}

	artifactWorkspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifactWorkspace, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	if err := captureWorkflowArtifact(dataRoot, "item", "result", artifactWorkspace, "report.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactWorkspace, "report.md"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := captureWorkflowArtifact(dataRoot, "item", "result", artifactWorkspace, "report.md"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowArtifactDir(dataRoot, "item"), ".ao-copy-crash"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed, err := listWorkflowArtifacts(dataRoot, "item")
	if err != nil || len(listed) != 1 || listed[0].Name != "result" || filepath.Ext(listed[0].Path) != ".md" {
		t.Fatalf("listed artifacts = %+v, %v", listed, err)
	}
	if err := captureWorkflowArtifact(dataRoot, "item", "escape", artifactWorkspace, "../report.txt"); err == nil {
		t.Fatal("escaping artifact path succeeded")
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "outside.env"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(project, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkflowSetupEntries(context.Background(), project, worktree, []string{"linked/*.env"}); err == nil {
		t.Fatal("symlinked source ancestor succeeded")
	}
	symlinkRoot := t.TempDir()
	if err := os.Symlink(external, filepath.Join(symlinkRoot, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkflowDestination(symlinkRoot, filepath.Join(symlinkRoot, "redirect", "file")); err == nil {
		t.Fatal("symlinked destination parent succeeded")
	}
	if branch := workflowWorktreeBranch("task", "flow", "12345678-abcd"); !strings.HasPrefix(branch, "task-workflow-flow-12345678-abcd-") {
		t.Fatalf("workflow branch = %q", branch)
	}
}

func writeWorkspaceWorkflow(t *testing.T, configRoot, outcome string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: workspace-flow
name: Workspace flow
outputs:
  report:
    from: write.report
    artifact: true
phases:
  - id: write
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: write.md
    access: write
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: auto
`
	if err := os.WriteFile(filepath.Join(dir, "workspace-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "write.md"), []byte("Write the deliverable for "+outcome), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWorkspaceProfile(t *testing.T, configRoot, slug, contents string) {
	t.Helper()
	dir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWorkspaceClaude(t *testing.T, cwdCapture string, writeArtifact bool, outcome string) string {
	return writeWorkspaceClaudeWithArtifactPath(t, cwdCapture, writeArtifact, outcome, "deliverable.md")
}

func writeWorkspaceClaudeWithArtifactPath(t *testing.T, cwdCapture string, writeArtifact bool, outcome, artifactPath string) string {
	t.Helper()
	artifactCommand := ""
	if writeArtifact {
		artifactCommand = `printf 'artifact body\n' > deliverable.md`
	}
	encodedPath, err := json.Marshal(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	status := `{"status":"done","outputs":{"report":` + string(encodedPath) + `},"question":null,"reason":null}`
	if outcome == "stuck" {
		status = `{"status":"stuck","outputs":null,"question":null,"reason":"blocked"}`
	}
	script := `#!/bin/bash
while IFS= read -r line; do
  pwd > ` + workflowShellQuote(cwdCapture) + `
  ` + artifactCommand + `
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"workspace","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + status + `}'
done
`
	return writeExecutable(t, "workspace-claude.sh", script)
}

func workflowShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
