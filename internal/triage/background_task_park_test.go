package triage

// Tests for the PARK model (claude-wire.md §E6b). An async agent that
// stops while one of its owned background shells is still running is
// idle, not done: the CLI wakes it when the shell reports, and its stop
// looks exactly like a final stop on the wire (task_updated{completed} +
// task_notification). The stop writes a parked sibling (agent_stops.go),
// which settles nothing; the stop with no shell running writes the ending
// sibling.
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

// parkClock stamps the park helpers' events; pinParkClock fixes it.
var parkClock = time.Now

// pinParkClock stamps every park helper event with at until the test ends.
func pinParkClock(t *testing.T, at time.Time) {
	t.Helper()
	parkClock = func() time.Time { return at }
	t.Cleanup(func() { parkClock = time.Now })
}

func parkHandle(t *testing.T, router *Router, evt provider.ProviderEvent) {
	t.Helper()
	evt.Timestamp = parkClock()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle %s %s: %v", evt.Kind, evt.ItemID, err)
	}
}

// nextMillisecond waits out the current millisecond. The store bounds an
// agent's runs by creation time (Store.LatestSubagentReport), and a real
// run lasts far longer than one.
func nextMillisecond() { time.Sleep(2 * time.Millisecond) }

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

// parkCompletions maps each launch to its ENDING sibling.
func parkCompletions(t *testing.T, st *store.Store, threadID string) map[string]store.Item {
	t.Helper()
	out := map[string]store.Item{}
	for _, it := range findItemsByKind(t, st, threadID, itemKindBackgroundDone) {
		if it.Status != store.ItemStatusParked {
			out[it.CompletionOf] = it
		}
	}
	return out
}

// parkedStops lists launchID's parked siblings in write order.
func parkedStops(t *testing.T, st *store.Store, threadID, launchID string) []store.Item {
	t.Helper()
	var out []store.Item
	for _, it := range findItemsByKind(t, st, threadID, itemKindBackgroundDone) {
		if it.CompletionOf == launchID && it.Status == store.ItemStatusParked {
			out = append(out, it)
		}
	}
	return out
}

// liveLaunches is the tray's set of launch ids.
func liveLaunches(t *testing.T, st *store.Store, threadID string) map[string]bool {
	t.Helper()
	items, err := st.ListLiveBackgroundTasks(threadID, 0)
	if err != nil {
		t.Fatalf("list live background tasks: %v", err)
	}
	out := map[string]bool{}
	for _, it := range items {
		if it.CompletionOf == "" {
			out[it.ID] = true
		}
	}
	return out
}

// assertNoAgentBells fails on a notification row of any of the agent
// tasks: an agent's stop is its sibling, never a bell.
func assertNoAgentBells(t *testing.T, st *store.Store, threadID string, agentTaskIDs ...string) {
	t.Helper()
	for _, bell := range findItemsByKind(t, st, threadID, itemKindNotification) {
		taskID, _ := decodeItemMetaMap(t, bell.Meta)["task_id"].(string)
		for _, agentTaskID := range agentTaskIDs {
			if taskID == agentTaskID {
				t.Errorf("an agent's stop rang a bell: %s %q", bell.ID, bell.Summary)
			}
		}
	}
}

// A stop with an owned shell still running is a pause: the stash stays,
// a parked sibling records it, and the launch stays live.
func TestParkedAgent_StopWithLiveShellWritesAParkedSibling(t *testing.T) {
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
		t.Errorf("no ending sibling may be written at a pause, got %v", dones)
	}
	stops := parkedStops(t, st, "t1", "agent")
	if len(stops) != 1 {
		t.Fatalf("the stop writes one parked sibling, got %d", len(stops))
	}
	launch := mustGetItem(t, st, "t1", "agent")
	if stop := stops[0]; stop.Kind != itemKindBackgroundDone || stop.Role != "assistant" || !stop.IsBackground ||
		stop.ToolName != launch.ToolName || stop.Summary != launch.Summary+" -> parked" {
		t.Errorf("parked sibling = %s/%s %q tool %q background=%v, want a completion-shaped sibling of %q", stop.Kind, stop.Role, stop.Summary, stop.ToolName, stop.IsBackground, launch.Summary)
	}
	assertNoAgentBells(t, st, "t1", "task-agent")
	live := liveLaunches(t, st, "t1")
	if !live["agent"] || !live["shell"] {
		t.Errorf("both the parked agent and its shell stay live, got %v", live)
	}
}

