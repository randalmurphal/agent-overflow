package triage

// Tests for the pre-row background-task correlation hold and the
// session-end settle (2026-08-31 tray-zombie class). Claude emits
// `system/task_started` on the main wire the moment ANY agent —
// including a nested async subagent — backgrounds a shell, but the
// launch row for a subagent-owned Bash only lands when the subagent
// transcript projection catches up. Dropping the meta update there
// permanently stripped the row of its task_id: no Stop button, no
// terminal correlation, and (because invariant 24 exempts nested rows
// from every top-level lifecycle gate) a tray row that ticked forever.

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func handleToolStart(t *testing.T, router *Router, threadID, itemID, itemType string, meta map[string]any) {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID,
		ItemType: itemType, Meta: raw, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool start %s: %v", itemID, err)
	}
}

// TestTaskStartedBeforeLaunchRowStampsTaskID pins that a task_started
// meta update arriving before its tool_call row exists is held and
// applied when the row lands, instead of being dropped.
func TestTaskStartedBeforeLaunchRowStampsTaskID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Parent agent launch row, so the held parent_tool_use_id survives
	// the dangling-parent check on persist.
	handleToolStart(t, router, "t1", "agent-1", "Task", map[string]any{
		"toolName":      "Task",
		"is_background": true,
		"input":         map[string]any{"description": "child agent"},
	})

	// task_started for the subagent-owned shell — BEFORE its row exists.
	handleToolStart(t, router, "t1", "bg-late", "", map[string]any{
		"task_id":            "tsk-late",
		"parent_tool_use_id": "agent-1",
	})

	// No ghost row fabricated.
	if _, found, err := st.GetThreadItem("t1", "bg-late"); err != nil {
		t.Fatalf("lookup: %v", err)
	} else if found {
		t.Fatal("meta update must not fabricate a tool_call row")
	}

	// The launch row lands later (transcript projection catch-up).
	handleToolStart(t, router, "t1", "bg-late", "Bash", map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 999"},
	})

	launch, found, err := st.GetThreadItem("t1", "bg-late")
	if err != nil || !found {
		t.Fatalf("launch row missing after create: found=%v err=%v", found, err)
	}
	if got := taskIDFromItemMeta(launch.Meta); got != "tsk-late" {
		t.Errorf("task_id = %q, want tsk-late (held correlation applied)", got)
	}
	if launch.ParentID != "agent-1" {
		t.Errorf("ParentID = %q, want agent-1", launch.ParentID)
	}
	if launch.Status != statusRunning {
		t.Errorf("Status = %q, want running", launch.Status)
	}
	// Shell still running: no completion sibling.
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Errorf("no sibling expected while shell runs, got %+v", dones)
	}

	// The hold is consumed: a second create must not re-apply anything
	// (take semantics — bounded map cannot accrete).
	if _, ok := router.takePendingToolCorrelation("t1", "bg-late"); ok {
		t.Error("hold should have been consumed by the create")
	}
}

// TestKilledTerminalBeforeLaunchRowSettlesWhenRowArrives pins the
// terminal-before-row ordering: a foreground agent exits, the CLI kills
// its backgrounded shells and emits task_updated{killed} on the main
// wire, and only afterwards does the launch row land. The killed
// terminal must be stashed (not dropped) and drained into the
// completion sibling when the row arrives.
func TestKilledTerminalBeforeLaunchRowSettlesWhenRowArrives(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	handleToolStart(t, router, "t1", "bg-k", "", map[string]any{"task_id": "tsk-k"})

	termMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-k", "status": "killed", "source": "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-k",
		Meta: termMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle killed terminal: %v", err)
	}

	// The killed terminal is stashed, not destroyed.
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-k"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if !found {
		t.Fatal("killed terminal with no launch row must be stashed")
	}

	handleToolStart(t, router, "t1", "bg-k", "Bash", map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 999"},
	})

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 sibling after late launch, got %+v", dones)
	}
	if dones[0].CompletionOf != "bg-k" {
		t.Errorf("CompletionOf = %q, want bg-k", dones[0].CompletionOf)
	}
	if dones[0].Status != statusKilled {
		t.Errorf("Status = %q, want %q", dones[0].Status, statusKilled)
	}
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-k"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if found {
		t.Error("stash should be drained by the late-launch settle")
	}
}

// TestCompletedTerminalBeforeLaunchRowSettlesWhenRowArrives is the
// non-killed sibling of the test above: a subagent-owned shell exits
// normally (task_updated{completed} → stash) before its row lands.
func TestCompletedTerminalBeforeLaunchRowSettlesWhenRowArrives(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	handleToolStart(t, router, "t1", "bg-c", "", map[string]any{"task_id": "tsk-c"})

	termMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-c", "status": "completed", "exit_code": 0, "source": "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-c",
		Meta: termMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle completed terminal: %v", err)
	}

	handleToolStart(t, router, "t1", "bg-c", "Bash", map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "make build"},
	})

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 sibling after late launch, got %+v", dones)
	}
	if dones[0].Status != statusCompleted {
		t.Errorf("Status = %q, want %q (stash outcome, not killed)", dones[0].Status, statusCompleted)
	}
}

