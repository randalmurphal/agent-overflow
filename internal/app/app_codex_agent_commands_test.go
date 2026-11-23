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

// The Codex threads of the fake app-server in these tests: the session's
// root thread and one agent's.
const (
	rootCodexThread  = "root-thread"
	childCodexThread = "child-thread"
)

func codexScope(id string) provider.ProcessScope {
	return provider.ProcessScope{ID: id, Addressable: true}
}

// handleCodexFrame routes one provider frame of threadID, as the provider
// event worker does.
func handleCodexFrame(t *testing.T, app *App, threadID string, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID, evt.Timestamp = threadID, time.Now()
	if err := app.triage.Handle(evt); err != nil {
		t.Fatalf("%s: %v", evt.Kind, err)
	}
}

// seedCodexCommand leaves threadID with one running command, displayed under
// owner ("" for the root thread) and run in the process table scope names,
// backgrounded by a poll from that table, and no open turn: what a gate on
// ending the session sees once the main agent's turn is over.
func seedCodexCommand(t *testing.T, app *App, threadID, itemID, processID, owner string, scope provider.ProcessScope) {
	t.Helper()
	seedCodexCommandRunning(t, app, threadID, itemID, processID, owner, scope, "pnpm run dev")
}

// seedCodexCommandRunning is seedCodexCommand for a command that runs
// command.
func seedCodexCommandRunning(t *testing.T, app *App, threadID, itemID, processID, owner string, scope provider.ProcessScope, command string) {
	t.Helper()
	startMeta, err := json.Marshal(map[string]any{
		"source": "unifiedExecStartup", "item_status": "inProgress", "process_id": processID,
		"toolName": "command_execution", "input": map[string]any{"command": command},
	})
	if err != nil {
		t.Fatal(err)
	}
	turnID := threadID + ":" + itemID
	handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: turnID})
	handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: itemID, ItemType: "commandExecution", TurnID: turnID, ParentToolUseID: owner, ProcessScope: scope, Meta: startMeta})
	handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ParentToolUseID: owner, ProcessScope: scope, Meta: json.RawMessage(`{"process_id":"` + processID + `"}`)})
	handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnID: turnID, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
	if app.triage.HasInFlightTurnOrRound(threadID) || app.triage.HasPendingWork(threadID) {
		t.Fatal("precondition: the thread still has a turn or pending work")
	}
}

// seedCodexAgentCommand is seedCodexCommand for a command the agent of
// spawn launch owner runs in its own thread.
func seedCodexAgentCommand(t *testing.T, app *App, threadID, itemID, processID, owner string) {
	t.Helper()
	seedCodexCommand(t, app, threadID, itemID, processID, owner, codexScope(childCodexThread))
}

// completeCodexCommand is the command's own item/completed.
func completeCodexCommand(t *testing.T, app *App, threadID, itemID, owner string, scope provider.ProcessScope) {
	t.Helper()
	meta := json.RawMessage(`{"source":"unifiedExecStartup","item_status":"completed","toolName":"command_execution"}`)
	handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: owner, ProcessScope: scope, Meta: meta})
}

// unloadCodexAgent is the status Codex publishes when it shuts an agent's
// thread down.
func unloadCodexAgent(t *testing.T, app *App, threadID, owner, childThreadID string) {
	t.Helper()
	handleCodexFrame(t, app, threadID, provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ItemID: owner, ParentToolUseID: owner, ProcessScope: codexScope(childThreadID),
		Meta: json.RawMessage(`{"agent_path":"` + childThreadID + `","status":"notLoaded"}`),
	})
}

