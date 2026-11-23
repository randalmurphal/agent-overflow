package app

import (
	"context"
	"encoding/json"
	"errors"
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
	startMeta, err := json.Marshal(map[string]any{
		"source": "unifiedExecStartup", "item_status": "inProgress", "process_id": processID,
		"toolName": "command_execution", "input": map[string]any{"command": "pnpm run dev"},
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
	mu         sync.Mutex
	cleaned    []string
	terminated []string
	listed     []string
	// terminate answers a terminate in a thread.
	terminate func(providerThreadID, processID string) (bool, error)
	// list answers the item ids a thread runs.
	list func(providerThreadID string) ([]string, error)
	// onClean runs after a clean is recorded, outside the lock.
	onClean func(providerThreadID string)
}

func (w *terminalWire) session() *codex.Session {
	return codex.NewBackgroundTerminalsTestSession(codex.BackgroundTerminalsTestWire{
		RootThreadID: rootCodexThread,
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
	app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: "tok", Codex: wire.session()})
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

// Codex's false is no proof the process is gone, so the stop asks the
// thread's list. A command the list still holds is still running and the
// stop fails naming it; a command the list no longer holds, the stopped
// one or another of that thread's, has ended and leaves the tray, and so
// does every command of a thread Codex no longer has loaded.
func TestCodexCommandStopChecksAFalseAnswerAgainstTheList(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-false")
	seedCodexAgentCommand(t, app, threadID, "stuck-cmd", "1001", "spawn-1")
	seedCodexAgentCommand(t, app, threadID, "exited-cmd", "1002", "spawn-1")
	seedCodexCommand(t, app, threadID, "unloaded-cmd", "1003", "spawn-2", codexScope("gone-thread"))
	wire := &terminalWire{
		terminate: func(providerThreadID, _ string) (bool, error) {
			if providerThreadID == "gone-thread" {
				return false, threadNotFound(providerThreadID)
			}
			return false, nil
		},
		list: func(string) ([]string, error) { return []string{"stuck-cmd"}, nil },
	}
	putCodexWire(app, threadID, wire)

	stopped, err := app.StopCodexBackgroundCommand(threadID, "stuck-cmd")
	if err == nil || stopped || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "pnpm run dev") {
		t.Fatalf("stop Codex did not carry out = (%v, %v), want an error naming the running command", stopped, err)
	}
	if _, ok := app.triage.LiveCodexCommand(threadID, "stuck-cmd"); !ok {
		t.Fatal("a command the list still holds left the tray")
	}
	if _, ok := app.triage.LiveCodexCommand(threadID, "exited-cmd"); ok {
		t.Fatal("a command of the listed thread that the list does not hold stayed live")
	}
	if stopped, err := app.StopCodexBackgroundCommand(threadID, "unloaded-cmd"); err != nil || stopped {
		t.Fatalf("stop in a thread Codex no longer has loaded = (%v, %v), want (false, nil)", stopped, err)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work = %d, want only the command Codex still runs", count)
	}
	if _, _, listed := wire.calls(); !slices.Equal(listed, []string{childCodexThread}) {
		t.Fatalf("lists = %v, want one for the false answer and none for the unloaded thread", listed)
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
	if cleaned, _, _ := wire.calls(); !slices.Equal(cleaned, []string{rootCodexThread, childCodexThread}) {
		t.Fatalf("cleaned threads = %v, want the root thread and the agent's", cleaned)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work after the clean = %d, want the command until it ends", count)
	}
}

// A revert truncates the turns the commands came from, so it waits until
// Codex has ended every one: a command leaves by its own item/completed or
// when its thread's list no longer holds it, and a command that started
// in a thread after that thread was cleaned gets a clean of its own.
func TestRevertWaitsForEveryCodexCommandToEnd(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-revert")
	seedCodexCommand(t, app, threadID, "root-cmd", "1001", "", codexScope(rootCodexThread))
	var once sync.Once
	wire := &terminalWire{
		list: func(providerThreadID string) ([]string, error) {
			if providerThreadID != rootCodexThread {
				return nil, nil
			}
			// The root command is still running when the root thread is
			// first listed; an agent starts a command meanwhile, and the
			// root command's own completion arrives after.
			once.Do(func() {
				seedCodexAgentCommand(t, app, threadID, "late-agent-cmd", "2001", "spawn-1")
				go func() {
					meta := json.RawMessage(`{"source":"unifiedExecStartup","item_status":"completed","toolName":"command_execution"}`)
					if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "root-cmd", ItemType: "commandExecution", ProcessScope: codexScope(rootCodexThread), Meta: meta, Timestamp: time.Now()}); err != nil {
						t.Errorf("root command completion: %v", err)
					}
				}()
			})
			if _, live := app.triage.LiveCodexCommand(threadID, "root-cmd"); live {
				return []string{"root-cmd"}, nil
			}
			return nil, nil
		},
	}
	putCodexWire(app, threadID, wire)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.stopCodexCommandsBeforeRevert(ctx, threadID); err != nil {
		t.Fatalf("revert's stop: %v", err)
	}
	if count := app.triage.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after the revert's stop = %d, want 0", count)
	}
	if cleaned, _, _ := wire.calls(); !slices.Equal(cleaned, []string{rootCodexThread, childCodexThread}) {
		t.Fatalf("cleaned threads = %v, want the root thread, then the agent's thread for its later command", cleaned)
	}
}