// TestSettleBackgroundLaunchesForSessionEnd pins the per-thread settle
// that runs when a Claude session closes or dies: nested AND top-level
// launches settle (invariant 24 exempts nested rows from every
// top-level gate, so before this they were permanent zombies),
// foreground rows and other threads stay untouched, and the thread's
// leftover stash rows are pruned.
func TestSettleBackgroundLaunchesForSessionEnd(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")

	// t1: async agent launch + nested backgrounded Bash under it +
	// top-level backgrounded Bash + a foreground Bash.
	handleToolStart(t, router, "t1", "agent-1", "Task", map[string]any{
		"toolName":      "Task",
		"is_background": true,
		"input":         map[string]any{"description": "async child"},
	})
	handleToolStart(t, router, "t1", "bg-nested", "Bash", map[string]any{
		"toolName":           "Bash",
		"is_background":      true,
		"parent_tool_use_id": "agent-1",
		"input":              map[string]any{"command": "sleep 999"},
	})
	handleToolStart(t, router, "t1", "bg-top", "Bash", map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 998"},
	})
	handleToolStart(t, router, "t1", "fg-1", "Bash", map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "ls"},
	})
	// Rowless stash on t1 (a subagent-private shell) — must be pruned.
	if err := st.UpsertPendingBackgroundTerminal(store.PendingBackgroundTaskTerminal{
		ThreadID: "t1", TaskID: "tsk-rowless", Status: "completed",
		Source: "task_updated", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed rowless stash: %v", err)
	}

	// t2: still-running launch on another thread — must be untouched.
	handleToolStart(t, router, "t2", "bg-other", "Bash", map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 997"},
	})

	settled, err := router.SettleBackgroundLaunchesForSessionEnd("t1")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// agent-1, bg-nested, bg-top — the foreground row is exempt.
	if settled != 3 {
		t.Fatalf("settled = %d, want 3 (agent + nested + top-level)", settled)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	byCompletion := map[string]store.Item{}
	for _, d := range dones {
		byCompletion[d.CompletionOf] = d
	}
	for _, id := range []string{"agent-1", "bg-nested", "bg-top"} {
		sibling, ok := byCompletion[id]
		if !ok {
			t.Errorf("missing completion sibling for %s", id)
			continue
		}
		if sibling.Status != statusKilled {
			t.Errorf("sibling for %s: Status = %q, want %q", id, sibling.Status, statusKilled)
		}
	}
	if _, ok := byCompletion["fg-1"]; ok {
		t.Error("foreground launch must not get a completion sibling")
	}

	// Thread scoping: t2's launch untouched.
	if dones := findItemsByKind(t, st, "t2", itemKindBackgroundDone); len(dones) != 0 {
		t.Errorf("t2 must be untouched, got %+v", dones)
	}

	// Rowless stash pruned.
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-rowless"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if found {
		t.Error("rowless stash should be pruned by the session-end settle")
	}

	// Idempotent.
	settled2, err := router.SettleBackgroundLaunchesForSessionEnd("t1")
	if err != nil {
		t.Fatalf("settle (2nd): %v", err)
	}
	if settled2 != 0 {
		t.Errorf("2nd settle = %d, want 0", settled2)
	}
}

// TestSessionDiedSettlesBackgroundLaunches pins that an unexpected
// provider process death (EventSessionStatus{"error"}) settles the
// thread's backgrounded launches without waiting for an app restart.
func TestSessionDiedSettlesBackgroundLaunches(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	handleToolStart(t, router, "t1", "bg-dead", "Bash", map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 999"},
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSessionStatus, ThreadID: "t1",
		Content: "error", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle session death: %v", err)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 sibling after session death, got %+v", dones)
	}
	if dones[0].CompletionOf != "bg-dead" {
		t.Errorf("CompletionOf = %q, want bg-dead", dones[0].CompletionOf)
	}
	if dones[0].Status != statusKilled {
		t.Errorf("Status = %q, want %q", dones[0].Status, statusKilled)
	}
}

// TestRecoverOrphanedBackgroundTasksPrunesRowlessStashes pins the boot
// sweep's stash prune: rows the sweep did not consume belong to tasks
// with no launch row and would otherwise accumulate forever.
func TestRecoverOrphanedBackgroundTasksPrunesRowlessStashes(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := st.UpsertPendingBackgroundTerminal(store.PendingBackgroundTaskTerminal{
		ThreadID: "t1", TaskID: "tsk-dead", Status: "completed",
		Source: "task_updated", CreatedAt: 100,
	}); err != nil {
		t.Fatalf("seed stash: %v", err)
	}

	if _, err := router.RecoverOrphanedBackgroundTasks(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-dead"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if found {
		t.Error("boot sweep should prune rowless stash rows")
	}
}
