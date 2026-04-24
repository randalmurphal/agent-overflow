package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// --- Claude scenario 1: spawn → stop per-row → killed ---

// TestE2E_Claude_SpawnBackground_StopPerRow_KilledStatus drives the
// full path for a backgrounded Bash launched, then stopped by the user
// via the per-row Stop button. Verifies:
//
//   - The task_started meta event merges task_id into the launch row
//     so the tray can key by task_id.
//   - StopClaudeTask writes the exact wire-shape stop_task control_request.
//   - control_response(success) + task_updated(killed) round-trips back
//     and the triage pipeline persists a sibling tool_completion with
//     status=killed (distinct from errored).
//   - ListLiveBackgroundTasks surfaces the pair within the retention
//     window so the tray renders the Stopped badge.
func TestE2E_Claude_SpawnBackground_StopPerRow_KilledStatus(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	capture := newBackgroundScriptCapture(t)
	binary := writeClaudeBackgroundStopScript(t, t.TempDir(), capture.capturePath, []struct{ ToolUseID, TaskID string }{
		{ToolUseID: "tool-bg-1", TaskID: "task-bg-1"},
	})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "run long job in background", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// The backgrounded tool_use + task_started should have landed as a
	// tool_call row with is_background=true, carrying task_id in meta.
	waitUntilE2E(t, 3*time.Second, "backgrounded tool_call with task_id in meta", func() bool {
		row, found, err := app.store.GetThreadItem(thread.ID, "tool-bg-1")
		if err != nil || !found {
			return false
		}
		if !row.IsBackground || row.Status != "running" {
			return false
		}
		return strings.Contains(row.Meta, `"task_id"`) && strings.Contains(row.Meta, "task-bg-1")
	})

	// The tray query must see the live launch before the stop.
	liveBefore, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks (before stop): %v", err)
	}
	if len(liveBefore) != 1 || liveBefore[0].ID != "tool-bg-1" {
		t.Fatalf("expected 1 live backgrounded tool_call before stop, got %d: %+v", len(liveBefore), liveBefore)
	}

	// Fire the stop. The binding writes the stop_task line and waits
	// for the matching control_response; the fake emits both the
	// response and a follow-up task_updated{killed} so triage lands
	// the sibling row.
	if err := app.StopClaudeTask(thread.ID, "task-bg-1"); err != nil {
		t.Fatalf("StopClaudeTask: %v", err)
	}

	// Inspect the captured stdin: the stop_task envelope must match
	// the exact wire shape the spike verified on Claude CLI 2.1.112.
	// Using a generic JSON round-trip (rather than string matching)
	// decouples the assertion from key ordering.
	stopLine := findStopTaskLine(t, capture.Lines(t))
	assertStopTaskWireShape(t, stopLine, "task-bg-1")

	// Wait for the triage-side killed sibling row to land.
	var killedSibling store.Item
	waitUntilE2E(t, 3*time.Second, "killed tool_completion sibling", func() bool {
		items, _ := app.store.ListItems(thread.ID)
		for _, it := range items {
			if it.Kind == "tool_completion" && it.CompletionOf == "tool-bg-1" {
				killedSibling = it
				return it.Status == "killed"
			}
		}
		return false
	})
	if killedSibling.Status != "killed" {
		t.Fatalf("sibling status = %q, want killed (not errored)", killedSibling.Status)
	}
	if !killedSibling.IsBackground {
		t.Error("killed sibling is_background = false, want true")
	}

	// Tray renders BOTH the launch and the killed sibling within the
	// retention window: the launch row stays running (invariant), the
	// sibling shows the terminal state.
	liveAfter, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks (after stop): %v", err)
	}
	var sawLaunch, sawKilled bool
	for _, it := range liveAfter {
		if it.ID == "tool-bg-1" {
			sawLaunch = true
		}
		if it.CompletionOf == "tool-bg-1" && it.Status == "killed" {
			sawKilled = true
		}
	}
	if !sawLaunch || !sawKilled {
		t.Fatalf("tray query missing launch+killed pair; sawLaunch=%v sawKilled=%v items=%+v", sawLaunch, sawKilled, liveAfter)
	}

	_ = app.StopSession(thread.ID)
}

// --- Claude scenario 2: spawn multiple → stop-all ---

// TestE2E_Claude_SpawnMultiple_StopAll drives the Stop-all path from
// the tray: two backgrounded Bashes on the same thread, two
// StopClaudeTask calls, both end up status=killed. Each stop_task
// carries its own task_id — verifies the session's per-request
// correlation map isn't mixing them up.
func TestE2E_Claude_SpawnMultiple_StopAll(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	capture := newBackgroundScriptCapture(t)
	pairs := []struct{ ToolUseID, TaskID string }{
		{ToolUseID: "tool-bg-a", TaskID: "task-bg-a"},
		{ToolUseID: "tool-bg-b", TaskID: "task-bg-b"},
	}
	binary := writeClaudeBackgroundStopScript(t, t.TempDir(), capture.capturePath, pairs)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "run two background jobs", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// Both backgrounded launches must have landed.
	waitUntilE2E(t, 3*time.Second, "both launches persisted", func() bool {
		for _, id := range []string{"tool-bg-a", "tool-bg-b"} {
			row, found, err := app.store.GetThreadItem(thread.ID, id)
			if err != nil || !found {
				return false
			}
			if !row.IsBackground {
				return false
			}
		}
		return true
	})

	// Dispatch the two per-row stops (the frontend's Stop-all iterates
	// Claude rows and fires StopClaudeTask per task_id).
	if err := app.StopClaudeTask(thread.ID, "task-bg-a"); err != nil {
		t.Fatalf("StopClaudeTask(a): %v", err)
	}
	if err := app.StopClaudeTask(thread.ID, "task-bg-b"); err != nil {
		t.Fatalf("StopClaudeTask(b): %v", err)
	}

	// Each stop_task must have landed on the CLI with its own task_id
	// — the session's per-session request_id counter keeps them distinct
	// so a mixed-up response doesn't kill the wrong task.
	lines := capture.Lines(t)
	seenTaskIDs := map[string]bool{}
	seenRequestIDs := map[string]bool{}
	for _, line := range lines {
		if !strings.Contains(line, `"stop_task"`) {
			continue
		}
		var raw struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
				TaskID  string `json:"task_id"`
			} `json:"request"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("parse stop_task line %q: %v", line, err)
		}
		if raw.Type != "control_request" || raw.Request.Subtype != "stop_task" {
			t.Fatalf("unexpected stop_task shape %q", line)
		}
		seenTaskIDs[raw.Request.TaskID] = true
		if seenRequestIDs[raw.RequestID] {
			t.Fatalf("duplicate request_id %q across stop_task calls", raw.RequestID)
		}
		seenRequestIDs[raw.RequestID] = true
	}
	if !seenTaskIDs["task-bg-a"] || !seenTaskIDs["task-bg-b"] {
		t.Fatalf("expected distinct task_ids for both stops, got %v", seenTaskIDs)
	}

	// Both siblings must land with status=killed. Poll the store since
	// the task_updated arrival is async from the stop round-trip.
	waitUntilE2E(t, 3*time.Second, "both killed siblings persisted", func() bool {
		items, _ := app.store.ListItems(thread.ID)
		killed := 0
		for _, it := range items {
			if it.Kind == "tool_completion" && it.Status == "killed" {
				killed++
			}
		}
		return killed == 2
	})

	_ = app.StopSession(thread.ID)
}
