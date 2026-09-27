package triage

// A FOREGROUND Bash the CLI moved to the background while it ran
// (claude-wire.md §E2b): `task_updated{is_backgrounded:true}` stamps the
// launch as a background row, then its result either acks the move, and
// the row runs on until its terminal, or is the command's real output,
// and the row settles in place and leaves the tray.

import (
	"testing"

	"agent-overflow/internal/provider"
)

// parkMovedShell drives a subagent's foreground Bash to the moment the
// CLI moved it: task_started, the launch row with no run_in_background,
// and the is_backgrounded patch.
func parkMovedShell(t *testing.T, router *Router, threadID, shellID, shellTaskID, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: shellID, ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"task_id": shellTaskID, "task_type": "local_bash", "parent_tool_use_id": parentID}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: shellID, ItemType: "Bash", ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"toolName": "Bash", "input": map[string]any{"command": "go test ./...", "timeout": 300000}}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventSubagentBackgrounded, ThreadID: threadID, ItemID: shellID, ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"task_id": shellTaskID}),
	})
}

// lastTrayFrameNaming is the newest tray frame that answers for id.
func lastTrayFrameNaming(t *testing.T, emissions *emissionLog, id string) BackgroundTrayEvent {
	t.Helper()
	frames := trayFrames(emissions)
	for i := len(frames) - 1; i >= 0; i-- {
		for _, launch := range frames[i].LaunchIDs {
			if launch == id {
				return frames[i]
			}
		}
	}
	t.Fatalf("no tray frame named %s in %+v", id, frames)
	return BackgroundTrayEvent{}
}

// A moved command that finished as it was moved answers with its real
// output. The row settles in place, and the tray that listed it running
// hears it leave: the settled push of an ordinary call names no launch.
func TestMovedShellThatSettlesInPlaceLeavesTheTray(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")

	emissions.reset()
	parkMovedShell(t, router, "t1", "shell", "task-shell", "agent")
	if row, ok := trayRow(lastTrayFrameNaming(t, emissions, "shell"), "shell"); !ok || row.Status != statusRunning || !row.IsBackground {
		t.Fatalf("the moved shell must join the tray running, got %+v ok=%v", row, ok)
	}

	emissions.reset()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "shell", ParentToolUseID: "agent",
		Content: "ok  agent-overflow/internal/store 12.1s", Meta: parkMeta(t, map[string]any{"exit_code": 0}),
	})
	frame := lastTrayFrameNaming(t, emissions, "shell")
	namesOnly(t, "settle in place", frame, "shell")
	if _, ok := trayRow(frame, "shell"); ok {
		t.Fatalf("the settled shell must leave the tray, frame carried %+v", frame.Rows)
	}
	if live := liveLaunches(t, st, "t1"); live["shell"] {
		t.Fatalf("the settled shell is no live launch, got %v", live)
	}
}

// A refused run_in_background launch at the top level joins the tray at
// its launch and leaves it when its error settles it.
func TestRefusedLaunchLeavesTheTray(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "shell", ItemType: "Bash",
		Meta: parkMeta(t, map[string]any{"toolName": "Bash", "is_background": true, "input": map[string]any{"command": "make apk", "run_in_background": true}}),
	})
	if _, ok := trayRow(lastTrayFrameNaming(t, emissions, "shell"), "shell"); !ok {
		t.Fatal("the flagged launch must join the tray")
	}

	emissions.reset()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "shell",
		Content: "Permission to use Bash has been denied", Meta: parkMeta(t, map[string]any{"is_error": true}),
	})
	frame := lastTrayFrameNaming(t, emissions, "shell")
	if _, ok := trayRow(frame, "shell"); ok {
		t.Fatalf("the refused launch must leave the tray, frame carried %+v", frame.Rows)
	}
}

// A moved command that acks the move keeps running as its agent's
// background command: the agent's stop while it runs is a pause, and the
// command's own terminal settles it and takes it out of the tray.
func TestMovedShellAckKeepsItRunningAndParksItsAgent(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkMovedShell(t, router, "t1", "shell", "task-shell", "agent")

	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "shell", ParentToolUseID: "agent",
		Content: "Command did not complete within its 300s timeout and was moved to the background (ID: task-shell).",
		Meta:    parkMeta(t, map[string]any{"is_background": true, "task_id": "task-shell"}),
	})
	if shell := mustGetItem(t, st, "t1", "shell"); shell.Status != statusRunning || !shell.IsBackground {
		t.Fatalf("the acked shell keeps running in the background, got status=%s background=%v", shell.Status, shell.IsBackground)
	}

	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")
	if stops := parkedStops(t, st, "t1", "agent"); len(stops) != 1 {
		t.Fatalf("the agent stopping while its moved shell runs parks, got %d parked stops", len(stops))
	}
	if dones := parkCompletions(t, st, "t1"); dones["agent"].ID != "" {
		t.Fatalf("the agent must not end while its shell runs, got %v", dones)
	}

	emissions.reset()
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	if dones := parkCompletions(t, st, "t1"); dones["shell"].ID == "" {
		t.Fatalf("the shell's terminal writes its completion sibling, got %v", dones)
	}
	frame := lastTrayFrameNaming(t, emissions, "shell")
	settled := false
	for _, row := range frame.Rows {
		settled = settled || (row.CompletionOf == "shell" && row.Kind == itemKindBackgroundDone)
	}
	if !settled {
		t.Fatalf("the shell's completion must reach the tray, frame carried %+v", frame.Rows)
	}
}
