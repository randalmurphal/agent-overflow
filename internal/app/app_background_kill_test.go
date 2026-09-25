package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// A Claude interrupt kills every running or parked async agent and the
// shells each one owns (claude-wire.md §Background task ownership). These
// tests drive agents through triage the way the parser delivers them and
// stop the thread through a fake CLI that records every interrupt it acks.

type agentKillFixture struct {
	app          *App
	thread       store.Thread
	interruptLog string
	// dbPath is the store's file, for a second handle that injects faults.
	dbPath string
}

func newAgentKillFixture(t *testing.T, providerName, model string) *agentKillFixture {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread, err := createTestThread(t, app, providerName, t.TempDir(), model, "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	f := &agentKillFixture{app: app, thread: thread, interruptLog: filepath.Join(t.TempDir(), "interrupts.log"), dbPath: dbPath}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: thread.ID + ":turn"}, nil)
	// A Claude session stands in for every provider: the gate must decide
	// by the thread, and the recorder is what proves an interrupt was sent.
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeInterruptRecorderBinary(t, f.interruptLog), WorkDir: t.TempDir()},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "agent-kill", Claude: sess})
	return f
}

func newClaudeAgentKillFixture(t *testing.T) *agentKillFixture {
	t.Helper()
	return newAgentKillFixture(t, string(provider.Claude), "claude-sonnet-4-6")
}

// writeClaudeInterruptRecorderBinary acks every interrupt control_request
// and appends it to logPath, so a test can count the interrupts sent.
func writeClaudeInterruptRecorderBinary(t *testing.T, logPath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"subtype":"interrupt"'*)
            printf '%%s\n' "$line" >> %s
            reqid=$(printf '%%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`, shellQuote(logPath))
	path := filepath.Join(t.TempDir(), "claude-interrupt-recorder.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write interrupt recorder: %v", err)
	}
	return path
}

func (f *agentKillFixture) interrupts(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.interruptLog)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read interrupt log: %v", err)
	}
	return strings.Count(string(data), "\n")
}

func (f *agentKillFixture) handle(t *testing.T, evt provider.ProviderEvent, fields map[string]any) {
	t.Helper()
	if fields != nil {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal meta: %v", err)
		}
		evt.Meta = raw
	}
	evt.ThreadID, evt.Timestamp = f.thread.ID, time.Now()
	if err := f.app.triage.Handle(evt); err != nil {
		t.Fatalf("handle %s %s: %v", evt.Kind, evt.ItemID, err)
	}
}

// launchAgent is an async Agent launch: the tool_use, its task_started,
// and the §E5 ack that backgrounds it.
func (f *agentKillFixture) launchAgent(t *testing.T, id, taskID, parentID string) {
	t.Helper()
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id, ItemType: "Agent", ParentToolUseID: parentID},
		map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Agent " + id, "prompt": "do the thing"}})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id, ParentToolUseID: parentID},
		map[string]any{"task_id": taskID, "task_type": "local_agent"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: id, ParentToolUseID: parentID, Content: "Async agent launched successfully."},
		map[string]any{"is_background": true})
}

// launchShell is a backgrounded Bash, owned by parentID's agent or, with no
// parent, by the main thread.
func (f *agentKillFixture) launchShell(t *testing.T, id, taskID, parentID string) {
	t.Helper()
	started := map[string]any{"task_id": taskID, "task_type": "local_bash"}
	if parentID != "" {
		started["parent_tool_use_id"] = parentID
	}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id, ParentToolUseID: parentID}, started)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: id, ItemType: "Bash", ParentToolUseID: parentID},
		map[string]any{"toolName": "Bash", "is_background": true, "input": map[string]any{"command": "sleep 120", "run_in_background": true}})
}

// stopAgent is one agent stop: the terminal and the notification the CLI
// sends at every stop. With a live owned shell the agent parks.
func (f *agentKillFixture) stopAgent(t *testing.T, boundID, taskID string) {
	t.Helper()
	f.handle(t, provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: boundID},
		map[string]any{"task_id": taskID, "tool_use_id": boundID, "status": "completed", "source": "task_updated"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventBackgroundTaskNotification, ItemID: boundID, Content: "WAITING"},
		map[string]any{"task_id": taskID, "tool_use_id": boundID, "status": "completed", "uuid": "u-" + boundID})
}