// createCodexAgentCommandThread creates a thread whose session triage
// admits, as a session start commits it.
func createCodexAgentCommandThread(t *testing.T, app *App, id string) string {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.triage.MarkThreadActive(thread.ID, "tok")
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

	unloadCodexAgent(t, app, threadID, "spawn-1", childCodexThread)
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
// agent, a thread with one is waiting on it, and the inventory stops it by
// item id.
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
	if kind, stopID := backgroundWorkHandle(string(provider.Codex), live[0]); kind != BackgroundWorkCodexCommand || stopID != "agent-cmd" {
		t.Fatalf("inventory handle = %s/%s, want a command stopped by its item id", kind, stopID)
	}
	if waits := app.memoryWaits(threadID); !waits.background || waits.backgroundDetail != live[0].Summary {
		t.Fatalf("rest assessment = %+v, want the agent's command %q as background work", waits, live[0].Summary)
	}
	if count, err := app.countRunningBackgroundTasks(threadID); err != nil || count != 1 {
		t.Fatalf("session gate count = %d (err %v), want 1", count, err)
	}
}

// terminalWire is a fake app-server's background terminals: it records
// what it is asked and answers from the processes each thread runs.
type terminalWire struct {
	mu          sync.Mutex
	cleaned     []string
	terminated  []string
	listed      []string
	interrupted []string
	// terminate answers a terminate in a thread.
	terminate func(providerThreadID, processID string) (bool, error)
	// list answers the item ids a thread runs.
	list func(providerThreadID string) ([]string, error)
	// onClean runs after a clean is recorded, outside the lock.
	onClean func(providerThreadID string)
	// onInterrupt runs after an agent's interrupt is recorded, outside the
	// lock, before Codex answers it.
	onInterrupt func(childThreadID string)
}

// session is the wire's session of threadID. Its agent threads are
// childCodexThread under spawn-1 and child-1 under spawn-live, and what it
// reports reaches app's router as the token session's frames.
func (w *terminalWire) session(app *App, threadID, token string) *codex.Session {
	return codex.NewBackgroundTerminalsTestSession(codex.BackgroundTerminalsTestWire{
		RootThreadID: rootCodexThread,
		Children:     map[string]string{childCodexThread: "spawn-1", "child-1": "spawn-live"},
		OnEvent: func(evt provider.ProviderEvent) {
			evt.ThreadID, evt.SessionToken = threadID, token
			if err := app.triage.Handle(evt); err != nil {
				panic(err)
			}
		},
		InterruptChild: func(_ context.Context, childThreadID, _ string) error {
			w.mu.Lock()
			w.interrupted = append(w.interrupted, childThreadID)
			onInterrupt := w.onInterrupt
			w.mu.Unlock()
			if onInterrupt != nil {
				onInterrupt(childThreadID)
			}
			return nil
		},
		Clean: func(_ context.Context, providerThreadID string) error {
			w.mu.Lock()
			w.cleaned = append(w.cleaned, providerThreadID)
			onClean := w.onClean
			w.mu.Unlock()
			if onClean != nil {
				onClean(providerThreadID)
			}
			return nil
		},
		Terminate: func(_ context.Context, providerThreadID, processID string) (bool, error) {
			w.mu.Lock()
			w.terminated = append(w.terminated, providerThreadID+"/"+processID)
			terminate := w.terminate
			w.mu.Unlock()
			if terminate == nil {
				return true, nil
			}
			return terminate(providerThreadID, processID)
		},
		List: func(_ context.Context, providerThreadID string) ([]string, error) {
			w.mu.Lock()
			w.listed = append(w.listed, providerThreadID)
			list := w.list
			w.mu.Unlock()
			if list == nil {
				return nil, nil
			}
			return list(providerThreadID)
		},
	})
}

func (w *terminalWire) calls() (cleaned, terminated, listed []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.cleaned), slices.Clone(w.terminated), slices.Clone(w.listed)
}

func putCodexWire(app *App, threadID string, wire *terminalWire) {
	putCodexWireAs(app, threadID, wire, "tok")
}

// putCodexWireAs makes wire's session the live session of threadID, as the
// session token names.
func putCodexWireAs(app *App, threadID string, wire *terminalWire, token string) {
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: token, Codex: wire.session(app, threadID, token)})
}

