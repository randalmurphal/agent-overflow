package triage

// Agent-owned rows (docs/architecture/turn-lifecycle.md, §Agent-owned
// rows): a turn's end, a Stop and a session's end settle only the rows no
// agent owns; the agent's end settles its own. Every sequence is the
// router.Handle shape the parser produces, reusing the park helpers.

import (
	"database/sql"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// scopeStreams is the count of open streams the router holds in scope.
func scopeStreams(r *Router, threadID, scope string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return 0
	}
	return st.streamingScopeCounts[scope]
}

func queuedRows(r *Router, threadID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	if st := r.threadStateIfPresent(threadID); st != nil {
		for _, queued := range st.interruptQueue {
			ids = append(ids, queued.item.ID)
		}
	}
	return ids
}

func mustItem(t *testing.T, st *store.Store, threadID, id string) store.Item {
	t.Helper()
	item, found, err := st.GetThreadItem(threadID, id)
	if err != nil || !found {
		t.Fatalf("item %s: found=%v err=%v", id, found, err)
	}
	return item
}

// isStopped and isInterrupted report a summary already carrying the
// suffix; the suffix functions are idempotent on exactly those.
func isStopped(summary string) bool { return summary != "" && stoppedSummary(summary) == summary }

func isInterrupted(summary string) bool {
	return summary != "" && interruptedSummary(summary) == summary
}

func agentTexts(t *testing.T, st *store.Store, threadID, scope string) []store.Item {
	t.Helper()
	var out []store.Item
	for _, it := range findItemsByKind(t, st, threadID, itemKindAssistantText) {
		if it.ParentID == scope {
			out = append(out, it)
		}
	}
	return out
}

// openRows lists the rows left running or streaming, except background
// launches, which a completion sibling settles.
func openRows(t *testing.T, st *store.Store, threadID string) []string {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var open []string
	for _, it := range items {
		if (it.Status == statusRunning || it.Status == statusStreaming) && !(it.Kind == itemKindToolCall && it.IsBackground) {
			open = append(open, it.ID+":"+it.Status)
		}
	}
	return open
}

func agentRead(t *testing.T, router *Router, threadID, id, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: id, ItemType: "Read", ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"toolName": "Read", "input": map[string]any{"file_path": "README.md"}}),
	})
}

func agentKilled(t *testing.T, router *Router, threadID, launchID, taskID, parentID string) {
	t.Helper()
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: threadID, ItemID: launchID, ParentToolUseID: parentID,
		Meta: parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": launchID, "status": "killed", "source": "task_updated"}),
	})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: threadID, ItemID: launchID, ParentToolUseID: parentID,
		Content: "stopped",
		Meta:    parkMeta(t, map[string]any{"task_id": taskID, "tool_use_id": launchID, "status": "stopped", "uuid": "stop-" + taskID}),
	})
}

func TestParentTurnEndLeavesAgentRowsForTheAgentsEnd(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	agentRead(t, router, "t1", "tu-a-read", "tu-a")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: "Half"})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})

	if got := mustItem(t, st, "t1", "tu-a-read"); got.Status != statusRunning {
		t.Fatalf("the parent's end settled the agent's tool call: %q %q", got.Status, got.Summary)
	}
	texts := agentTexts(t, st, "t1", "tu-a")
	if len(texts) != 1 || texts[0].Status != statusStreaming {
		t.Fatalf("the parent's end settled the agent's text: %+v", texts)
	}
	if n := scopeStreams(router, "t1", "tu-a"); n != 1 {
		t.Fatalf("the parent's end dropped the agent's stream: count %d", n)
	}

	// The next turn starting leaves the agent's stream alone too.
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 2})
	if n := scopeStreams(router, "t1", "tu-a"); n != 1 {
		t.Fatalf("the next turn's start dropped the agent's stream: count %d", n)
	}
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: " done"})
	if texts := agentTexts(t, st, "t1", "tu-a"); len(texts) != 1 {
		t.Fatalf("the agent's text split into %d rows: %+v", len(texts), texts)
	}

	// The agent finishes with its Read unresolved and its text open.
	parkStop(t, router, "t1", "tu-a", "task-a", "Read it.", "u-final")
	if got := parkCompletions(t, st, "t1")["tu-a"]; got.Status != statusCompleted {
		t.Fatalf("agent sibling = %+v, want completed", got)
	}
	if got := mustItem(t, st, "t1", "tu-a-read"); got.Status != statusErrored || !strings.HasSuffix(got.Summary, forceCloseSuffix) {
		t.Fatalf("the agent's end left its tool call %q %q, want errored and unresolved", got.Status, got.Summary)
	}
	router.WaitForPendingSettles()
	texts = agentTexts(t, st, "t1", "tu-a")
	if len(texts) != 1 || texts[0].Status != statusCompleted || texts[0].Summary != "Half done" {
		t.Fatalf("the agent's end left its text %+v, want completed \"Half done\"", texts)
	}
	if n := scopeStreams(router, "t1", "tu-a"); n != 0 {
		t.Fatalf("the agent's end kept its stream counted: %d", n)
	}
	if open := openRows(t, st, "t1"); len(open) != 0 {
		t.Fatalf("rows left open: %v", open)
	}
}

