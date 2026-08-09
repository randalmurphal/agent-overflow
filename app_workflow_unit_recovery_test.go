package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// Human recovery of one fan-out unit: a failed unit parks the attempt with its
// siblings' results intact, and retry / drop / takeover repair that attempt in
// place rather than replacing it. The fixtures these use live in
// app_workflow_fanout_test.go.

// TestWorkflowUnitFailureParksAndRetryCompletesTheRun covers the recovery loop a
// human drives: one unit fails, the attempt parks with its siblings' results
// intact, WorkflowRetryUnit re-runs only the failed unit on a fresh try, and the
// join then produces the phase's envelope.
func TestWorkflowUnitFailureParksAndRetryCompletesTheRun(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t, "BETA-UNIT")
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	units := unitsByID(t, app, item.ID)
	if units["alpha"].Status != store.WorkItemUnitDone || units["beta"].Status != store.WorkItemUnitFailed {
		t.Fatalf("unit statuses = alpha:%q beta:%q", units["alpha"].Status, units["beta"].Status)
	}
	if units["merge"].Status != store.WorkItemUnitPending {
		t.Fatalf("join status = %q, want pending — it must not run over a failed unit", units["merge"].Status)
	}
	// The failed unit keeps its worktree and its branch: the work is evidence a
	// human retries or salvages from.
	if units["beta"].WorktreePath == "" || units["beta"].Branch == "" {
		t.Fatalf("failed unit lost its isolation: %+v", units["beta"])
	}
	if _, err := os.Stat(units["beta"].WorktreePath); err != nil {
		t.Fatalf("failed unit worktree %q: %v", units["beta"].WorktreePath, err)
	}
	failedWorktree := units["beta"].WorktreePath
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, failedWorktree, true) })

	// A failed unit's thread stays inspectable but is not steerable: takeover
	// detaches a LIVE unit, and this one has already ended. The recovery actions
	// for a settled unit are retry and drop, and the refusal says so rather than
	// accepting a send nothing would consume.
	if items, itemsErr := app.store.ListItems(units["beta"].ThreadID); itemsErr != nil || len(items) == 0 {
		t.Fatalf("failed unit thread is not inspectable: %d items, %v", len(items), itemsErr)
	}
	if err := app.SendMessage(units["beta"].ThreadID, "let me fix this myself", nil); err == nil ||
		!strings.Contains(err.Error(), "only a running unit can be taken over") {
		t.Fatalf("send into a failed unit thread = %v, want a takeover refusal", err)
	}
	if err := app.WorkflowTakeOverUnit(item.ID, "beta"); err == nil {
		t.Fatal("WorkflowTakeOverUnit accepted a unit whose run already ended")
	}

	if err := app.WorkflowRetryUnit(context.Background(), item.ID, "beta", "the merge script is fixed"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units = unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("after retry unit %q status = %q", id, units[id].Status)
		}
	}
	// The retried unit reused its row and counted a second try, and its second try
	// was cut fresh from the item's branch rather than inheriting the failed one.
	if units["beta"].UnitAttempt != 2 {
		t.Fatalf("retried unit try = %d, want 2", units["beta"].UnitAttempt)
	}
	if want := workflowUnitBranch(item.Branch, workflowUnitWorkspaceRef{
		itemID: item.ID, phaseID: "port", attempt: 1, unitID: "beta", unitAttempt: 2,
	}); units["beta"].Branch != want {
		t.Fatalf("retried unit branch = %q, want %q", units["beta"].Branch, want)
	}
	if units["alpha"].UnitAttempt != 1 {
		t.Fatalf("a sibling was re-run: alpha try = %d", units["alpha"].UnitAttempt)
	}
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Attempt != 1 {
		t.Fatalf("repairing a unit replaced the attempt: %+v", phases)
	}
}