// threadNotFound is Codex's answer for a thread it has not loaded.
func threadNotFound(threadID string) error {
	return codex.TestRPCError(-32600, "thread not found: "+threadID)
}

// A row's Stop names the command by item id and reaches the process table
// that holds it, though the root thread and an agent's number their
// processes alike. A stop by process id, which only an older window sends,
// names only a root command: an id that no live root command holds sends
// nothing, whatever an agent's command or an ended command carried.
func TestCodexCommandStopReachesTheTableThatRunsIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-stop")
	seedCodexCommand(t, app, threadID, "root-cmd", "4242", "", codexScope(rootCodexThread))
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "4242", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "agent-only-cmd", "5555", "spawn-1")
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)

	for _, launch := range []string{"agent-cmd", "root-cmd"} {
		if stopped, err := app.StopCodexBackgroundCommand(threadID, launch); err != nil || !stopped {
			t.Fatalf("stop of %s = (%v, %v), want (true, nil)", launch, stopped, err)
		}
	}
	if _, got, _ := wire.calls(); !slices.Equal(got, []string{childCodexThread + "/4242", rootCodexThread + "/4242"}) {
		t.Fatalf("terminate calls = %v, want each command in its own thread", got)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 3 {
		t.Fatalf("live work after stops Codex accepted = %d, want 3 until each item/completed", count)
	}

	if stopped, err := app.TerminateCodexBackgroundTerminal(threadID, "4242"); err != nil || !stopped {
		t.Fatalf("older window's stop of process 4242 = (%v, %v), want the root command stopped", stopped, err)
	}
	completeCodexCommand(t, app, threadID, "root-cmd", "", codexScope(rootCodexThread))
	for _, processID := range []string{"4242", "5555", "9999"} {
		if stopped, err := app.TerminateCodexBackgroundTerminal(threadID, processID); err != nil || stopped {
			t.Fatalf("older window's stop of process %s with no live root command holding it = (%v, %v), want (false, nil)", processID, stopped, err)
		}
	}
	if _, got, _ := wire.calls(); !slices.Equal(got, []string{childCodexThread + "/4242", rootCodexThread + "/4242", rootCodexThread + "/4242"}) {
		t.Fatalf("terminate calls = %v, want nothing sent for an id no live root command holds", got)
	}
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "ended-cmd"); err != nil || stopped {
		t.Fatalf("stop of a command that is not live = (%v, %v), want (false, nil)", stopped, err)
	}
}

// Codex's false says it did not stop the process, and nothing else: an
// exited command's process has left the table, and a command that is
// starting is not in it yet, since Codex reports the start first. So the
// stop leaves every command live, the stopped one too, says Codex did not
// stop it, and asks nothing more. A thread Codex no longer has loaded
// answers "thread not found", which is the thread's unload: its commands
// end.
func TestCodexCommandStopFalseLeavesTheCommandsLive(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-false")
	seedCodexAgentCommand(t, app, threadID, "exited-cmd", "1001", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "starting-cmd", "1002", "spawn-1")
	wire := &terminalWire{
		terminate: func(providerThreadID, _ string) (bool, error) {
			if providerThreadID == "child-1" {
				return false, threadNotFound(providerThreadID)
			}
			return false, nil
		},
		// Neither is in the table: one left it, one is not in it yet.
		list: func(string) ([]string, error) { return nil, nil },
	}
	putCodexWire(app, threadID, wire)

	stopped, err := app.StopCodexBackgroundCommand(threadID, "exited-cmd")
	if err == nil || stopped || !strings.Contains(err.Error(), "Codex did not stop it") || !strings.Contains(err.Error(), "pnpm run dev") {
		t.Fatalf("stop Codex answered false = (%v, %v), want an error naming the command Codex did not stop", stopped, err)
	}
	for _, id := range []string{"exited-cmd", "starting-cmd"} {
		if _, ok := app.triage.LiveCodexCommand(threadID, id); !ok {
			t.Fatalf("%s left the live work on Codex's false", id)
		}
	}
	if _, _, listed := wire.calls(); len(listed) != 0 {
		t.Fatalf("lists = %v, want none: a list is no evidence", listed)
	}

	seedCodexCommand(t, app, threadID, "unloaded-cmd", "1003", "spawn-live", codexScope("child-1"))
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "unloaded-cmd"); err != nil || stopped {
		t.Fatalf("stop in a thread Codex no longer has loaded = (%v, %v), want (false, nil)", stopped, err)
	}
	if _, ok := app.triage.LiveCodexCommand(threadID, "unloaded-cmd"); ok {
		t.Fatal("a command of a thread Codex answered not found stayed live")
	}
}

