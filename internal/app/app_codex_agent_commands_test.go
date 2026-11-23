package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// seedCodexAgentCommand leaves threadID with one running command that the
// agent of spawn launch owner started, backgrounded by a poll from the
// agent's thread, and no open turn: what a gate on ending the session
// sees once the main agent's turn is over.
func seedCodexAgentCommand(t *testing.T, app *App, threadID, itemID, processID, owner string) {
	t.Helper()
	handle := func(evt provider.ProviderEvent) {
		t.Helper()
		evt.ThreadID, evt.Timestamp = threadID, time.Now()
		if err := app.triage.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	startMeta, err := json.Marshal(map[string]any{
		"source": "unifiedExecStartup", "item_status": "inProgress", "process_id": processID,
		"toolName": "command_execution", "input": map[string]any{"command": "pnpm run dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: threadID + ":" + itemID})
	handle(provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: itemID, ItemType: "commandExecution", TurnID: threadID + ":" + itemID, ParentToolUseID: owner, Meta: startMeta})
	handle(provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ParentToolUseID: owner, Meta: json.RawMessage(`{"process_id":"` + processID + `"}`)})
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnID: threadID + ":" + itemID, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
	if app.triage.HasInFlightTurnOrRound(threadID) || app.triage.HasPendingWork(threadID) {
		t.Fatal("precondition: the thread still has a turn or pending work")
	}
}

// unloadCodexAgent is the status Codex publishes when it shuts an agent's
// thread down.
func unloadCodexAgent(t *testing.T, app *App, threadID, owner, childThreadID string) {
	t.Helper()
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: owner, ParentToolUseID: owner,
		Meta: json.RawMessage(`{"agent_path":"` + childThreadID + `","status":"notLoaded"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent status: %v", err)
	}
}

func createCodexAgentCommandThread(t *testing.T, app *App, id string) string {
	t.Helper()
	thread := testThread(id)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread.ID
}

// The idle reaper keeps a session while a command an agent started runs,
// and closes it once the agent's thread unloads and takes the command with
// it.
func TestReapIdleSessionsKeepsASessionWhileAnAgentCommandRuns(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-reap")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")

	now := time.Now()
	app.sessionManager().put(threadID, session{
		Provider: string(provider.Codex),
		Token:    "tok",
		Liveness: newSessionLiveness(now.Add(-idleReapThreshold - time.Minute)),
	})
	app.reapIdleSessions(now)
	if _, present := app.sessionManager().get(threadID); !present {
		t.Fatal("the session was reaped while an agent's command ran")
	}

	unloadCodexAgent(t, app, threadID, "spawn-1", "child-1")
	app.reapIdleSessions(now)
	if _, present := app.sessionManager().get(threadID); present {
		t.Fatal("the session was kept after the agent's thread unloaded")
	}
}

// The Codex account switch restarts the session, so it waits for an
// agent's command as it does for the main agent's.
func TestCodexAccountSwitchWaitsForAnAgentCommand(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-account")
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	installTestProviderAccounts(t, app, string(provider.Codex))
	app.sessionManager().put(threadID, session{
		Provider:             string(provider.Codex),
		Token:                "old-process",
		CredentialGeneration: 1,
		Liveness:             newSessionLiveness(time.Now()),
	})
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")

	err = app.ensureProviderAccountReadyForSendLocked(thread)
	if err == nil || !strings.Contains(err.Error(), "background work finishes") {
		t.Fatalf("error = %v, want background-work pending message", err)
	}
	if got, _ := app.sessionManager().get(threadID); got.Token != "old-process" || got.CredentialGeneration != 1 {
		t.Fatalf("the Codex session was modified: %+v", got)
	}
}

// The tray, the rest assessment and the background inventory read the same
// live work the session gates count: an agent's command is listed under the
// agent, a thread with one is waiting on background work, and the row
// carries the process id its Stop targets.
func TestAgentCommandReachesTheTrayRestAndInventory(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-readers")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")

	live, err := app.ListLiveBackgroundTasks(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "agent-cmd" || live[0].ParentID != "spawn-1" || !live[0].IsBackground {
		t.Fatalf("tray rows = %+v, want the agent's backgrounded command under spawn-1", live)
	}
	if kind, stopID := backgroundWorkHandle(string(provider.Codex), live[0]); kind != BackgroundWorkCodexTerminal || stopID != "1002" {
		t.Fatalf("inventory handle = %s/%s, want a terminal stopped by process 1002", kind, stopID)
	}
	if waits := app.memoryWaits(threadID); !waits.background {
		t.Fatal("rest assessment does not see the agent's command as background work")
	}
	if count, err := app.countRunningBackgroundTasks(threadID); err != nil || count != 1 {
		t.Fatalf("session gate count = %d (err %v), want 1", count, err)
	}
}

// terminalWire records what a fake app-server is asked to stop.
type terminalWire struct {
	mu         sync.Mutex
	cleaned    []string
	terminated []string
	// terminate answers a terminate in a thread.
	terminate func(providerThreadID string) bool
}

func (w *terminalWire) session(childThreadByLaunch map[string]string) *codex.Session {
	return codex.NewBackgroundTerminalsTestSession(codex.BackgroundTerminalsTestWire{
		RootThreadID:        "root-thread",
		ChildThreadByLaunch: childThreadByLaunch,
		Clean: func(_ context.Context, providerThreadID string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.cleaned = append(w.cleaned, providerThreadID)
			return nil
		},
		Terminate: func(_ context.Context, providerThreadID, processID string) (bool, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.terminated = append(w.terminated, providerThreadID+"/"+processID)
			return w.terminate(providerThreadID), nil
		},
	})
}

func (w *terminalWire) calls() (cleaned, terminated []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.cleaned), slices.Clone(w.terminated)
}

// A row's Stop reaches the agent's thread, whose process table holds the
// command. A Stop that finds nothing to stop, because the process exited
// or the agent's thread is gone, ends the command instead of leaving a row
// that can never be stopped. Each Stop touches only its own thread.
func TestTerminateCodexBackgroundTerminalStopsAnAgentCommandInItsThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-stop")
	otherID := createCodexAgentCommandThread(t, app, "thread-agent-command-other")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "gone-agent-cmd", "1003", "spawn-gone")
	seedCodexAgentCommand(t, app, otherID, "other-cmd", "1002", "spawn-1")

	running := true
	wire := &terminalWire{terminate: func(string) bool { return running }}
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Codex: wire.session(map[string]string{"spawn-1": "child-thread"})})

	terminated, err := app.TerminateCodexBackgroundTerminal(threadID, "1002")
	if err != nil || !terminated {
		t.Fatalf("stop of a running agent command = (%v, %v), want (true, nil)", terminated, err)
	}
	if _, got := wire.calls(); !slices.Equal(got, []string{"child-thread/1002"}) {
		t.Fatalf("terminate calls = %v, want the agent's thread", got)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 2 {
		t.Fatalf("live work after a stop Codex accepted = %d, want 2 until the command's item/completed", count)
	}

	running = false
	terminated, err = app.TerminateCodexBackgroundTerminal(threadID, "1002")
	if err != nil || terminated {
		t.Fatalf("stop of an exited agent command = (%v, %v), want (false, nil)", terminated, err)
	}
	terminated, err = app.TerminateCodexBackgroundTerminal(threadID, "1003")
	if err != nil || terminated {
		t.Fatalf("stop of a gone agent's command = (%v, %v), want (false, nil)", terminated, err)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after stops that found nothing = %d, want 0", count)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(otherID); count != 1 {
		t.Fatalf("another thread's live work = %d after this thread's stops, want 1", count)
	}
}

// Stop All cleans each Codex thread that runs a named terminal once: the
// root thread's and each agent's. A named terminal whose agent's thread is
// gone has ended, and leaves the tray.
func TestStopBackgroundTasksCleansEachThreadThatRunsANamedTerminal(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-stop-all")
	seedCodexAgentCommand(t, app, threadID, "root-cmd", "1001", "")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd-2", "1004", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "gone-agent-cmd", "1003", "spawn-gone")

	wire := &terminalWire{terminate: func(string) bool { return true }}
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Codex: wire.session(map[string]string{"spawn-1": "child-thread"})})

	results, err := app.StopBackgroundTasks(threadID, []string{"root-cmd", "agent-cmd", "agent-cmd-2", "gone-agent-cmd"})
	if err != nil {
		t.Fatal(err)
	}
	want := []BackgroundTaskStop{
		{LaunchItemID: "root-cmd", Outcome: BackgroundStopStopping},
		{LaunchItemID: "agent-cmd", Outcome: BackgroundStopStopping},
		{LaunchItemID: "agent-cmd-2", Outcome: BackgroundStopStopping},
		{LaunchItemID: "gone-agent-cmd", Outcome: BackgroundStopEnded},
	}
	if !slices.Equal(results, want) {
		t.Fatalf("results = %+v, want %+v", results, want)
	}
	if cleaned, _ := wire.calls(); !slices.Equal(cleaned, []string{"root-thread", "child-thread"}) {
		t.Fatalf("cleaned threads = %v, want the root thread and the agent's, once each", cleaned)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 3 {
		t.Fatalf("live work = %d, want the three stopping commands until their item/completed", count)
	}
}

// A revert and a thread delete clean every background terminal of the
// session, the agents' included: a cleanup of the root thread alone leaves
// an agent's command running behind a tray the revert then clears.
func TestCleanCodexBackgroundTerminalsReachesEveryThreadWithALiveCommand(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-clean")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "gone-agent-cmd", "1003", "spawn-gone")

	wire := &terminalWire{terminate: func(string) bool { return true }}
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Codex: wire.session(map[string]string{"spawn-1": "child-thread"})})

	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	// The revert's two steps around its provider rollback.
	if err := app.cleanRunningBackgroundTasksBeforeProviderRevert(thread, "revert"); err != nil {
		t.Fatalf("clean before revert: %v", err)
	}
	if cleaned, _ := wire.calls(); !slices.Equal(cleaned, []string{"root-thread", "child-thread"}) {
		t.Fatalf("cleaned threads = %v, want the root thread and the agent's", cleaned)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work = %d, want only the live agent's command (the gone agent's ended)", count)
	}
	if err := app.markConfirmedBackgroundTasksInactiveAfterProviderCleanup(threadID, "revert"); err != nil {
		t.Fatalf("mark inactive after cleanup: %v", err)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after the revert's cleanup = %d, want 0", count)
	}
}
