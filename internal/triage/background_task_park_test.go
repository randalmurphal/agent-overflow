package triage

// Tests for the PARK model (claude-wire.md §E6b). An async agent that
// stops while one of its owned background shells is still running is
// idle, not done: the CLI wakes it when the shell reports, and its stop
// looks exactly like a final stop on the wire (task_updated{completed} +
// task_notification). Before 2026-09-08 triage settled the launch at that
// first stop, and every woken round's rows, bells and counters then landed
// under a card the reader had been told was done.
//
// Every sequence here is the router.Handle shape the parser produces for
// the 2026-09-08 captures (local_agent_owned_shell_wake_20260908.ndjson and
// siblings).

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func parkHandle(t *testing.T, router *Router, evt provider.ProviderEvent) {
	t.Helper()
	evt.Timestamp = time.Now()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle %s %s: %v", evt.Kind, evt.ItemID, err)
	}
}

func parkMeta(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// parkLaunchAgent drives an async Agent launch to its backgrounded state:
// the launch tool_use, its task_started meta update, and the §E5 ack.
func parkLaunchAgent(t *testing.T, router *Router, threadID, launchID, taskID, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: launchID, ItemType: "Agent", ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Spike " + launchID, "prompt": "do the thing"}}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: launchID, ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"task_id": taskID, "task_type": "local_agent"}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: launchID, ParentToolUseID: parentID,
		Content: "Async agent launched successfully.", Meta: parkMeta(t, map[string]any{"is_background": true}),
	})
}

// parkLaunchShell drives a subagent-owned backgrounded Bash: task_started
// announces on the main wire before the row exists (the correlation hold),
// then the sidechain projection lands the row.
func parkLaunchShell(t *testing.T, router *Router, threadID, shellID, shellTaskID, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: shellID, ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"task_id": shellTaskID, "task_type": "local_bash", "parent_tool_use_id": parentID}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: shellID, ItemType: "Bash", ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"toolName": "Bash", "is_background": true, "input": map[string]any{"command": "sleep 60; echo LONG", "run_in_background": true}}),
	})
}

// parkStop is one agent stop: the host terminal (stashed) and the
// notification the CLI fires at EVERY stop, final or not.
func parkStop(t *testing.T, router *Router, threadID, boundID, taskID, summary, uuid string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: threadID, ItemID: boundID,
		Meta: parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": boundID, "status": "completed", "source": "task_updated"}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: threadID, ItemID: boundID, Content: summary,
		Meta: parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": boundID, "status": "completed", "uuid": uuid}),
	})
}

// parkShellDone is the owned shell's terminal pair, which precedes the
// wake on the wire.
func parkShellDone(t *testing.T, router *Router, threadID, shellID, shellTaskID, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: threadID, ItemID: shellID, ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"task_id": shellTaskID, "tool_use_id": shellID, "parent_tool_use_id": parentID, "status": "completed", "source": "task_updated"}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: threadID, ItemID: shellID, ParentToolUseID: parentID,
		Content: `Background command "sleep 60; echo LONG" completed (exit code 0)`,
		Meta:    parkMeta(t, map[string]any{"task_id": shellTaskID, "tool_use_id": shellID, "parent_tool_use_id": parentID, "status": "completed", "uuid": "u-" + shellTaskID}),
	})
}

// parkWake is the parser's event for the wake task_started: a user_text
// under the BOUND tool_use carrying the wake marker.
func parkWake(t *testing.T, router *Router, threadID, boundID, taskID, shellID, shellTaskID string, extra map[string]any) {
	t.Helper()
	fields := map[string]any{
		"wire_only": true, provider.MetaSubagentWakePromptKey: true, "task_id": taskID,
		provider.MetaWakeTaskIDKey: shellTaskID, provider.MetaWakeToolUseIDKey: shellID, provider.MetaWakeStatusKey: "completed",
	}
	for k, v := range extra {
		fields[k] = v
	}
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: threadID, ItemID: provider.SubagentWakePromptItemID(shellID), Role: "user",
		Content: `Background command "sleep 60; echo LONG" completed (exit code 0)`, ContentPresent: true,
		ParentToolUseID: boundID, Meta: parkMeta(t, fields),
	})
}

func parkStashed(t *testing.T, st *store.Store, threadID, taskID string) bool {
	t.Helper()
	_, found, err := st.GetPendingBackgroundTerminal(threadID, taskID)
	if err != nil {
		t.Fatalf("stash lookup %s: %v", taskID, err)
	}
	return found
}

func parkCompletions(t *testing.T, st *store.Store, threadID string) map[string]store.Item {
	t.Helper()
	out := map[string]store.Item{}
	for _, it := range findItemsByKind(t, st, threadID, itemKindBackgroundDone) {
		out[it.CompletionOf] = it
	}
	return out
}

