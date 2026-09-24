package triage

// The background tray reads its rows, and each agent row's latest-tool
// line, from ListLiveBackgroundTasks, and watches no child scope: a row
// pushed under a parent reaches only a connection watching that parent.
// So the writes that change what the list serves for a row the tray does
// not receive pushes for announce it on provider:background_tasks_changed.

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

func trayNudges(emissions *emissionLog) int {
	return countEvents(emissions.snapshot(), eventchan.ProviderBackgroundTasksChanged.String())
}

func trayChildTool(t *testing.T, router *Router, threadID, id, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: id, ItemType: "Read", ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"toolName": "Read", "input": map[string]any{"file_path": "/tmp/" + id}}),
	})
}

// A nested background agent's stamp moves when its rows are written; its
// push is scoped to its parent, so the refresh that pushes it nudges the
// tray. The first child's anchor push nudges the same way. A
// top-level agent's own push reaches the tray, and its stamp change
// nudges nothing.
func TestNestedLiveLaunchStampChangeNudgesTheTray(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "outer", "task-outer", "")
	parkLaunchAgent(t, router, "t1", "nested", "task-nested", "outer")
	router.DrainWireItemRefresh()

	emissions.reset()
	trayChildTool(t, router, "t1", "c1", "nested")
	if got := trayNudges(emissions); got != 1 {
		t.Errorf("the nested agent's first child nudged %d times, want 1", got)
	}
	router.DrainWireItemRefresh()

	emissions.reset()
	trayChildTool(t, router, "t1", "c2", "nested")
	if got := trayNudges(emissions); got != 0 {
		t.Fatalf("a child write nudged %d times before the refresh, want 0", got)
	}
	router.DrainWireItemRefresh()
	if got := trayNudges(emissions); got != 1 {
		t.Errorf("the refresh that restamped the nested agent nudged %d times, want 1", got)
	}

	emissions.reset()
	trayChildTool(t, router, "t1", "c3", "outer")
	router.DrainWireItemRefresh()
	if got := trayNudges(emissions); got != 0 {
		t.Errorf("a top-level agent's restamp nudged %d times, want 0", got)
	}
}

// A settled nested agent is not in the live list: its restamp nudges
// nothing.
func TestSettledNestedLaunchStampChangeDoesNotNudge(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "outer", "task-outer", "")
	parkLaunchAgent(t, router, "t1", "nested", "task-nested", "outer")
	trayChildTool(t, router, "t1", "c1", "nested")
	parkStop(t, router, "t1", "nested", "task-nested", "done", "u1")
	router.DrainWireItemRefresh()

	emissions.reset()
	trayChildTool(t, router, "t1", "c2", "nested")
	router.DrainWireItemRefresh()
	if got := trayNudges(emissions); got != 0 {
		t.Errorf("a settled nested agent's restamp nudged %d times, want 0", got)
	}
}

// A live Codex agent's direct tool call nudges the tray, which reads the
// agent's latest-tool line from the list. A tool call under a Codex agent
// that is no longer live nudges nothing.
func TestCodexAgentDirectToolCallNudgesTheTray(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	spawnMeta := buildSpawnAgentMeta(t, "child-1", "running")
	for _, kind := range []provider.EventKind{provider.EventToolStart, provider.EventToolComplete} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: kind, ThreadID: "t1", ItemID: "spawn-1", ItemType: "collab_agent", TurnID: "turn-0",
			Meta: spawnMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("spawn %s: %v", kind, err)
		}
	}
	if live := router.ListLiveCodexAgentTasks("t1"); len(live) != 1 {
		t.Fatalf("live Codex agents = %+v, want spawn-1", live)
	}

	emissions.reset()
	trayChildTool(t, router, "t1", "child-read", "spawn-1")
	if got := trayNudges(emissions); got != 1 {
		t.Errorf("a live agent's direct tool call nudged %d times, want 1", got)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-1",
		Meta: json.RawMessage(`{"agent_path":"child-1","status":"completed"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child completed: %v", err)
	}
	if live := router.ListLiveCodexAgentTasks("t1"); len(live) != 0 {
		t.Fatalf("live Codex agents after completion = %+v, want none", live)
	}
	emissions.reset()
	trayChildTool(t, router, "t1", "late-read", "spawn-1")
	if got := trayNudges(emissions); got != 0 {
		t.Errorf("a tool call under a finished agent nudged %d times, want 0", got)
	}
}