// A stop goes to the session triage read the command in. A replacement
// session committed after the read gets nothing: its process table is
// another table, and the same process id there names another process.
func TestCodexCommandStopGoesOnlyToTheSessionThatRunsIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-replaced")
	seedCodexCommand(t, app, threadID, "root-cmd", "1001", "", codexScope(rootCodexThread))
	putCodexWire(app, threadID, &terminalWire{})
	command, ok := app.triage.LiveCodexCommand(threadID, "root-cmd")
	if !ok {
		t.Fatal("no live command to stop")
	}

	app.triage.MarkThreadActive(threadID, "tok-2")
	replacement := &terminalWire{}
	putCodexWireAs(app, threadID, replacement, "tok-2")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if stopped, err := app.stopCodexCommand(ctx, threadID, command); err == nil || stopped {
		t.Fatalf("stop after the session was replaced = (%v, %v), want a refusal", stopped, err)
	}
	if _, terminated, _ := replacement.calls(); len(terminated) != 0 {
		t.Fatalf("the replacement session was asked to terminate %v", terminated)
	}
}

// Codex gives an exited process's id to the next process it starts, which
// can be before the exited command's completion arrives. A stop of the
// exited command is refused rather than sent: it would terminate the newer
// command's process.
func TestCodexCommandStopRefusesAProcessIDANewerCommandHolds(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-reused")
	seedCodexAgentCommand(t, app, threadID, "old-cmd", "1001", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "new-cmd", "1001", "spawn-1")
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "old-cmd"); err == nil || stopped || !strings.Contains(err.Error(), "another command") {
		t.Fatalf("stop of the command whose process id was reused = (%v, %v), want a refusal", stopped, err)
	}
	if _, terminated, _ := wire.calls(); len(terminated) != 0 {
		t.Fatalf("terminate calls = %v, want none: process 1001 is the newer command's", terminated)
	}
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "new-cmd"); err != nil || !stopped {
		t.Fatalf("stop of the newer command = (%v, %v), want (true, nil)", stopped, err)
	}
}

// A command inside a code review runs in the review delegate's table,
// which no request reaches, so its Stop says it ends with the review and
// sends nothing.
func TestCodexReviewCommandStopIsRefused(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-review-command-stop")
	seedCodexCommand(t, app, threadID, "review-cmd", "1001", "review-1", provider.ProcessScope{ID: "codex-review-run:review-1"})
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "review-cmd"); err == nil || stopped || !strings.Contains(err.Error(), "ends with the review") {
		t.Fatalf("stop of a review command = (%v, %v), want a refusal", stopped, err)
	}
	if cleaned, terminated, listed := wire.calls(); len(cleaned)+len(terminated)+len(listed) != 0 {
		t.Fatalf("calls = %v %v %v, want none", cleaned, terminated, listed)
	}
}

// A command whose start named no process yet has nothing a terminate can
// name, so its Stop says so and sends nothing.
func TestCodexCommandStopBeforeItsProcessIsNamedIsRefused(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-unnamed-command-stop")
	seedCodexAgentCommand(t, app, threadID, "unnamed-cmd", "", "spawn-1")
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "unnamed-cmd"); err == nil || stopped || !strings.Contains(err.Error(), "has not named its process") {
		t.Fatalf("stop of a command with no process = (%v, %v), want a refusal", stopped, err)
	}
	if cleaned, terminated, listed := wire.calls(); len(cleaned)+len(terminated)+len(listed) != 0 {
		t.Fatalf("calls = %v %v %v, want none", cleaned, terminated, listed)
	}
}