// TestWorkflowDropUnitLetsTheJoinProceedOverSurvivors is the other half of unit
// recovery: the human accepts the unit's absence and the attempt finishes
// without it.
func TestWorkflowDropUnitLetsTheJoinProceedOverSurvivors(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t, "BETA-UNIT")
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	if err := app.WorkflowDropUnit(item.ID, "beta", "not needed for this port"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units := unitsByID(t, app, item.ID)
	if units["beta"].Status != store.WorkItemUnitDropped {
		t.Fatalf("dropped unit status = %q", units["beta"].Status)
	}
	if units["merge"].Status != store.WorkItemUnitDone {
		t.Fatalf("join status = %q, want done over the survivors", units["merge"].Status)
	}
	// The attempt ended on a done join, so every unit checkout is retired —
	// including the dropped one, whose branch is what preserves whatever it
	// committed before the human let it go.
	if units["beta"].WorktreePath != "" {
		t.Fatalf("dropped unit still records worktree %q after a done join", units["beta"].WorktreePath)
	}
	branches := map[string]bool{}
	for _, branch := range mustListBranches(t, app, repo) {
		branches[branch.Name] = true
	}
	if !branches[units["beta"].Branch] {
		t.Fatalf("dropped unit branch %q was removed with its worktree", units["beta"].Branch)
	}
}

// TestWorkflowRetryFailedUnitsRepairsEveryFailedUnitAtOnce is the many-at-once
// half of unit recovery: one cause — a provider usage limit — fails every unit
// of the attempt, and one call puts all of them back on the same attempt. The
// per-unit outcome is the same as calling WorkflowRetryUnit on each: a fresh
// try on a fresh branch cut from the item's, on the attempt row that already
// exists.
func TestWorkflowRetryFailedUnitsRepairsEveryFailedUnitAtOnce(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t, "ALPHA-UNIT", "BETA-UNIT")
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	units := unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta"} {
		if units[id].Status != store.WorkItemUnitFailed {
			t.Fatalf("unit %q status = %q, want every unit failed", id, units[id].Status)
		}
	}
	if units["merge"].Status != store.WorkItemUnitPending {
		t.Fatalf("join status = %q, want pending over a wholly failed attempt", units["merge"].Status)
	}
	for _, id := range []string{"alpha", "beta"} {
		failedWorktree := units[id].WorktreePath
		if failedWorktree == "" {
			t.Fatalf("failed unit %q lost its isolation: %+v", id, units[id])
		}
		t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, failedWorktree, true) })
	}

	if err := app.WorkflowRetryFailedUnits(context.Background(), item.ID, "the usage limit reset"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units = unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("after retry-all unit %q status = %q", id, units[id].Status)
		}
	}
	for _, id := range []string{"alpha", "beta"} {
		if units[id].UnitAttempt != 2 {
			t.Fatalf("repaired unit %q try = %d, want 2", id, units[id].UnitAttempt)
		}
		if want := workflowUnitBranch(item.Branch, workflowUnitWorkspaceRef{
			itemID: item.ID, phaseID: "port", attempt: 1, unitID: id, unitAttempt: 2,
		}); units[id].Branch != want {
			t.Fatalf("repaired unit %q branch = %q, want %q", id, units[id].Branch, want)
		}
	}
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Attempt != 1 {
		t.Fatalf("repairing every unit replaced the attempt: %+v", phases)
	}
}

// A run with nothing failed is refused rather than quietly resumed: the CLI
// caller that mistimed a usage-reset poll has to see that its repair did not
// happen.
func TestWorkflowRetryFailedUnitsRefusesARunWithNothingFailed(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t)
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	if err := app.WorkflowRetryFailedUnits(context.Background(), item.ID, ""); err == nil {
		t.Fatal("WorkflowRetryFailedUnits accepted a run that has nothing to repair")
	}
}

// startFailingFanOutRun boots a fan-out run whose named units fail on their
// first try and succeed on any later one, and returns once the run has started.
// The tokens are the unit markers in the prompt bodies (`ALPHA-UNIT`,
// `BETA-UNIT`), so a caller picks between the one-unit park and the
// many-at-once park a usage limit produces.
func startFailingFanOutRun(t *testing.T, failFirstTurn ...string) (*App, store.WorkItem, string) {
	t.Helper()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeFanOutWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	cwdLog := filepath.Join(t.TempDir(), "cwds.txt")
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeFanOutClaude(t, cwdLog, t.TempDir(), failFirstTurn...),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "fanout-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return app, item, repo
}

