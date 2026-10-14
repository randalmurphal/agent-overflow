package triage

// A parked agent whose wake the CLI drops (claude-wire.md §E6b, dropped
// wake): the owned command reports, the CLI fails to resume the agent and
// records the drop, and nothing else on the wire says so. The sequences
// are the router.Handle shape the parser produces for the 2026-10-14
// capture (local_agent_dropped_wake_20261014.ndjson), and the stop
// replies are the 2026-09-08 captures' order plus that capture's reply to
// a stop of the dropped agent.

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func dropWake(t *testing.T, router *Router, threadID, taskID, toolUseID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventAgentWakeDropped, ThreadID: threadID,
		Meta: parkMeta(t, map[string]any{"taskId": taskID, "toolUseId": toolUseID, "reason": "agent_stopped"}),
	})
}

func stopAcked(t *testing.T, router *Router, threadID, taskID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskStopAcked, ThreadID: threadID,
		Meta: parkMeta(t, map[string]any{"taskId": taskID}),
	})
}

// servedRunState is the run state the tray serves on launchID.
func servedRunState(t *testing.T, st *store.Store, threadID, launchID string) string {
	t.Helper()
	items, err := st.ListLiveBackgroundTasks(threadID, 0)
	if err != nil {
		t.Fatalf("list live background tasks: %v", err)
	}
	for _, it := range items {
		if it.ID == launchID {
			return store.AgentRunState(it)
		}
	}
	t.Fatalf("launch %s not served", launchID)
	return ""
}

func parkedAgentWithShell(t *testing.T, router *Router, st *store.Store) {
	t.Helper()
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
}

// The incident: the shell reports, the wake is dropped, and the parked
// agent ends there instead of waiting on its session.
func TestDroppedWake_EndsTheParkedAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	parkedAgentWithShell(t, router, st)
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	if got := servedRunState(t, st, "t1", "agent"); got != store.AgentRunParked {
		t.Fatalf("before the drop the agent is %q, want parked", got)
	}

	dropWake(t, router, "t1", "task-shell", "shell")

	end := parkCompletions(t, st, "t1")["agent"]
	if end.Status != statusErrored {
		t.Fatalf("ending sibling = %+v, want an errored one", end)
	}
	if source := decodeItemMetaMap(t, end.Meta)["status_source"]; source != wakeDroppedSource {
		t.Errorf("status_source = %v, want %s", source, wakeDroppedSource)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the park terminal must be consumed, not left for a wake that never comes")
	}
	if stops := parkedStops(t, st, "t1", "agent"); len(stops) != 1 {
		t.Errorf("the parked card must stay as it was, got %d parked siblings", len(stops))
	}
	if got := servedRunState(t, st, "t1", "agent"); got != store.AgentRunEnded {
		t.Errorf("served run state = %q, want ended", got)
	}
	assertNoAgentBells(t, st, "t1", "task-agent")

	// A repeated drop record writes nothing more.
	dropWake(t, router, "t1", "task-shell", "shell")
	if again := parkCompletions(t, st, "t1")["agent"]; again.UpdatedAt != end.UpdatedAt || again.Status != end.Status {
		t.Errorf("a repeated drop rewrote the ending sibling: %+v -> %+v", end, again)
	}
}

// The drop record can reach AO before the command's own terminal: the
// record proves the command finished, so the agent ends all the same, and
// the command's late report settles the command and nothing more.
func TestDroppedWake_BeforeTheCommandsTerminalEndsTheAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	parkedAgentWithShell(t, router, st)

	dropWake(t, router, "t1", "task-shell", "shell")
	end := parkCompletions(t, st, "t1")["agent"]
	if end.Status != statusErrored {
		t.Fatalf("ending sibling = %+v, want an errored one", end)
	}

	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	if again := parkCompletions(t, st, "t1")["agent"]; again.UpdatedAt != end.UpdatedAt || again.Status != end.Status {
		t.Errorf("the late terminal rewrote the agent's end: %+v -> %+v", end, again)
	}
	if got := servedRunState(t, st, "t1", "agent"); got != store.AgentRunEnded {
		t.Errorf("served run state = %q, want ended", got)
	}
	assertNoAgentBells(t, st, "t1", "task-agent")
}

// A drop while the agent still owns a live command keeps it parked: that
// command's report is the CLI's next wake attempt, with its own outcome.
func TestDroppedWake_AgentWithALiveCommandStaysParked(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	dropWake(t, router, "t1", "task-shell", "shell")
	if _, ended := parkCompletions(t, st, "t1")["agent"]; ended {
		t.Fatal("the agent ended while a command it owns still runs")
	}
	if got := servedRunState(t, st, "t1", "agent"); got != store.AgentRunParked {
		t.Fatalf("run state = %q, want parked", got)
	}

	parkShellDone(t, router, "t1", "shell-2", "task-shell-2", "agent")
	dropWake(t, router, "t1", "task-shell-2", "shell-2")
	if end := parkCompletions(t, st, "t1")["agent"]; end.Status != statusErrored {
		t.Fatalf("after its last command's dropped wake the agent = %+v, want ended", end)
	}
}