// A revert whose commands Codex still runs when its deadline passes fails
// before anything is truncated, and says which commands still run.
func TestRevertFailsNamingTheCodexCommandsStillRunning(t *testing.T) {
	t.Parallel()
	app := newTestAppWithTriage(t)
	threadID := createCodexAgentCommandThread(t, app, "thread-agent-command-revert-fail")
	seedCodexAgentCommand(t, app, threadID, "stuck-cmd", "1001", "spawn-1")
	wire := &terminalWire{list: func(string) ([]string, error) { return []string{"stuck-cmd"}, nil }}
	putCodexWire(app, threadID, wire)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := app.stopCodexCommandsBeforeRevert(ctx, threadID)
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "pnpm run dev") {
		t.Fatalf("revert's stop = %v, want a failure naming the running command", err)
	}
	if _, live := app.triage.LiveCodexCommand(threadID, "stuck-cmd"); !live {
		t.Fatal("a failed revert's stop ended the command Codex still runs")
	}
}

// A close item names an agent whose thread runs commands. The close does
// not say the thread is gone, so the app asks the thread's list: a close
// that succeeded removed the thread, which answers "thread not found", and
// its commands end; a close that failed left it loaded, its list still
// holds them, and they stay.
func TestCodexCloseItemEndsCommandsOnlyWhenTheThreadIsGone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		list     func(string) ([]string, error)
		wantLive bool
	}{
		{"close succeeded", func(threadID string) ([]string, error) { return nil, threadNotFound(threadID) }, false},
		{"close failed", func(string) ([]string, error) { return []string{"agent-cmd"}, nil }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.triage = app.newTriageRouter(app.store)
			threadID := createCodexAgentCommandThread(t, app, "thread-close-"+strings.ReplaceAll(tc.name, " ", "-"))
			seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1001", "spawn-1")
			wire := &terminalWire{list: tc.list}
			putCodexWire(app, threadID, wire)

			closeMeta, err := json.Marshal(map[string]any{"toolName": "close_agent", "item_status": "completed", "input": map[string]any{"tool": "close_agent", "receiverThreadIds": []string{childCodexThread}}})
			if err != nil {
				t.Fatal(err)
			}
			handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "close-turn"})
			handleCodexFrame(t, app, threadID, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "close-1", ItemType: "close_agent", TurnID: "close-turn", Meta: closeMeta})
			app.codexScopeChecks.Wait()

			if _, _, listed := wire.calls(); !slices.Equal(listed, []string{childCodexThread}) {
				t.Fatalf("lists = %v, want one of the closed agent's thread", listed)
			}
			if _, live := app.triage.LiveCodexCommand(threadID, "agent-cmd"); live != tc.wantLive {
				t.Fatalf("command live = %v after the check, want %v", live, tc.wantLive)
			}
		})
	}
}

// The hook only asks; when the check cannot run, the commands stay live.
func TestCodexCloseCheckFailureKeepsTheCommands(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = app.newTriageRouter(app.store)
	threadID := createCodexAgentCommandThread(t, app, "thread-close-check-fails")
	seedCodexAgentCommand(t, app, threadID, "agent-cmd", "1001", "spawn-1")
	putCodexWire(app, threadID, &terminalWire{list: func(string) ([]string, error) { return nil, errors.New("pipe closed") }})
	check, ok := app.triage.CodexScopeCheckFor(threadID, childCodexThread)
	if !ok {
		t.Fatal("no check for a thread with a live command")
	}
	app.runCodexScopeCheck(threadID, check)
	app.codexScopeChecks.Wait()
	if _, live := app.triage.LiveCodexCommand(threadID, "agent-cmd"); !live {
		t.Fatal("a check that failed ended the command")
	}
}