// Stop All stops each named command in its own table and nothing else: a
// command it does not name keeps running, so there is no thread-wide clean.
func TestStopBackgroundTasksStopsOnlyTheNamedCommands(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-stop-all")
	seedCodexCommand(t, app, threadID, "root-cmd", "1001", "", codexScope(rootCodexThread))
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "unnamed-cmd", "1003", "spawn-1")
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)

	results, err := app.StopBackgroundTasks(threadID, []string{"root-cmd", "agent-cmd", "ended-cmd"})
	if err != nil {
		t.Fatal(err)
	}
	want := []BackgroundTaskStop{
		{LaunchItemID: "root-cmd", Outcome: BackgroundStopStopping},
		{LaunchItemID: "agent-cmd", Outcome: BackgroundStopStopping},
		{LaunchItemID: "ended-cmd", Outcome: BackgroundStopEnded},
	}
	if !slices.Equal(results, want) {
		t.Fatalf("results = %+v, want %+v", results, want)
	}
	cleaned, terminated, _ := wire.calls()
	slices.Sort(terminated)
	if len(cleaned) != 0 || !slices.Equal(terminated, []string{childCodexThread + "/1002", rootCodexThread + "/1001"}) {
		t.Fatalf("cleaned %v, terminated %v, want only the two named commands terminated", cleaned, terminated)
	}
}

// A thread delete cleans every Codex thread of the session that runs a
// command, the root thread always. A clean's answer only acknowledges the
// request, so the commands stay live until each leaves by its own way.
func TestCleanCodexBackgroundTerminalsReachesEveryThreadAndEndsNothing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-clean")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1002", "spawn-1")
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)
	if err := app.CleanCodexBackgroundTerminals(threadID); err != nil {
		t.Fatal(err)
	}
	cleaned, _, _ := wire.calls()
	slices.Sort(cleaned)
	if !slices.Equal(cleaned, []string{childCodexThread, rootCodexThread}) {
		t.Fatalf("cleaned threads = %v, want the root thread and the agent's", cleaned)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work after the clean = %d, want the command until it ends", count)
	}
}

// revertCodexCommands runs a revert's stop of the thread's Codex commands on
// its live session.
func revertCodexCommands(t *testing.T, app *App, threadID string, within time.Duration) error {
	t.Helper()
	live, ok := app.activeCodexSession(threadID)
	if !ok {
		t.Fatal("no live Codex session to revert on")
	}
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return app.stopCodexCommandsBeforeRevert(ctx, threadID, live)
}

// A revert truncates the turns the commands came from, so it waits until
// Codex has ended every one: a command leaves by its own item/completed or
// by its thread's unload, and a command that started in a thread after
// that thread was cleaned gets a clean of its own.
func TestRevertWaitsForEveryCodexCommandToEnd(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-revert")
	seedCodexCommand(t, app, threadID, "root-cmd", "1001", "", codexScope(rootCodexThread))
	var once sync.Once
	wire := &terminalWire{
		onClean: func(providerThreadID string) {
			switch providerThreadID {
			case rootCodexThread:
				// An agent starts a command while the root thread is cleaned,
				// and the root command's completion arrives after.
				once.Do(func() {
					seedCodexAgentCommand(t, app, threadID, "late-agent-cmd", "2001", "spawn-1")
					go completeCodexCommand(t, app, threadID, "root-cmd", "", codexScope(rootCodexThread))
				})
			case childCodexThread:
				completeCodexCommand(t, app, threadID, "late-agent-cmd", "spawn-1", codexScope(childCodexThread))
			}
		},
	}
	putCodexWire(app, threadID, wire)

	if err := revertCodexCommands(t, app, threadID, 5*time.Second); err != nil {
		t.Fatalf("revert's stop: %v", err)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after the revert's stop = %d, want 0", count)
	}
	if cleaned, _, listed := wire.calls(); !slices.Equal(cleaned, []string{rootCodexThread, childCodexThread}) || len(listed) != 0 {
		t.Fatalf("cleaned threads = %v, lists %v; want the root thread, then the agent's thread for its later command, and no list", cleaned, listed)
	}
}

