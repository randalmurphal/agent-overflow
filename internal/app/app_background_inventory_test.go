package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// registerInventoryTestSession puts a session entry on the thread so the
// inventory considers it. No subprocess: the inventory's domain is
// "threads with a live session", and what it reads out of them is the
// store and the triage router, neither of which needs a process.
func registerInventoryTestSession(app *App, threadID, providerName string) {
	app.sessionManager().put(threadID, session{
		Provider: providerName,
		Token:    "tok-" + threadID,
		Liveness: newSessionLiveness(time.Now()),
	})
}

// seedClaudeBackgroundTaskRow writes the persisted shape of a
// backgrounded Claude task: a running tool_call carrying the task_id
// that StopClaudeTask takes.
func seedClaudeBackgroundTaskRow(t *testing.T, app *App, threadID, itemID, taskID string, createdAt int64) {
	t.Helper()
	if _, err := app.store.AppendItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: pnpm run dev",
		ToolName:     "Bash",
		Meta:         `{"task_id":"` + taskID + `"}`,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}); err != nil {
		t.Fatalf("seed Claude background row: %v", err)
	}
}

// seedCodexSubagentLaunchRow writes the persisted shape of a live Codex
// spawn_agent launch. Its own row id is the handle StopCodexSubagent
// takes, which is why the inventory reports the item id as the stop id
// for this kind and the meta blob for the other two.
func seedCodexSubagentLaunchRow(t *testing.T, app *App, threadID, itemID string, createdAt int64) {
	t.Helper()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    threadID + ":0",
		ThreadID:  threadID,
		TurnIndex: 0,
		StartedAt: createdAt,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"running"}}}}`,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}); err != nil {
		t.Fatalf("seed Codex subagent launch: %v", err)
	}
}

// seedCodexUnifiedExecTracker drives the triage router the way a live
// Codex session would, producing a background task that exists ONLY in
// router memory — no items row, no turn row of its own. This is the
// source a SQLite-only inventory silently drops.
func seedCodexUnifiedExecTracker(t *testing.T, app *App, threadID, itemID, processID, command string) {
	t.Helper()
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: threadID + ":turn",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	startMeta, err := json.Marshal(map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "inProgress",
		"process_id":  processID,
		"toolName":    "command_execution",
		"input":       map[string]any{"command": command},
		"item": map[string]any{
			"id":        itemID,
			"type":      "commandExecution",
			"source":    "unifiedExecStartup",
			"status":    "inProgress",
			"processId": processID,
			"command":   command,
		},
	})
	if err != nil {
		t.Fatalf("marshal unified-exec meta: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID,
		ItemType: "commandExecution", TurnID: threadID + ":turn", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
}

// TestListRunningBackgroundWorkUnionsAllThreeSources is the reason the
// inventory reuses ListLiveBackgroundTasks instead of writing its own
// query. Live background work comes from three places — the store's
// background tool calls, the store's live Codex subagent launches, and
// the triage router's in-memory unified-exec trackers — and the third
// exists in no table at all. Each is seeded on its own thread here, so a
// leg dropped from the union takes a whole row with it.
func TestListRunningBackgroundWorkUnionsAllThreeSources(t *testing.T) {
	app, _ := setupE2EApp(t)

	claudeThread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("create Claude thread: %v", err)
	}
	subagentThread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatalf("create Codex subagent thread: %v", err)
	}
	execThread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatalf("create Codex exec thread: %v", err)
	}

	seedClaudeBackgroundTaskRow(t, app, claudeThread.ID, "bg-claude", "task-claude", 1000)
	seedCodexSubagentLaunchRow(t, app, subagentThread.ID, "spawn-codex", 2000)
	seedCodexUnifiedExecTracker(t, app, execThread.ID, "cmd-codex", "pid-codex", "pnpm run dev")

	registerInventoryTestSession(app, claudeThread.ID, string(provider.Claude))
	registerInventoryTestSession(app, subagentThread.ID, string(provider.Codex))
	registerInventoryTestSession(app, execThread.ID, string(provider.Codex))

	// The third source is invisible to SQLite. Pin that here so the
	// union assertion below cannot be satisfied by a store-only
	// implementation that happens to pass for the other two.
	storeOnly, err := app.store.ListLiveBackgroundTasks(execThread.ID, 0)
	if err != nil {
		t.Fatalf("store-only query: %v", err)
	}
	if len(storeOnly) != 0 {
		t.Fatalf("expected the unified-exec task to exist in no table, store returned %d rows", len(storeOnly))
	}

	inv, err := app.ListRunningBackgroundWork()
	if err != nil {
		t.Fatalf("ListRunningBackgroundWork: %v", err)
	}
	if len(inv.UnreadableThreadIDs) != 0 {
		t.Fatalf("unreadable threads = %v, want none", inv.UnreadableThreadIDs)
	}
	rows := inv.Rows
	if len(rows) != 3 {
		t.Fatalf("inventory row count = %d, want 3: %+v", len(rows), rows)
	}

	byThread := map[string]RunningBackgroundWork{}
	for _, row := range rows {
		byThread[row.ThreadID] = row
	}
	for _, want := range []struct {
		threadID string
		kind     string
		stopID   string
	}{
		{claudeThread.ID, BackgroundWorkClaudeTask, "task-claude"},
		{subagentThread.ID, BackgroundWorkCodexSubagent, "spawn-codex"},
		{execThread.ID, BackgroundWorkCodexTerminal, "pid-codex"},
	} {
		got, found := byThread[want.threadID]
		if !found {
			t.Fatalf("thread %s missing from the inventory: %+v", want.threadID, rows)
		}
		if got.Kind != want.kind {
			t.Errorf("thread %s kind = %q, want %q", want.threadID, got.Kind, want.kind)
		}
		if got.StopID != want.stopID {
			t.Errorf("thread %s stopId = %q, want %q", want.threadID, got.StopID, want.stopID)
		}
		if got.ThreadTitle == "" {
			t.Errorf("thread %s row carries no thread title; the inventory must name where work is running", want.threadID)
		}
		if got.StartedAt == 0 {
			t.Errorf("thread %s row carries no start time; \"since when\" is half the question", want.threadID)
		}
	}

	// Oldest first: an operator deciding what to shut down reads the
	// longest-running task at the top.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].StartedAt > rows[i].StartedAt {
			t.Fatalf("inventory is not ordered oldest-first: %+v", rows)
		}
	}
}

// TestListRunningBackgroundWorkSkipsThreadsWithoutALiveSession pins the
// domain. A `running` row on a thread with no session is residue the
// boot sweep settles, not work in progress — reporting it would invite a
// client to stop something that is not there.
func TestListRunningBackgroundWorkSkipsThreadsWithoutALiveSession(t *testing.T) {
	app, _ := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	seedClaudeBackgroundTaskRow(t, app, thread.ID, "bg-orphan", "task-orphan", 1000)

	inv, err := app.ListRunningBackgroundWork()
	if err != nil {
		t.Fatalf("ListRunningBackgroundWork: %v", err)
	}
	if len(inv.UnreadableThreadIDs) != 0 {
		t.Fatalf("unreadable threads = %v, want none", inv.UnreadableThreadIDs)
	}
	rows := inv.Rows
	if len(rows) != 0 {
		t.Fatalf("inventory reported %d rows for a thread with no session: %+v", len(rows), rows)
	}

	// Same store state, now with a session: the row appears. Without
	// this half the assertion above would also pass on a method that
	// returns nothing at all.
	registerInventoryTestSession(app, thread.ID, string(provider.Claude))
	inv, err = app.ListRunningBackgroundWork()
	if err != nil {
		t.Fatalf("ListRunningBackgroundWork (with session): %v", err)
	}
	rows = inv.Rows
	if len(rows) != 1 || rows[0].ItemID != "bg-orphan" {
		t.Fatalf("expected the launch once a session exists, got %+v", rows)
	}
}

// TestListRunningBackgroundWorkOmitsRecentlyCompletedWork holds the line
// the spec draws: the tray keeps a completed task on screen for a couple
// of seconds so the reader sees it finish, and that retention is a
// rendering choice, not history. An inventory reports what is running,
// so both halves of a settled pair — the terminal sibling row and the
// launch it settles — are absent.
func TestListRunningBackgroundWorkOmitsRecentlyCompletedWork(t *testing.T) {
	app, _ := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	now := time.Now().UnixMilli()
	seedClaudeBackgroundTaskRow(t, app, thread.ID, "bg-done", "task-done", now)
	if _, err := app.store.AppendItem(store.Item{
		ID:           "bg-done-completion",
		ThreadID:     thread.ID,
		TurnIndex:    1,
		Kind:         "tool_completion",
		Role:         "user",
		Status:       "completed",
		CompletionOf: "bg-done",
		IsBackground: true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed completion sibling: %v", err)
	}
	registerInventoryTestSession(app, thread.ID, string(provider.Claude))

	// The tray still shows the pair inside its retention window; that is
	// the behavior being distinguished from, not a bug.
	tray, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks: %v", err)
	}
	if len(tray) != 2 {
		t.Fatalf("tray row count = %d, want the launch plus its recent completion", len(tray))
	}

	inv, err := app.ListRunningBackgroundWork()
	if err != nil {
		t.Fatalf("ListRunningBackgroundWork: %v", err)
	}
	if len(inv.UnreadableThreadIDs) != 0 {
		t.Fatalf("unreadable threads = %v, want none", inv.UnreadableThreadIDs)
	}
	rows := inv.Rows
	if len(rows) != 0 {
		t.Fatalf("inventory reported finished work as running: %+v", rows)
	}
}

// TestStopThreadBackgroundWorkRoutesThroughTheProviderStopRPC drives the
// per-thread control end to end on a real (mocked) Claude session: the
// task it finds is stopped through StopClaudeTask, which puts the
// provider's own stop_task envelope on the wire and lands the killed
// sibling row. No second termination path exists for it to use.
func TestStopThreadBackgroundWorkRoutesThroughTheProviderStopRPC(t *testing.T) {
	app, bus := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	capture := newBackgroundScriptCapture(t)
	binary := writeClaudeBackgroundStopScript(t, t.TempDir(), capture.capturePath, []struct{ ToolUseID, TaskID string }{
		{ToolUseID: "tool-inv-1", TaskID: "task-inv-1"},
	})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "run a long job in background", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)
	waitUntilE2E(t, 3*time.Second, "background launch carries its task_id", func() bool {
		inv, err := app.ListRunningBackgroundWork()
		return err == nil && len(inv.Rows) == 1 && inv.Rows[0].StopID == "task-inv-1"
	})

	stopped, err := app.StopThreadBackgroundWork(thread.ID)
	if err != nil {
		t.Fatalf("StopThreadBackgroundWork: %v", err)
	}
	if stopped != 1 {
		t.Fatalf("stopped = %d, want 1", stopped)
	}

	// The provider saw the stop it would have seen from the per-row
	// button, in the exact wire shape.
	assertStopTaskWireShape(t, findStopTaskLine(t, capture.Lines(t)), "task-inv-1")

	waitUntilE2E(t, 3*time.Second, "killed completion sibling landed", func() bool {
		items, _ := app.store.ListItems(thread.ID)
		for _, item := range items {
			if item.CompletionOf == "tool-inv-1" && item.Status == "killed" {
				return true
			}
		}
		return false
	})

	// The session itself survives: stopping the work a thread left
	// running is a different control from releasing the thread.
	if _, live := app.sessionManager().get(thread.ID); !live {
		t.Fatal("StopThreadBackgroundWork closed the provider session; that is archive's job, not this one")
	}
}

// TestBackgroundWorkHandleSelectsTheProviderStopTarget pins the mapping
// a remote client reads off each row. The two id namespaces are not
// interchangeable — Claude stops by task id, Codex by launch id or PTY
// process id — so the row has to name which one it is carrying.
func TestBackgroundWorkHandleSelectsTheProviderStopTarget(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		item       store.Item
		wantKind   string
		wantStopID string
	}{
		{
			name:       "claude task",
			provider:   string(provider.Claude),
			item:       store.Item{ID: "tool-1", Meta: `{"task_id":"task-1"}`},
			wantKind:   BackgroundWorkClaudeTask,
			wantStopID: "task-1",
		},
		{
			name:       "claude-tui launch has no task id",
			provider:   string(provider.ClaudeTUI),
			item:       store.Item{ID: "tool-2", Meta: `{}`},
			wantKind:   BackgroundWorkClaudeTask,
			wantStopID: "",
		},
		{
			name:       "codex subagent stops by launch row id",
			provider:   string(provider.Codex),
			item:       store.Item{ID: "spawn-1", ToolName: "collab_agent", Meta: `{"input":{"tool":"spawn_agent"}}`},
			wantKind:   BackgroundWorkCodexSubagent,
			wantStopID: "spawn-1",
		},
		{
			name:       "codex terminal stops by process id",
			provider:   string(provider.Codex),
			item:       store.Item{ID: "cmd-1", ToolName: "command_execution", Meta: `{"process_id":"pid-1"}`},
			wantKind:   BackgroundWorkCodexTerminal,
			wantStopID: "pid-1",
		},
		{
			name:       "unnamed process yields no stop handle",
			provider:   string(provider.Codex),
			item:       store.Item{ID: "cmd-2", ToolName: "command_execution", Meta: `{"source":"unifiedExecStartup"}`},
			wantKind:   BackgroundWorkCodexTerminal,
			wantStopID: "",
		},
		{
			name:       "malformed meta is a missing handle, not a failure",
			provider:   string(provider.Claude),
			item:       store.Item{ID: "tool-3", Meta: `{not json`},
			wantKind:   BackgroundWorkClaudeTask,
			wantStopID: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, stopID := backgroundWorkHandle(tc.provider, tc.item)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if stopID != tc.wantStopID {
				t.Errorf("stopId = %q, want %q", stopID, tc.wantStopID)
			}
		})
	}
}

// TestStopBackgroundWorkItemRefusesAnUnknownKind guards the dispatcher's
// closed vocabulary. A kind nobody handles must surface as an error
// rather than silently reporting that nothing was running.
func TestStopBackgroundWorkItemRefusesAnUnknownKind(t *testing.T) {
	app := newTestAppWithStore(t)
	stopped, err := app.stopBackgroundWorkItem(RunningBackgroundWork{
		ThreadID: "t1",
		Kind:     "somethingElse",
		StopID:   "handle-1",
	})
	if err == nil {
		t.Fatal("expected an unknown background-work kind to error")
	}
	if stopped {
		t.Fatal("an unhandled kind reported a stop it never performed")
	}
}
