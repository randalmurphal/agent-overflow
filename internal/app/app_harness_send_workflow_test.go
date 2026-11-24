package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// An awaited harness send to the thread of a live workflow phase is refused
// with the working-thread error and leaves the run as it was: still running,
// with no intervention recorded.
func TestHarnessSendAtRestOnlyLeavesALivePhaseRunning(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeLiveTakeoverWorkflowFixture(t, configRoot)
	claudeBinary, _ := writeLiveTakeoverClaude(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": claudeBinary}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowApplication().Engine() != nil {
			_ = app.workflowApplication().Engine().Close()
		}
	})
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item, err := app.WorkflowStartRun(
		projectRow.ID, "live-takeover", "shared", "keep running",
		json.RawMessage(`{"goal":"keep running"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	threadID := waitForRunningWorkflowThread(t, app, item.ID)

	if _, err := (&harnessHost{app: app}).Send(context.Background(), threadID, "probe", true); !errors.Is(err, errWorkflowRunning) {
		t.Fatalf("at-rest send to a live phase = %v, want the running-workflow refusal", err)
	}
	if err := app.workflowApplication().Engine().Sync(); err != nil {
		t.Fatal(err)
	}
	requireWorkflowItemState(t, app.store, item.ID, engine.StateRunning, "")
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range phases {
		if len(phase.Intervention) != 0 {
			t.Fatalf("phase %s/%d recorded an intervention: %s", phase.PhaseID, phase.Attempt, phase.Intervention)
		}
	}
}

// The same for a running fan-out unit: the unit keeps running and is not
// detached.
func TestHarnessSendAtRestOnlyLeavesALiveUnitRunning(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := initMainGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeFanOutWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeFanOutTakeoverClaude(t, filepath.Join(t.TempDir(), "beta-hung")),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "fanout-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	threadID := waitForRunningWorkflowUnitThread(t, app, item.ID, "beta")

	if _, err := (&harnessHost{app: app}).Send(context.Background(), threadID, "probe", true); !errors.Is(err, errWorkflowRunning) {
		t.Fatalf("at-rest send to a live unit = %v, want the running-workflow refusal", err)
	}
	if err := app.workflowApplication().Engine().Sync(); err != nil {
		t.Fatal(err)
	}
	requireWorkflowItemState(t, app.store, item.ID, engine.StateRunning, "")
	if beta := unitsByID(t, app, item.ID)["beta"]; beta.Status != store.WorkItemUnitRunning {
		t.Fatalf("unit beta = %q, want still running", beta.Status)
	}
}