// The full cycle: park, shell reports, wake drops the stash and opens the
// woken run under the root, the next stop with no live shell ends it.
func TestParkedAgent_WakeOpensRunAndFinalStopSettles(t *testing.T) {
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
	launch := mustGetItem(t, st, "t1", "agent")
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
			t.Errorf("wake prompt must not carry %s: it is not a §E6 run and nothing binds it", absent)
		}
	}
	if stop := parkedStops(t, st, "t1", "agent")[0]; prompt.CreatedAt <= stop.CreatedAt {
		t.Errorf("wake at %d does not follow the stop it wakes from at %d", prompt.CreatedAt, stop.CreatedAt)
	}
	// A re-delivered wake is a no-op.
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	if rows := findItemsByKind(t, st, "t1", itemKindUserText); len(rows) != 1 {
		t.Errorf("a re-delivered wake writes nothing: want one wake row, got %d user rows", len(rows))
	}

	parkStop(t, router, "t1", "agent", "task-agent", "WOKE", "u2")
	dones := parkCompletions(t, st, "t1")
	if len(dones) != 2 || dones["agent"].Status != statusCompleted || !dones["agent"].IsBackground {
		t.Fatalf("the stop with no live shell ends the agent, got %v", dones)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the final stop drains the stash")
	}
	if stops := parkedStops(t, st, "t1", "agent"); len(stops) != 1 {
		t.Errorf("the final stop writes no parked sibling: got %d", len(stops))
	}
	assertNoAgentBells(t, st, "t1", "task-agent")
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
		t.Fatalf("the parent ends at its stop despite the running child agent, got %v", dones)
	}
	if stops := parkedStops(t, st, "t1", "parent"); len(stops) != 0 {
		t.Errorf("the parent parked on its child agent: %v", stops)
	}
	if parkStashed(t, st, "t1", "task-parent") {
		t.Error("an ending stop drains its stash")
	}
}

// A user stop on a parked agent is final: task_updated{killed} ends it as
// stopped (capture E: the CLI sends no agent notification after it). The
// parked sibling stays as the record of the run before it.
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
	if stops := parkedStops(t, st, "t1", "agent"); len(stops) != 1 {
		t.Errorf("the kill kept %d parked siblings, want the one", len(stops))
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the kill consumes the parked stash")
	}
}

// A TaskOutput observation of a parked agent reads a pause: no ending
// sibling, stash kept.
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
// row's run: its parked sibling records it, so the rebind takes the row
// out of the live set and writes nothing. The carrier then parks and
// wakes like the root did, with the wake row still filed under the ROOT,
// and the root never gets a second card.
func TestParkedAgent_RebindRetiresTheParkedRowAndCarrierParksInTurn(t *testing.T) {
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
	if dones := parkCompletions(t, st, "t1"); dones["root"].ID != "" {
		t.Fatalf("the rebind wrote the parked root an ending sibling: %v", dones["root"])
	}
	if live := liveLaunches(t, st, "t1"); live["root"] {
		t.Errorf("the rebind left the parked root live: %v", live)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the rebind drops the parked root's stash")
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

	// The carrier's run stops with the shell still live: the CARRIER parks.
	parkStop(t, router, "t1", "carrier", "task-agent", "GOT-MESSAGE", "u2")
	if dones := parkCompletions(t, st, "t1"); dones["carrier"].ID != "" {
		t.Fatalf("the carrier must park like the root did, got %v", dones)
	}
	if stops := parkedStops(t, st, "t1", "carrier"); len(stops) != 1 {
		t.Fatalf("the carrier's stop wrote %d parked siblings of its own, want 1", len(stops))
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
	dones := parkCompletions(t, st, "t1")
	if len(dones) != 2 || dones["carrier"].Status != statusCompleted || dones["shell"].ID == "" {
		t.Fatalf("the shell and the carrier each end once and the root never, got %v", dones)
	}
	if stops := parkedStops(t, st, "t1", "root"); len(stops) != 1 {
		t.Errorf("the root keeps its one parked sibling, got %d", len(stops))
	}

	// The session's end settles nothing the rebind retired.
	if _, err := router.SettleBackgroundLaunchesForSessionEnd("t1"); err != nil {
		t.Fatalf("session-end settle: %v", err)
	}
	if dones := parkCompletions(t, st, "t1"); dones["root"].ID != "" {
		t.Errorf("the session's end wrote the retired root a sibling: %v", dones["root"])
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