func (f *agentKillFixture) itemCount(t *testing.T) int {
	t.Helper()
	items, err := f.app.store.ListItems(f.thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	return len(items)
}

// requireRefusal asserts err is the public background_agents_running
// refusal and returns the agents it names.
func requireRefusal(t *testing.T, err error) []BackgroundKillAgent {
	t.Helper()
	var refusal *backgroundKillRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v, want a background kill refusal", err)
	}
	if code, _, ok := errorsx.PublicDetails(err); !ok || code != transport.ErrCodeBackgroundAgentsRunning {
		t.Fatalf("public code = %q (public=%v), want %s", code, ok, transport.ErrCodeBackgroundAgentsRunning)
	}
	var wire []BackgroundKillAgent
	if err := json.Unmarshal(refusal.RefusedBackgroundAgents(), &wire); err != nil {
		t.Fatalf("decode refusal payload: %v", err)
	}
	if fmt.Sprint(wire) != fmt.Sprint(refusal.Agents) {
		t.Fatalf("wire payload %+v, want %+v", wire, refusal.Agents)
	}
	return refusal.Agents
}

func agentIDs(agents []BackgroundKillAgent) []string {
	ids := make([]string, 0, len(agents))
	for _, agent := range agents {
		ids = append(ids, agent.LaunchItemID+"="+agent.RunState)
	}
	return ids
}

// The list names every agent an interrupt kills and nothing else: running
// and parked agents at any depth, a resumed round by its carrier opening
// the original launch's pane; not finished agents, not shells, not a
// foreground agent.
func TestRunningBackgroundAgents_ListsTheAgentsAnInterruptKills(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "running", "task-running", "")
	f.launchAgent(t, "parked", "task-parked", "")
	f.launchShell(t, "parked-shell", "task-parked-shell", "parked")
	f.stopAgent(t, "parked", "task-parked")
	f.launchAgent(t, "finished", "task-finished", "")
	f.stopAgent(t, "finished", "task-finished")
	// A background agent launched by a foreground one: nested, and not
	// counted by the top-level background gates.
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "foreground", ItemType: "Agent"},
		map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Agent foreground", "prompt": "delegate"}})
	f.launchAgent(t, "nested", "task-nested", "foreground")
	f.launchShell(t, "main-shell", "task-main-shell", "")
	// A resumed agent: its first round ended, the carrier runs the second.
	f.launchAgent(t, "root", "task-root", "")
	f.stopAgent(t, "root", "task-root")
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "carrier", ItemType: "SendMessage"},
		map[string]any{"toolName": "SendMessage", "input": map[string]any{"to": "task-root", "message": "continue"}})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "carrier"},
		map[string]any{"task_id": "task-root", "task_type": "local_agent", "resumes_tool_use_id": "root",
			"description": "Agent root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "carrier", Content: "Resuming agent"},
		map[string]any{"is_background": true})

	agents, err := f.app.RunningBackgroundAgents(f.thread.ID)
	if err != nil {
		t.Fatalf("RunningBackgroundAgents: %v", err)
	}
	byID := map[string]BackgroundKillAgent{}
	for _, agent := range agents {
		byID[agent.LaunchItemID] = agent
	}
	want := map[string]BackgroundKillAgent{
		"running": {LaunchItemID: "running", Description: "Agent running", RunState: "running", TranscriptRootID: "running"},
		"parked":  {LaunchItemID: "parked", Description: "Agent parked", RunState: "parked", TranscriptRootID: "parked"},
		"nested":  {LaunchItemID: "nested", Description: "Agent nested", RunState: "running", TranscriptRootID: "nested"},
		"carrier": {LaunchItemID: "carrier", Description: "Agent root", RunState: "running", TranscriptRootID: "root"},
	}
	if len(byID) != len(want) {
		t.Fatalf("agents = %v, want %d agents", agentIDs(agents), len(want))
	}
	for id, expected := range want {
		if got := byID[id]; got != expected {
			t.Errorf("agent %s = %+v, want %+v", id, got, expected)
		}
	}
}