// TestWorkflowTakeOverLiveUnitSteersThenRetriesUnderEngineControl covers the
// live-unit takeover path end to end: one unit is detached mid-turn for human
// steering while its siblings finish, the human's send runs schema-less on the
// unit's own thread, and the run completes once the unit is handed back through
// a retry.
func TestWorkflowTakeOverLiveUnitSteersThenRetriesUnderEngineControl(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
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

	if err := app.WorkflowTakeOverUnit(item.ID, "beta"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonTakenOver)
	units := unitsByID(t, app, item.ID)
	if units["beta"].Status != store.WorkItemUnitTakenOver {
		t.Fatalf("taken-over unit status = %q", units["beta"].Status)
	}
	// Its siblings were left alone: taking over one unit is not taking over the
	// attempt, and the join still has not run.
	if units["alpha"].Status != store.WorkItemUnitDone || units["merge"].Status != store.WorkItemUnitPending {
		t.Fatalf("takeover disturbed the attempt: alpha=%q merge=%q", units["alpha"].Status, units["merge"].Status)
	}
	if units["beta"].ThreadID != threadID {
		t.Fatalf("unit thread changed on takeover: %q vs %q", units["beta"].ThreadID, threadID)
	}

	steered := make(chan struct{}, 1)
	unsubscribe := app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		if event.Kind == provider.EventTurnComplete {
			select {
			case steered <- struct{}{}:
			default:
			}
		}
	})
	if err := app.SendMessage(threadID, "I unblocked the beta slice by hand.", nil); err != nil {
		unsubscribe()
		t.Fatal(err)
	}
	select {
	case <-steered:
	case <-time.After(8 * time.Second):
		unsubscribe()
		t.Fatal("schema-less steering turn on the unit thread did not complete")
	}
	unsubscribe()

	if err := app.WorkflowRetryUnit(context.Background(), item.ID, "beta", "steered by hand; run it again"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	units = unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("after handing the unit back, %q status = %q", id, units[id].Status)
		}
	}
	// The retry ran on a new thread under engine control; the steered one stays
	// as the record of what the human did.
	if units["beta"].ThreadID == threadID {
		t.Fatal("the retried try reused the steering thread")
	}
	if units["beta"].UnitAttempt != 2 {
		t.Fatalf("retried unit try = %d, want 2", units["beta"].UnitAttempt)
	}
	steeredThread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if steeredThread.Mode != "workflow" {
		t.Fatalf("steered unit thread mode = %q", steeredThread.Mode)
	}
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	var userMessages []string
	for _, timelineItem := range items {
		if timelineItem.Kind == "user_text" && timelineItem.Role == "user" {
			userMessages = append(userMessages, timelineItem.Summary)
		}
	}
	if len(userMessages) != 2 || !strings.Contains(userMessages[1], "I unblocked the beta slice") {
		t.Fatalf("steering turns on the unit thread = %#v", userMessages)
	}
}

func waitForRunningWorkflowUnitThread(t *testing.T, app *App, itemID, unitID string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		units, err := app.store.ListWorkItemUnits(itemID)
		if err != nil {
			t.Fatal(err)
		}
		for _, unit := range units {
			if unit.UnitID != unitID || unit.Status != store.WorkItemUnitRunning || unit.ThreadID == "" {
				continue
			}
			if _, active, activeErr := app.store.GetActiveTurn(unit.ThreadID); activeErr == nil && active {
				return unit.ThreadID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unit %q of item %s never exposed a live thread", unitID, itemID)
	return ""
}

// writeFanOutTakeoverClaude hangs the beta unit's first turn so it can be taken
// over mid-flight, answers the interrupt the takeover sends, runs the human's
// steering turn without a schema, and completes every workflow turn after that.
func writeFanOutTakeoverClaude(t *testing.T, hangMarker string) string {
	t.Helper()
	done := `{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null}`
	script := `#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"subtype":"interrupt"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming"}'
      continue
      ;;
  esac
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"fanout-takeover","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  if [[ "$line" == *BETA-UNIT* && ! -f ` + workflowShellQuote(hangMarker) + ` ]]; then
    : > ` + workflowShellQuote(hangMarker) + `
    continue
  fi
  if [[ "$line" == *workflow-system-instructions* ]]; then
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + done + `}'
    continue
  fi
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
done
`
	return writeExecutable(t, "fanout-takeover-claude.sh", script)
}