func TestStopKillsAnEarlierTurnsAgentWithEveryRowStopped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})

	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 2})
	parkLaunchShell(t, router, "t1", "tu-a-shell", "task-a-shell", "tu-a")
	agentRead(t, router, "t1", "tu-a-read", "tu-a")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: "Worker is reading"})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Waiting on the worker"})

	// The interrupt's kill frames, in the CLI's order: the agent, then
	// the shell it owns. The main text streams, so the agent's end waits
	// behind it, and so does the shell's behind the agent's own stream.
	agentKilled(t, router, "t1", "tu-a", "task-a", "")
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "tu-a-shell", ParentToolUseID: "tu-a",
		Meta: parkMeta(t, map[string]any{"task_id": "task-a-shell", "tool_use_id": "tu-a-shell", "parent_tool_use_id": "tu-a", "status": "killed", "source": "task_updated"}),
	})
	if queued := strings.Join(queuedRows(router, "t1"), ","); queued != "complete:tu-a,task-notification:task-a:stop-task-a,complete:tu-a-shell" {
		t.Fatalf("queued rows = %s, want the agent's sibling and bell, then the shell's sibling", queued)
	}
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: &provider.TruncatedTurnCompleteMeta{}})
	router.WaitForPendingSettles()

	completions := parkCompletions(t, st, "t1")
	for _, launch := range []string{"tu-a", "tu-a-shell"} {
		if got := completions[launch]; got.Status != statusKilled {
			t.Fatalf("%s sibling = %q %q, want killed", launch, got.Status, got.Summary)
		}
	}
	for _, id := range []string{"tu-a-read"} {
		if got := mustItem(t, st, "t1", id); got.Status != statusErrored || !isStopped(got.Summary) {
			t.Fatalf("%s = %q %q, want errored and stopped", id, got.Status, got.Summary)
		}
	}
	texts := agentTexts(t, st, "t1", "tu-a")
	if len(texts) != 1 || texts[0].Status != statusErrored || !isStopped(texts[0].Summary) {
		t.Fatalf("agent text = %+v, want errored and stopped", texts)
	}
	for _, bell := range findItemsByKind(t, st, "t1", itemKindNotification) {
		if strings.Contains(bell.Meta, notificationKindParkedAgent) {
			t.Fatalf("a killed agent parked: %+v", bell)
		}
	}
	if open := openRows(t, st, "t1"); len(open) != 0 {
		t.Fatalf("rows left open: %v", open)
	}
	if queued := queuedRows(router, "t1"); len(queued) != 0 {
		t.Fatalf("rows left queued: %v", queued)
	}
	if n := scopeStreams(router, "t1", "tu-a"); n != 0 {
		t.Fatalf("the killed agent's stream is still counted: %d", n)
	}
}

// An agent's failed or stopped report is its end even while it owns a
// running shell: only a completed report can be a pause. Both places
// that decide a pause read the typed status.
func TestAnEndingReportOfAnAgentWithALiveShellIsNotAPause(t *testing.T) {
	hostExit := func(status string) provider.ProviderEvent {
		return provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: "tu-a",
			Meta: parkMeta(t, map[string]any{"task_id": "task-a", "tool_use_id": "tu-a", "status": status, "source": "task_updated"})}
	}
	notification := func(status string) provider.ProviderEvent {
		return provider.ProviderEvent{Kind: provider.EventBackgroundTaskNotification, ItemID: "tu-a", Content: "ended",
			Meta: parkMeta(t, map[string]any{"task_id": "task-a", "tool_use_id": "tu-a", "status": status, "uuid": status + "-a"})}
	}
	for _, tc := range []struct {
		name   string
		events []provider.ProviderEvent
		want   string
	}{
		{"TaskOutput failed", []provider.ProviderEvent{hostExit("failed"), {
			Kind: provider.EventBackgroundTaskTerminal, ItemID: "tu-a",
			Meta: parkMeta(t, map[string]any{"task_id": "task-a", "tool_use_id": "tu-a", "status": "failed", "source": "task_output"}),
		}}, statusErrored},
		{"notification failed", []provider.ProviderEvent{hostExit("failed"), notification("failed")}, statusErrored},
		{"notification stopped", []provider.ProviderEvent{hostExit("stopped"), notification("stopped")}, statusKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
			parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
			parkLaunchShell(t, router, "t1", "tu-a-shell", "task-a-shell", "tu-a")
			for _, evt := range tc.events {
				evt.ThreadID = "t1"
				parkHandle(t, router, evt)
			}
			if got := parkCompletions(t, st, "t1")["tu-a"]; got.Status != tc.want {
				t.Fatalf("an ending agent with a live shell read as paused: sibling %q %q, want %s", got.ID, got.Status, tc.want)
			}
		})
	}
}