// Refused: nothing is cancelled, interrupted or written. Confirmed: the
// same call stops the turn as it always did.
func TestInterruptTurn_RefusesWhileAgentsLiveAndProceedsOnConfirm(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	remoteWait, endRemote := f.app.beginRemoteWait(context.Background(), f.thread.ID, "computer", "job")
	defer endRemote()
	requestWait, _, endRequest := f.app.beginRequestWait(context.Background(), f.thread.ID, []string{"token"})
	defer endRequest()
	itemsBefore := f.itemCount(t)
	openTurn := f.app.triage.OpenTurnIndex(f.thread.ID)

	agents := requireRefusal(t, f.app.InterruptTurn(f.thread.ID, nil))
	if got := agentIDs(agents); len(got) != 1 || got[0] != "agent=running" {
		t.Fatalf("refusal agents = %v, want the running agent", got)
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("a refused stop sent %d interrupts", n)
	}
	if remoteWait.Err() != nil || requestWait.Err() != nil {
		t.Fatalf("a refused stop cancelled parked calls: remote=%v request=%v", remoteWait.Err(), requestWait.Err())
	}
	if got := f.itemCount(t); got != itemsBefore {
		t.Fatalf("a refused stop wrote rows: %d items, want %d", got, itemsBefore)
	}
	if got := f.app.triage.OpenTurnIndex(f.thread.ID); got != openTurn {
		t.Fatalf("a refused stop closed the turn: open turn %d, want %d", got, openTurn)
	}

	if err := f.app.InterruptTurn(f.thread.ID, []string{"agent"}); err != nil {
		t.Fatalf("confirmed InterruptTurn: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("confirmed stop sent %d interrupts, want 1", n)
	}
	if remoteWait.Err() == nil || requestWait.Err() == nil {
		t.Fatal("a confirmed stop must end the thread's parked calls")
	}
}

// Background work that is not an agent survives the interrupt, so it asks
// nothing.
func TestInterruptTurn_MainThreadShellDoesNotRefuse(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchShell(t, "main-shell", "task-main-shell", "")
	if err := f.app.InterruptTurn(f.thread.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}

// A parked agent dies with its shells on an interrupt and never wakes
// (spike D, 2.1.280), so it is refused like a running one.
func TestInterruptTurn_RefusesForAParkedAgent(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	f.launchShell(t, "shell", "task-shell", "agent")
	f.stopAgent(t, "agent", "task-agent")

	agents := requireRefusal(t, f.app.InterruptTurn(f.thread.ID, nil))
	if got := agentIDs(agents); len(got) != 1 || got[0] != "agent=parked" {
		t.Fatalf("refusal agents = %v, want the parked agent", got)
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("a refused stop sent %d interrupts", n)
	}
}

// A background agent under a foreground agent dies with the interrupt, so
// the interrupt refuses for it, and the stop count a revert confirms
// counts it too.
func TestInterruptTurn_RefusesForANestedAgent(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "foreground", ItemType: "Agent"},
		map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Agent foreground", "prompt": "delegate"}})
	f.launchAgent(t, "nested", "task-nested", "foreground")
	if count, err := f.app.countRunningBackgroundTasks(f.thread.ID); err != nil || count != 1 {
		t.Fatalf("running background tasks = %d (%v), want the nested agent", count, err)
	}

	agents := requireRefusal(t, f.app.InterruptTurn(f.thread.ID, nil))
	if got := agentIDs(agents); len(got) != 1 || got[0] != "nested=running" {
		t.Fatalf("refusal agents = %v, want the nested agent", got)
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("a refused stop sent %d interrupts", n)
	}
}

// A Codex thread's interrupt leaves its agents running, so it never asks,
// even over rows shaped like a Claude agent.
func TestInterruptTurn_CodexThreadNeverRefuses(t *testing.T) {
	f := newAgentKillFixture(t, string(provider.Codex), "gpt-5.4")
	f.launchAgent(t, "agent", "task-agent", "")

	agents, err := f.app.RunningBackgroundAgents(f.thread.ID)
	if err != nil || len(agents) != 0 {
		t.Fatalf("RunningBackgroundAgents = %v (%v), want none on a Codex thread", agents, err)
	}
	if err := f.app.InterruptTurn(f.thread.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}

// An agent that launches after the first check, while the stop waits for
// the thread lock, is refused by the check under the lock before any
// interrupt. The parked-call cancels between the two checks have already
// run: they return backgrounded receipts and stop no work.
func TestInterruptTurn_AgentLaunchedWhileWaitingForTheLockIsRefused(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	remoteWait, endRemote := f.app.beginRemoteWait(context.Background(), f.thread.ID, "computer", "job")
	defer endRemote()

	unlock := f.app.threadLocks().Lock(f.thread.ID)
	done := make(chan error, 1)
	go func() { done <- f.app.InterruptTurn(f.thread.ID, nil) }()
	// The parked-call cancel runs after the first check and before the lock.
	waitUntil(t, 5*time.Second, func() bool { return remoteWait.Err() != nil })

	f.launchAgent(t, "late", "task-late", "")
	unlock()

	select {
	case err := <-done:
		agents := requireRefusal(t, err)
		if got := agentIDs(agents); len(got) != 1 || got[0] != "late=running" {
			t.Fatalf("refusal agents = %v, want the late agent", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("InterruptTurn did not return after the lock was released")
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("the late agent was killed: %d interrupts sent", n)
	}
}

// The un-send refuses before its own "running background tasks" decline,
// and nothing is interrupted, reverted or written. Confirmed, it declines
// the revert for that background work and stops the turn.
func TestInterruptAndRevertIfClean_RefusesWhileAgentsLiveAndProceedsOnConfirm(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	// The newest turn is an unanswered message, so only the background
	// work keeps it from being un-sent.
	insertUserItem(t, f.app.store, f.thread.ID, "u:1", 1, "never mind")
	itemsBefore := f.itemCount(t)

	result, err := f.app.InterruptAndRevertIfClean(f.thread.ID, InterruptRevertOptions{}, nil)
	agents := requireRefusal(t, err)
	if got := agentIDs(agents); len(got) != 1 || got[0] != "agent=running" {
		t.Fatalf("refusal agents = %v, want the running agent", got)
	}
	if result.Reverted || result.Reason != "" {
		t.Fatalf("refused result = %+v, want empty", result)
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("a refused un-send sent %d interrupts", n)
	}
	if got := f.itemCount(t); got != itemsBefore {
		t.Fatalf("a refused un-send changed rows: %d items, want %d", got, itemsBefore)
	}
	if _, ok := f.app.sessionManager().get(f.thread.ID); !ok {
		t.Fatal("a refused un-send stopped the session")
	}

	result, err = f.app.InterruptAndRevertIfClean(f.thread.ID, InterruptRevertOptions{}, []string{"agent"})
	if err != nil {
		t.Fatalf("confirmed InterruptAndRevertIfClean: %v", err)
	}
	if result.Reverted || result.Reason != "running background tasks" {
		t.Fatalf("confirmed result = %+v, want the plain-interrupt decline", result)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("confirmed un-send sent %d interrupts, want 1", n)
	}
	if _, found, err := f.app.store.GetThreadItem(f.thread.ID, "u:1"); err != nil || !found {
		t.Fatalf("the message must stay: found=%v err=%v", found, err)
	}
}

// With only a main-thread shell the un-send takes its existing decline to
// the plain interrupt without asking.
func TestInterruptAndRevertIfClean_MainThreadShellKeepsTheExistingDecline(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchShell(t, "main-shell", "task-main-shell", "")
	insertUserItem(t, f.app.store, f.thread.ID, "u:1", 1, "never mind")

	result, err := f.app.InterruptAndRevertIfClean(f.thread.ID, InterruptRevertOptions{}, nil)
	if err != nil {
		t.Fatalf("InterruptAndRevertIfClean: %v", err)
	}
	if result.Reverted || result.Reason != "running background tasks" {
		t.Fatalf("result = %+v, want the plain-interrupt decline", result)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}

// Agent requests, their cancels and workflow takeovers are not a person's
// Stop: they interrupt with agents live.
func TestUngatedInterruptsStopWithAgentsLive(t *testing.T) {
	cases := map[string]func(*agentKillFixture) error{
		// interruptTurnCtx is what the workflow runner is constructed with.
		"workflow takeover": func(f *agentKillFixture) error {
			return f.app.interruptTurnCtx(context.Background(), f.thread.ID)
		},
		"thread tools cancel": func(f *agentKillFixture) error {
			_, err := f.app.threadTools().interruptRunningThread(context.Background(), f.thread.ID)
			return err
		},
		"request lifecycle cancel": func(f *agentKillFixture) error {
			_, err := f.app.interruptRequestTurn(context.Background(), f.thread.ID, f.app.triage.OpenTurnIndex(f.thread.ID))
			return err
		},
	}
	for name, interrupt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newClaudeAgentKillFixture(t)
			f.launchAgent(t, "agent", "task-agent", "")
			if err := interrupt(f); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			if n := f.interrupts(t); n != 1 {
				t.Fatalf("interrupts = %d, want 1", n)
			}
		})
	}
}

// Spike D's wire after the interrupt: the parked agent, which already sent
// its completed terminal at the park, gets task_updated{killed} with no
// notification, and its shell gets killed plus a stopped notification. The
// agent settles killed, its stash goes, and it leaves the kill list and the
// tray.
func TestInterruptKillOfAParkedAgentSettlesItAndItsShell(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	f.launchShell(t, "shell", "task-shell", "agent")
	f.stopAgent(t, "agent", "task-agent")

	f.handle(t, provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: "agent"},
		map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "killed", "is_error": true, "source": "task_updated", "end_time": time.Now().UnixMilli()})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: "shell", ParentToolUseID: "agent"},
		map[string]any{"task_id": "task-shell", "tool_use_id": "shell", "parent_tool_use_id": "agent", "status": "killed", "is_error": true, "source": "task_updated", "end_time": time.Now().UnixMilli()})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventBackgroundTaskNotification, ItemID: "shell", ParentToolUseID: "agent", Content: `Background command "sleep 120" was stopped`},
		map[string]any{"task_id": "task-shell", "tool_use_id": "shell", "parent_tool_use_id": "agent", "status": "stopped", "uuid": "u-shell"})

	if _, stashed, err := f.app.store.GetPendingBackgroundTerminal(f.thread.ID, "task-agent"); err != nil || stashed {
		t.Fatalf("the kill must consume the park's stash: stashed=%v err=%v", stashed, err)
	}
	tasks, err := f.app.ListLiveBackgroundTasks(f.thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks: %v", err)
	}
	completions := map[string]store.Item{}
	for _, task := range tasks {
		if task.CompletionOf != "" {
			completions[task.CompletionOf] = task
		}
		if task.ID == "agent" {
			if state := store.AgentRunState(task); state != "done" {
				t.Errorf("tray run state = %q, want done", state)
			}
		}
	}
	for _, id := range []string{"agent", "shell"} {
		if completions[id].Status != "killed" {
			t.Errorf("%s completion = %+v, want killed", id, completions[id])
		}
	}
	agents, err := f.app.RunningBackgroundAgents(f.thread.ID)
	if err != nil || len(agents) != 0 {
		t.Fatalf("RunningBackgroundAgents = %v (%v), want none after the kill", agentIDs(agents), err)
	}
	// Past the tray's retention window the settled pair is gone.
	aged, err := f.app.store.ListLiveBackgroundTasks(f.thread.ID, time.Now().Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatalf("store ListLiveBackgroundTasks: %v", err)
	}
	if len(aged) != 0 {
		t.Fatalf("aged tray = %d rows, want none", len(aged))
	}
}