// A drop never ends an agent that woke, nor reaches past the agent that
// owns the command.
func TestDroppedWake_LeavesWokenAndUnrelatedAgents(t *testing.T) {
	router, st, _ := newTestRouter(t)
	parkedAgentWithShell(t, router, st)
	parkLaunchAgent(t, router, "t1", "other", "task-other", "")
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)

	dropWake(t, router, "t1", "task-shell", "shell")
	dropWake(t, router, "t1", "task-unknown", "")
	if dones := parkCompletions(t, st, "t1"); len(dones) != 1 || dones["shell"].ID == "" {
		t.Fatalf("drops ended %v, want only the shell's own completion", dones)
	}
	if got := servedRunState(t, st, "t1", "agent"); got != store.AgentRunRunning {
		t.Errorf("woken agent = %q, want running", got)
	}
}

// A drop record for a task with no owning agent is not an error.
func TestDroppedWake_MainThreadCommandIsNoAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchShell(t, router, "t1", "main-shell", "task-main", "")
	dropWake(t, router, "t1", "task-main", "main-shell")
	if dones := parkCompletions(t, st, "t1"); len(dones) != 0 {
		t.Fatalf("a drop for a main-thread command wrote %v", dones)
	}
}

// A stop's kill frames precede its reply: the reply finds the agent
// ended by them and writes nothing of its own.
func TestStopAck_AfterKillFramesWritesNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	parkedAgentWithShell(t, router, st)
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "killed", "is_error": true, "source": "task_updated"}),
	})
	killed := parkCompletions(t, st, "t1")["agent"]

	stopAcked(t, router, "t1", "task-agent")

	end := parkCompletions(t, st, "t1")["agent"]
	if end.Status != statusKilled || end.UpdatedAt != killed.UpdatedAt {
		t.Fatalf("the reply changed the kill's sibling: %+v -> %+v", killed, end)
	}
	if source := decodeItemMetaMap(t, end.Meta)["status_source"]; source != "task_updated" {
		t.Errorf("status_source = %v, want the kill's", source)
	}
}

// A kill frame for the run that lands after the reply enriches the stop's
// end in place, as a later terminal does any sibling: one end, still
// stopped, and no terminal left stashed.
func TestStopAck_KillFrameAfterTheReplyKeepsOneStoppedEnd(t *testing.T) {
	router, st, _ := newTestRouter(t)
	parkedAgentWithShell(t, router, st)
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	stopAcked(t, router, "t1", "task-agent")
	end := parkCompletions(t, st, "t1")["agent"]

	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "killed", "is_error": true, "source": "task_updated"}),
	})

	again := parkCompletions(t, st, "t1")["agent"]
	if again.ID != end.ID || again.Status != statusKilled || again.CreatedAt != end.CreatedAt {
		t.Fatalf("the late kill changed the stop's end: %+v -> %+v", end, again)
	}
	if ends := len(parkCompletions(t, st, "t1")); ends != 2 {
		t.Errorf("completions = %d, want the shell's and the agent's", ends)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the late kill must not be left stashed for an agent that ended")
	}
}

// The 2026-10-14 capture: a stop of an agent whose wake was dropped gets
// the bare reply. The reply is the Stop's only answer, and it ends the
// agent as stopped.
func TestStopAck_AloneEndsTheParkedAgentAsStopped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	parkedAgentWithShell(t, router, st)
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")

	stopAcked(t, router, "t1", "task-agent")

	end := parkCompletions(t, st, "t1")["agent"]
	if end.Status != statusKilled {
		t.Fatalf("ending sibling = %+v, want a stopped one", end)
	}
	if source := decodeItemMetaMap(t, end.Meta)["status_source"]; source != stopAckedSource {
		t.Errorf("status_source = %v, want %s", source, stopAckedSource)
	}
	if parkStashed(t, st, "t1", "task-agent") {
		t.Error("the stop must consume the park terminal")
	}
	if got := servedRunState(t, st, "t1", "agent"); got != store.AgentRunDone {
		t.Errorf("served run state = %q, want done", got)
	}
}

// The reply to a stop of a command that already reported changes
// nothing: its kill frames, or its own report, wrote the sibling.
func TestStopAck_SettledCommandIsLeftAlone(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchShell(t, router, "t1", "main-shell", "task-main", "")
	parkShellDone(t, router, "t1", "main-shell", "task-main", "")
	done := parkCompletions(t, st, "t1")["main-shell"]

	stopAcked(t, router, "t1", "task-main")

	if end := parkCompletions(t, st, "t1")["main-shell"]; end.Status != done.Status || end.UpdatedAt != done.UpdatedAt ||
		decodeItemMetaMap(t, end.Meta)["status_source"] != decodeItemMetaMap(t, done.Meta)["status_source"] {
		t.Fatalf("the reply rewrote a settled command: %+v -> %+v", done, end)
	}
}

// A command whose exit the host reported but whose notification never
// came settles from that report, not as killed.
func TestStopAck_CommandSettlesFromItsStashedExit(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchShell(t, router, "t1", "main-shell", "task-main", "")
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "main-shell",
		Meta: parkMeta(t, map[string]any{"task_id": "task-main", "tool_use_id": "main-shell", "status": "completed", "source": "task_updated"}),
	})

	stopAcked(t, router, "t1", "task-main")

	if end := parkCompletions(t, st, "t1")["main-shell"]; end.Status != statusCompleted {
		t.Fatalf("stopped command = %+v, want its stashed completed exit", end)
	}
}