// A stop with an owned shell still running is a pause: the stash stays,
// no sibling is written, the bell rings, and the launch stays live.
func TestParkedAgent_StopWithLiveShellKeepsStashAndWritesNoSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")

	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	if !parkStashed(t, st, "t1", "task-agent") {
		t.Error("the parked agent's terminal must stay stashed for the wake to drop")
	}
	if dones := parkCompletions(t, st, "t1"); len(dones) != 0 {
		t.Errorf("no completion sibling may be written at a pause, got %v", dones)
	}
	if bells := findItemsByKind(t, st, "t1", itemKindNotification); len(bells) != 1 {
		t.Errorf("the stop still rings the bell once, got %d rows", len(bells))
	}
	running, err := st.ListRunningBackgroundToolCalls("t1")
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	ids := map[string]bool{}
	for _, it := range running {
		ids[it.ID] = true
	}
	if !ids["agent"] || !ids["shell"] {
		t.Errorf("both the parked agent and its shell stay live, got %v", ids)
	}
}

// The full cycle: park, shell reports, wake drops the stash and opens the
// woken round under the root, the next stop with no live shell settles.
func TestParkedAgent_WakeOpensRoundAndFinalStopSettles(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	// The main thread moves on while the agent is parked, so the wake
	// must file under the LAUNCH's turn, not the write head.
	seedOpenTurn(t, router, st, "t1", 1)

	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	if dones := parkCompletions(t, st, "t1"); len(dones) != 1 || dones["shell"].ID == "" {
		t.Fatalf("the shell settles on its own terminal, got %v", dones)
	}

	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the wake must drop the parked terminal: that stop was a pause")
	}
	launch, _, err := st.GetThreadItem("t1", "agent")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	prompt, found, err := st.GetThreadItem("t1", provider.SubagentWakePromptItemID("shell"))
	if err != nil || !found {
		t.Fatalf("wake prompt row missing: found=%v err=%v", found, err)
	}
	if prompt.Kind != itemKindUserText || prompt.Role != "user" || prompt.ParentID != "agent" || prompt.TurnIndex != launch.TurnIndex {
		t.Errorf("wake prompt = %s/%s under %q turn %d, want user_text/user under agent turn %d", prompt.Kind, prompt.Role, prompt.ParentID, prompt.TurnIndex, launch.TurnIndex)
	}
	promptMeta := decodeItemMetaMap(t, prompt.Meta)
	if promptMeta[provider.MetaSubagentWakePromptKey] != true || promptMeta[provider.MetaWakeTaskIDKey] != "task-shell" {
		t.Errorf("wake prompt meta = %v, want the wake marker and the shell", promptMeta)
	}
	for _, absent := range []string{provider.MetaSubagentResumePromptKey, provider.MetaSubagentPromptProvisionalKey, provider.MetaTranscriptRootIDKey} {
		if _, has := promptMeta[absent]; has {
			t.Errorf("wake prompt must not carry %s: it is not a §E6 round and nothing binds it", absent)
		}
	}
	// A re-delivered wake is a no-op.
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	if rows := findItemsByKind(t, st, "t1", itemKindUserText); len(rows) != 1 {
		t.Errorf("a re-delivered wake writes nothing: want one wake row, got %d user rows", len(rows))
	}

	parkStop(t, router, "t1", "agent", "task-agent", "WOKE", "u2")
	dones := parkCompletions(t, st, "t1")
	if len(dones) != 2 || dones["agent"].Status != statusCompleted || !dones["agent"].IsBackground {
		t.Fatalf("the stop with no live shell settles the agent, got %v", dones)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the final stop drains the stash")
	}
	if bells := findItemsByKind(t, st, "t1", itemKindNotification); len(bells) != 2 {
		t.Errorf("one bell per stop, got %d", len(bells))
	}
}

// A nested async AGENT is a top-level task of its own and never wakes its
// parent (spike D, 2026-09-08): it must not park the parent.
func TestParkedAgent_NestedAsyncAgentChildDoesNotPark(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "parent", "task-parent", "")
	parkLaunchAgent(t, router, "t1", "child", "task-child", "parent")

	parkStop(t, router, "t1", "parent", "task-parent", "WAITING", "u1")

	dones := parkCompletions(t, st, "t1")
	if dones["parent"].ID == "" {
		t.Fatalf("the parent settles at its stop despite the running child agent, got %v", dones)
	}
	if parkStashed(t, st, "t1", "task-parent") {
		t.Error("a settled stop drains its stash")
	}
}

