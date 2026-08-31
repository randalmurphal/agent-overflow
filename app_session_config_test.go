package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// seedConfigReconcileThread creates a thread plus a registered fake session
// with no live-update surface, so every config change exercises the
// deferred-restart path. Returns the thread ID and a channel that receives
// each startSession attempt.
func seedConfigReconcileThread(t *testing.T, app *App, id string, liveness *sessionLiveness) (string, chan string) {
	t.Helper()
	thread := testThread(id)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.mu.Lock()
	app.sessions[id] = session{provider: thread.Provider, token: "cfg-token", liveness: liveness}
	app.mu.Unlock()

	started := make(chan string, 4)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}
	// Tight cadence so tests observe the watcher without long sleeps; the
	// quiet window shrinks to (effectively) nothing so only the explicit
	// busy signals under test gate the restart.
	app.configReconnectPollIntervalOverride = 10 * time.Millisecond
	app.configReconnectQuietWindowOverride = time.Nanosecond
	return id, started
}

func assertNoRestartWithin(t *testing.T, started chan string, d time.Duration, context string) {
	t.Helper()
	select {
	case <-started:
		t.Fatalf("session restarted while %s — restart would have killed live work", context)
	case <-time.After(d):
	}
}

func waitRestart(t *testing.T, started chan string, want string) {
	t.Helper()
	select {
	case threadID := <-started:
		if threadID != want {
			t.Fatalf("restart thread = %q, want %q", threadID, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred config reconnect never restarted the session")
	}
}

// TestConfigChangeDefersRestartWhileBackgroundTasksRun pins the core no-kill
// guarantee: a config change that needs a session restart must not fire
// while the thread has running backgrounded tool calls (the restart would
// reap their processes). It fires once the background work settles.
func TestConfigChangeDefersRestartWhileBackgroundTasksRun(t *testing.T) {
	app := newTestAppWithStore(t)
	id, started := seedConfigReconcileThread(t, app, "thread-config-bg", nil)

	bgID := "bg-tool-1"
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:           bgID,
		ThreadID:     id,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: long-running script",
		ToolName:     "Bash",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed running bg tool: %v", err)
	}

	if _, err := app.UpdateThreadReasoningEffort(id, "low"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}

	assertNoRestartWithin(t, started, 200*time.Millisecond, "a backgrounded task was running")

	// Settle the background task the same way triage does: a completion
	// sibling row keyed by completion_of.
	if _, err := app.store.AppendItem(store.Item{
		ID:           "bg-tool-1-completion",
		ThreadID:     id,
		TurnIndex:    1,
		ItemIndex:    1,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		CompletionOf: bgID,
		Summary:      "Bash: long-running script",
		ToolName:     "Bash",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed bg completion: %v", err)
	}

	waitRestart(t, started, id)
}

// TestConfigChangeDefersRestartWhileTurnActive pins the same guarantee for
// an in-flight turn (tracked via session liveness): no restart while a turn
// is open, restart once it completes.
func TestConfigChangeDefersRestartWhileTurnActive(t *testing.T) {
	app := newTestAppWithStore(t)
	liveness := newSessionLiveness(time.Now())
	liveness.activeTurns.Store(1)
	id, started := seedConfigReconcileThread(t, app, "thread-config-turn", liveness)

	if _, err := app.UpdateThreadReasoningEffort(id, "low"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}

	assertNoRestartWithin(t, started, 200*time.Millisecond, "a turn was active")

	liveness.activeTurns.Store(0)
	waitRestart(t, started, id)
}

// TestConfigChangeWithoutSessionSkipsRestart pins the lazy-start contract:
// with no live session there is nothing to reconcile — the next start reads
// the row.
func TestConfigChangeWithoutSessionSkipsRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-config-idle")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.UpdateThreadReasoningEffort(thread.ID, "low"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}
	assertNoRestartWithin(t, started, 100*time.Millisecond, "no session existed")
}

// TestThreadConfigBusySignals covers each busy signal the deferred-restart
// watcher consults.
func TestThreadConfigBusySignals(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-config-busy")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.configReconnectQuietWindowOverride = time.Nanosecond

	if app.threadConfigBusy(thread.ID) {
		t.Fatal("fresh thread reported busy")
	}

	// Recent session activity inside the quiet window.
	app.configReconnectQuietWindowOverride = time.Hour
	liveness := newSessionLiveness(time.Now())
	app.mu.Lock()
	app.sessions[thread.ID] = session{provider: thread.Provider, token: "busy-token", liveness: liveness}
	app.mu.Unlock()
	if !app.threadConfigBusy(thread.ID) {
		t.Fatal("recent activity inside the quiet window not reported busy")
	}
	app.configReconnectQuietWindowOverride = time.Nanosecond
	if app.threadConfigBusy(thread.ID) {
		t.Fatal("quiet session reported busy")
	}

	// Active turn.
	liveness.activeTurns.Store(1)
	if !app.threadConfigBusy(thread.ID) {
		t.Fatal("active turn not reported busy")
	}
	liveness.activeTurns.Store(0)

	// In-flight flush dispatch.
	app.flushDispatch.mu.Lock()
	if app.flushDispatch.inflightItems == nil {
		app.flushDispatch.inflightItems = map[string]int{}
	}
	app.flushDispatch.inflightItems[thread.ID] = 1
	app.flushDispatch.mu.Unlock()
	if !app.threadConfigBusy(thread.ID) {
		t.Fatal("in-flight flush dispatch not reported busy")
	}
	app.flushDispatch.mu.Lock()
	delete(app.flushDispatch.inflightItems, thread.ID)
	app.flushDispatch.mu.Unlock()

	// Running backgrounded tool call.
	now := time.Now().UnixMilli()
	if _, err := app.store.AppendItem(store.Item{
		ID:           "busy-bg",
		ThreadID:     thread.ID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: bg",
		ToolName:     "Bash",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed running bg tool: %v", err)
	}
	if !app.threadConfigBusy(thread.ID) {
		t.Fatal("running background task not reported busy")
	}
}

// TestEffortChangeLiveAppliesOnCodexSessionWithoutRestart pins the live
// path end to end at the app layer: an effort change on a thread with a
// real (mock-scripted) Codex session is absorbed through the per-turn
// override — no restart is ever scheduled — and the session's stored
// launch options advance so later reconciles diff against the applied
// config.
func TestEffortChangeLiveAppliesOnCodexSessionWithoutRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-config-live-codex")
	thread.ReasoningEffort = string(provider.EffortHigh)
	// The mock provider process spawns with cwd = workspace path, so it
	// must exist.
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-live\"}}}"
done
`
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	// Build launch options from the stored row (CreateThread normalizes
	// fields like the context window), mirroring what a real spawn reads.
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	launchOpts, err := app.buildSessionOptions(app.sanitizeThreadModelSettings(stored))
	if err != nil {
		t.Fatalf("buildSessionOptions() error = %v", err)
	}
	cfg := codex.ConfigFromOptions(launchOpts)
	cfg.Binary = scriptPath
	sess, err := codex.NewSession(context.Background(), thread.ID, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("codex.NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.mu.Lock()
	app.sessions[thread.ID] = session{
		provider:   thread.Provider,
		token:      "live-codex-token",
		codex:      sess,
		launchOpts: launchOpts,
		liveness:   newSessionLiveness(time.Now()),
	}
	app.mu.Unlock()

	app.startSessionFn = func(threadID string) error {
		return fmt.Errorf("unexpected restart of %s — effort must live-apply on codex", threadID)
	}
	app.configReconnectPollIntervalOverride = 10 * time.Millisecond
	app.configReconnectQuietWindowOverride = time.Nanosecond

	if _, err := app.UpdateThreadReasoningEffort(thread.ID, "low"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}

	app.mu.Lock()
	got := app.sessions[thread.ID].launchOpts.ReasoningEffort
	app.mu.Unlock()
	if got != provider.EffortLow {
		t.Fatalf("session launchOpts effort = %q, want low (live apply must advance stored options)", got)
	}

	// No pending watcher may exist — the live apply fully converged.
	app.mu.Lock()
	pending := app.pendingConfigReconnects[thread.ID]
	app.mu.Unlock()
	if pending {
		t.Fatal("live-applied change left a deferred restart pending")
	}
}

// TestReconcileSessionConfigFoldsStackedChanges pins watcher idempotence: a
// second config change while a restart is already pending folds into the
// same watcher (one restart, against the latest row).
func TestReconcileSessionConfigFoldsStackedChanges(t *testing.T) {
	app := newTestAppWithStore(t)
	liveness := newSessionLiveness(time.Now())
	liveness.activeTurns.Store(1)
	id, started := seedConfigReconcileThread(t, app, "thread-config-stacked", liveness)

	if _, err := app.UpdateThreadReasoningEffort(id, "low"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}
	if _, err := app.UpdateThreadReasoningEffort(id, "high"); err != nil {
		t.Fatalf("UpdateThreadReasoningEffort() error = %v", err)
	}

	liveness.activeTurns.Store(0)
	waitRestart(t, started, id)

	stored, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.ReasoningEffort != string(provider.EffortHigh) {
		t.Fatalf("stored effort = %q, want high", stored.ReasoningEffort)
	}
	// No second restart for the folded change.
	assertNoRestartWithin(t, started, 100*time.Millisecond, "the stacked change was already applied")
}