// A consented revert keeps the session, and with it the thread's agents,
// which could start a command after any check. It interrupts each active
// agent first, and handles the events read before Codex answered the
// interrupt, so the command an agent started just before its turn ended is
// counted and cleaned.
func TestRevertStopsTheAgentsAndCountsTheirLastCommand(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-revert-agents")
	seedCodexSubagentLaunchRow(t, app, threadID, "spawn-live", time.Now().UnixMilli())
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	startMeta, err := json.Marshal(map[string]any{
		"source": "unifiedExecStartup", "item_status": "inProgress", "process_id": "3001",
		"toolName": "command_execution", "input": map[string]any{"command": "pnpm run dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := &terminalWire{
		// The agent's command start was read before the interrupt's answer
		// and still waits in the thread's event queue.
		onInterrupt: func(string) {
			start := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: "last-agent-cmd", ItemType: "commandExecution", ParentToolUseID: "spawn-live", ProcessScope: codexScope("child-1"), Meta: startMeta, SessionToken: "tok", Timestamp: time.Now()}
			app.providerEvents.enqueue(threadID, start, func(evt provider.ProviderEvent) {
				<-release
				if err := app.triage.Handle(evt); err != nil {
					t.Errorf("agent command start: %v", err)
				}
			})
		},
		onClean: func(providerThreadID string) {
			if providerThreadID == "child-1" {
				completeCodexCommand(t, app, threadID, "last-agent-cmd", "spawn-live", codexScope("child-1"))
			}
		},
	}
	putCodexWire(app, threadID, wire)
	go func() {
		waitUntil(t, 5*time.Second, func() bool { return providerEventDrainWaiting(app, threadID) })
		releaseOnce.Do(func() { close(release) })
	}()

	if err := revertCodexCommands(t, app, threadID, 5*time.Second); err != nil {
		t.Fatalf("revert's stop: %v", err)
	}
	wire.mu.Lock()
	interrupted := slices.Clone(wire.interrupted)
	wire.mu.Unlock()
	if !slices.Equal(interrupted, []string{"child-1"}) {
		t.Fatalf("interrupted agents = %v, want the active agent's thread", interrupted)
	}
	cleaned, _, _ := wire.calls()
	slices.Sort(cleaned)
	if !slices.Equal(cleaned, []string{"child-1", rootCodexThread}) {
		t.Fatalf("cleaned threads = %v, want the root thread and the agent's, which ran the last command", cleaned)
	}
	if _, live := app.triage.LiveCodexCommand(threadID, "last-agent-cmd"); live {
		t.Fatal("the revert returned with the agent's last command still live")
	}
}

// providerEventDrainWaiting reports whether a caller waits for threadID's
// queued provider events to be handled.
func providerEventDrainWaiting(app *App, threadID string) bool {
	q := app.providerEvents.lookup(threadID)
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.changed != nil
}

// A revert whose commands Codex still runs when its deadline passes fails
// before anything is truncated, and says which commands still run.
func TestRevertFailsNamingTheCodexCommandsStillRunning(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-revert-fail")
	seedCodexAgentCommand(t, app, threadID, "stuck-cmd", "1001", "spawn-1")
	wire := &terminalWire{}
	putCodexWire(app, threadID, wire)

	err := revertCodexCommands(t, app, threadID, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "pnpm run dev") {
		t.Fatalf("revert's stop = %v, want a failure naming the running command", err)
	}
	if _, live := app.triage.LiveCodexCommand(threadID, "stuck-cmd"); !live {
		t.Fatal("a failed revert's stop ended the command Codex still runs")
	}
}