// A user stop on a parked agent is final: task_updated{killed} settles it
// as stopped (capture E: the CLI sends no agent notification after it).
func TestParkedAgent_KillSettlesAsStopped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "killed", "is_error": true, "source": "task_updated"}),
	})
	dones := parkCompletions(t, st, "t1")
	if dones["agent"].Status != statusKilled {
		t.Fatalf("kill while parked = %v, want a stopped sibling", dones)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the kill consumes the parked stash")
	}
}

// A TaskOutput observation of a parked agent reads a pause: no sibling,
// stash kept.
func TestParkedAgent_TaskOutputObservationDoesNotSettle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent", Content: "WAITING",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "completed", "source": "task_output"}),
	})
	if dones := parkCompletions(t, st, "t1"); len(dones) != 0 {
		t.Errorf("TaskOutput on a parked agent must not settle it, got %v", dones)
	}
	if !parkStashed(t, st, "t1", "task-agent") {
		t.Error("the stash stays for the wake")
	}
}

// A §E6 SendMessage rebind onto a parked agent (capture C) ends the parked
// row's round: it settles from its stash before the carrier takes over.
// The carrier then parks and wakes like the root did, with the wake row
// still filed under the ROOT.
func TestParkedAgent_RebindSettlesTheParkedRowAndCarrierParksInTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "root", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "root")
	parkStop(t, router, "t1", "root", "task-agent", "WAITING", "u1")

	// The rebind: SendMessage tool_use, the rebind task_started (meta
	// update carrying the §E6 stamps), the resume prompt, the ack.
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "carrier", ItemType: "SendMessage",
		Meta: parkMeta(t, map[string]any{"toolName": "SendMessage", "input": map[string]any{"to": "task-agent", "message": "PING"}}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "carrier",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "task_type": "local_agent", "resumes_tool_use_id": "root", "description": "Spike root", "subagent_type": "general-purpose", "transcript_root_id": "root"}),
	})
	dones := parkCompletions(t, st, "t1")
	if dones["root"].Status != statusCompleted {
		t.Fatalf("the rebind settles the parked root from its stash, got %v", dones)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the rebind consumes the parked stash")
	}
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", ItemID: provider.SubagentOpeningPromptItemID("carrier"), Role: "user",
		Content: "PING", ContentPresent: true, ParentToolUseID: "carrier",
		Meta: parkMeta(t, map[string]any{"wire_only": true, provider.MetaSubagentResumePromptKey: true, provider.MetaSubagentPromptProvisionalKey: true, provider.MetaResumeCarrierIDKey: "carrier", provider.MetaTranscriptRootIDKey: "root"}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "carrier",
		Content: `{"success":true,"message":"Resuming agent","resumedAgentId":"task-agent"}`, Meta: parkMeta(t, map[string]any{"is_background": true}),
	})

	// Round 2 stops with the shell still live: the CARRIER parks.
	parkStop(t, router, "t1", "carrier", "task-agent", "GOT-MESSAGE", "u2")
	if dones := parkCompletions(t, st, "t1"); dones["carrier"].ID != "" {
		t.Fatalf("the carrier must park like the root did, got %v", dones)
	}
	if !parkStashed(t, st, "t1", "task-agent") {
		t.Error("the carrier's stop stays stashed")
	}

	// The shell reports; the wake names the carrier (the bound tool_use)
	// and lands under the root.
	parkShellDone(t, router, "t1", "shell", "task-shell", "root")
	parkWake(t, router, "t1", "carrier", "task-agent", "shell", "task-shell", map[string]any{provider.MetaTranscriptRootIDKey: "root"})
	prompt, found, err := st.GetThreadItem("t1", provider.SubagentWakePromptItemID("shell"))
	if err != nil || !found {
		t.Fatalf("wake prompt row missing: found=%v err=%v", found, err)
	}
	if prompt.ParentID != "root" {
		t.Errorf("wake prompt under %q, want the transcript root", prompt.ParentID)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the wake drops the carrier's parked stash")
	}

	parkStop(t, router, "t1", "carrier", "task-agent", "WOKE", "u3")
	dones = parkCompletions(t, st, "t1")
	if len(dones) != 3 || dones["carrier"].Status != statusCompleted {
		t.Fatalf("root, shell and carrier each settle once, got %v", dones)
	}
}

// Session end settles a parked agent from its stash like any other
// backgrounded launch: the pause is over because the process is.
func TestParkedAgent_SessionEndSettlesFromStash(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	if _, err := router.SettleBackgroundLaunchesForSessionEnd("t1"); err != nil {
		t.Fatalf("session-end settle: %v", err)
	}
	dones := parkCompletions(t, st, "t1")
	if dones["agent"].Status != statusCompleted || dones["shell"].ID == "" {
		t.Fatalf("session end settles the parked agent (completed, from its stash) and kills the shell, got %v", dones)
	}
}
