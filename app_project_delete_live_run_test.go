package main

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// deleteProjectCascadeTimeout bounds the deadlock this test exists to catch. A
// correct DeleteProject finishes in well under a second here; a lock-first one
// never finishes at all.
const deleteProjectCascadeTimeout = 30 * time.Second

// The load-bearing ordering: DeleteProject cascades the workflow discard BEFORE
// it takes a single thread lock.
//
// The run below is live on a provider that acks an interrupt and then says
// nothing else — a wedged CLI. Deleting the project has to cancel it, and
// cancelling walks engine teardown → Runner.Stop → App.InterruptTurn, which
// locks the phase thread. DeleteProject holds a lock on every thread in the
// project, the phase thread among them, so a cascade run after the locks are
// taken blocks on a lock its own caller owns and never returns: the select
// below fails instead of the suite hanging. It would also fail if the cascade
// were dropped altogether, because the live run's open turn would trip the
// thread-activity refusal.
func TestDeleteProjectCancelsLiveWorkflowRunBeforeTakingThreadLocks(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    gate:
      routes:
        - to: done`)
	binary := writeStallingWorkflowClaude(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "reliability-flow", "shared", "live run",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	phaseThreadID := waitForLiveWorkflowPhaseTurn(t, app, item.ID)

	preview, err := app.ProjectDeletionPreview(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.LiveRunIDs) != 1 || preview.LiveRunIDs[0] != item.ID {
		t.Fatalf("preview live runs = %v, want the in-flight run %s", preview.LiveRunIDs, item.ID)
	}

	deleted := make(chan error, 1)
	go func() {
		_, err := app.DeleteProjectDiscardingWorkflowWork(projectRow.ID)
		deleted <- err
	}()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("DeleteProject with a live run: %v", err)
		}
	case <-time.After(deleteProjectCascadeTimeout):
		t.Fatalf(
			"DeleteProject did not return within %s: the discard cascade is running "+
				"under the thread locks it needs App.InterruptTurn to take on %s",
			deleteProjectCascadeTimeout, phaseThreadID,
		)
	}

	if _, err := app.store.GetWorkItem(item.ID); err == nil {
		t.Fatal("the live run's record survived the deletion")
	}
	if _, err := app.store.GetThread(phaseThreadID); err == nil {
		t.Fatal("the phase thread survived the deletion")
	}
	if _, err := app.store.GetProject(projectRow.ID); err == nil {
		t.Fatal("the project row survived the deletion")
	}
}

// waitForLiveWorkflowPhaseTurn blocks until the run is executing a phase on a
// thread with an open turn — the exact state that makes the deletion ordering
// observable. Failing here means the test never reached its own precondition.
func waitForLiveWorkflowPhaseTurn(t *testing.T, app *App, itemID string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		item, err := app.store.GetWorkItem(itemID)
		if err == nil && engine.State(item.State) == engine.StateRunning {
			if threadID, ok := liveWorkflowPhaseThread(t, app, itemID); ok {
				return threadID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a running phase with an open turn", itemID)
	return ""
}

func liveWorkflowPhaseThread(t *testing.T, app *App, itemID string) (string, bool) {
	t.Helper()
	phases, err := app.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range phases {
		if phase.ThreadID == "" {
			continue
		}
		if _, open, err := app.store.GetActiveTurn(phase.ThreadID); err != nil {
			t.Fatal(err)
		} else if open {
			return phase.ThreadID, true
		}
	}
	return "", false
}