// A session's end persists the rows still queued behind an agent's
// stream, and its settle ends the agent with every row under it.
func TestSessionEndPersistsQueuedAgentRowsAndEndsTheAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	parkLaunchShell(t, router, "t1", "tu-a-shell", "task-a-shell", "tu-a")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: "Worker is reading"})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "tu-a-shell", ParentToolUseID: "tu-a",
		Meta: parkMeta(t, map[string]any{"task_id": "task-a-shell", "tool_use_id": "tu-a-shell", "parent_tool_use_id": "tu-a", "status": "killed", "source": "task_updated"}),
	})
	if queued := queuedRows(router, "t1"); len(queued) != 1 {
		t.Fatalf("queued rows = %v, want the shell's sibling", queued)
	}

	router.CleanupThread("t1")
	if got := parkCompletions(t, st, "t1")["tu-a-shell"]; got.Status != statusKilled {
		t.Fatalf("the session's end lost the queued shell sibling: %+v", got)
	}
	if _, err := router.SettleBackgroundLaunchesForSessionEnd("t1"); err != nil {
		t.Fatalf("session-end settle: %v", err)
	}
	if got := parkCompletions(t, st, "t1")["tu-a"]; got.Status != statusKilled || completionStatusSource(got.Meta) != "session_died" {
		t.Fatalf("agent sibling = %+v, want killed by the session's end", got)
	}
	texts := agentTexts(t, st, "t1", "tu-a")
	if len(texts) != 1 || texts[0].Status != statusErrored || !isInterrupted(texts[0].Summary) {
		t.Fatalf("agent text = %+v, want errored and interrupted", texts)
	}
	if open := openRows(t, st, "t1"); len(open) != 0 {
		t.Fatalf("rows left open: %v", open)
	}
}

// A session death whose settle fails tells the user in the thread.
func TestSessionDiedReportsAFailedBackgroundSettle(t *testing.T) {
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	router := NewRouter(st, func(eventchan.Channel, any) {})
	t.Cleanup(router.DrainWireItemRefresh)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_sibling BEFORE INSERT ON items WHEN NEW.completion_of = 'tu-a' BEGIN SELECT RAISE(ABORT, 'injected sibling write failure'); END`); err != nil {
		t.Fatal(err)
	}
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: "t1", Content: "error"})

	var reported bool
	for _, row := range findItemsByKind(t, st, "t1", ItemKindError) {
		if strings.HasPrefix(row.Summary, "Background work from the ended session could not all be settled") &&
			strings.Contains(row.Summary, "injected sibling write failure") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the failed settle reached no error row: %+v", findItemsByKind(t, st, "t1", ItemKindError))
	}
}

// Rows queued in one scope persist once that scope's streams settle,
// while another scope still streams.
func TestAQueuedRowWaitsOnlyForItsOwnScope(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	parkLaunchShell(t, router, "t1", "tu-a-shell", "task-a-shell", "tu-a")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: "Worker is reading"})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "Main is still talking"})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "tu-a-shell", ParentToolUseID: "tu-a",
		Meta: parkMeta(t, map[string]any{"task_id": "task-a-shell", "tool_use_id": "tu-a-shell", "parent_tool_use_id": "tu-a", "status": "killed", "source": "task_updated"}),
	})
	if queued := queuedRows(router, "t1"); len(queued) != 1 {
		t.Fatalf("queued rows = %v, want the shell's sibling", queued)
	}
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "tu-a"})
	router.WaitForPendingSettles()
	if got := parkCompletions(t, st, "t1")["tu-a-shell"]; got.Status != statusKilled {
		t.Fatalf("the shell's sibling waited on the main stream: %+v (queued %v)", got, queuedRows(router, "t1"))
	}
	if n := scopeStreams(router, "t1", ""); n != 1 {
		t.Fatalf("main stream count = %d, want 1", n)
	}
}

// An agent's end settles its own streams without a stream stop, so it
// persists the rows that were queued behind them itself.
func TestAnAgentsEndPersistsTheRowsQueuedBehindItsStreams(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	parkLaunchShell(t, router, "t1", "tu-a-shell", "task-a-shell", "tu-a")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: "Worker is reading"})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "tu-a-shell", ParentToolUseID: "tu-a",
		Meta: parkMeta(t, map[string]any{"task_id": "task-a-shell", "tool_use_id": "tu-a-shell", "parent_tool_use_id": "tu-a", "status": "killed", "source": "task_updated"}),
	})
	agentKilled(t, router, "t1", "tu-a", "task-a", "")
	if queued := queuedRows(router, "t1"); len(queued) != 0 {
		t.Fatalf("rows left queued behind the ended agent's stream: %v", queued)
	}
	completions := parkCompletions(t, st, "t1")
	for _, launch := range []string{"tu-a", "tu-a-shell"} {
		if got := completions[launch]; got.Status != statusKilled {
			t.Fatalf("%s sibling = %q %q, want killed", launch, got.Status, got.Summary)
		}
	}
	if open := openRows(t, st, "t1"); len(open) != 0 {
		t.Fatalf("rows left open: %v", open)
	}
}