// Both Stop RPCs send the refusal through the real dispatcher as the
// background_agents_running frame naming the agents, on every origin,
// whether the caller confirmed nothing (an empty list or null) or another
// agent.
func TestStopRefusalWireFrameNamesTheAgents(t *testing.T) {
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(f.app, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
		t.Fatal(err)
	}
	type call struct {
		name string
		args []any
	}
	var calls []call
	for _, confirmed := range [][]string{nil, {}, {"another-agent"}} {
		calls = append(calls,
			call{"InterruptTurn", []any{f.thread.ID, confirmed}},
			call{"InterruptAndRevertIfClean", []any{f.thread.ID, InterruptRevertOptions{}, confirmed}})
	}
	for _, c := range calls {
		name, args := c.name, c.args
		method, ok := dispatcher.LookupName(name)
		if !ok {
			t.Fatalf("missing method %s", name)
		}
		params := make([]json.RawMessage, len(args))
		for i, arg := range args {
			raw, err := json.Marshal(arg)
			if err != nil {
				t.Fatal(err)
			}
			params[i] = raw
		}
		for _, loopback := range []bool{false, true} {
			_, frame := dispatcher.InvokeForOrigin(context.Background(), method, params, loopback)
			if frame == nil || frame.Code != transport.ErrCodeBackgroundAgentsRunning {
				t.Fatalf("%s %s loopback=%v: frame %+v, want %s", name, params[len(params)-1], loopback, frame, transport.ErrCodeBackgroundAgentsRunning)
			}
			if frame.Message != "Stopping now would also stop 1 background agent. Confirm to stop it." {
				t.Fatalf("%s loopback=%v: message %q", name, loopback, frame.Message)
			}
			var agents []BackgroundKillAgent
			if err := json.Unmarshal(frame.BackgroundAgents, &agents); err != nil {
				t.Fatalf("%s loopback=%v: backgroundAgents %s: %v", name, loopback, frame.BackgroundAgents, err)
			}
			want := BackgroundKillAgent{LaunchItemID: "agent", Description: "Agent agent", RunState: "running", TranscriptRootID: "agent"}
			if len(agents) != 1 || agents[0] != want {
				t.Fatalf("%s loopback=%v: agents %+v, want %+v", name, loopback, agents, want)
			}
		}
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("refused stops sent %d interrupts", n)
	}
}
