package app

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// registerInventoryTestSession puts a session entry on the thread, as a
// session end that counts the thread's work finds it. No subprocess: what
// the count reads is the store and the triage router, neither of which
// needs a process.
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
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: itemID, TurnID: "child-turn",
		Meta: json.RawMessage(`{"agent_path":"child-1","status":"running"}`), Timestamp: time.UnixMilli(createdAt),
	}); err != nil {
		t.Fatal(err)
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

// TestRunningBackgroundTaskCountUnionsAllThreeSources is the reason the
// count reuses ListLiveBackgroundTasks instead of writing its own query.
// Live background work comes from three places — the store's background
// tool calls, the router's live Codex agents, and the triage router's
// in-memory unified-exec trackers — and the third exists in no table at
// all. Each is seeded on its own thread here, so a leg dropped from the
// union takes that thread's count to zero.
func TestRunningBackgroundTaskCountUnionsAllThreeSources(t *testing.T) {
	t.Parallel()
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

	for _, threadID := range []string{claudeThread.ID, subagentThread.ID, execThread.ID} {
		count, err := app.runningBackgroundTaskCount(threadID)
		if err != nil {
			t.Fatalf("runningBackgroundTaskCount(%s): %v", threadID, err)
		}
		if count != 1 {
			t.Fatalf("thread %s running count = %d, want 1", threadID, count)
		}
	}
}

// TestRunningBackgroundTaskCountOmitsRecentlyCompletedWork: the tray keeps a
// completed task on screen for a couple of seconds so the reader sees it
// finish, and that retention is a rendering choice, not running work. Both
// halves of a settled pair — the terminal sibling row and the launch it
// settles — are left out of the count.
func TestRunningBackgroundTaskCountOmitsRecentlyCompletedWork(t *testing.T) {
	t.Parallel()
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

	count, err := app.runningBackgroundTaskCount(thread.ID)
	if err != nil {
		t.Fatalf("runningBackgroundTaskCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("finished work counted as running: %d", count)
	}
}
