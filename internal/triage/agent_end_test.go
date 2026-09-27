package triage

// Agent-owned rows (docs/architecture/turn-lifecycle.md, §Agent-owned
// rows): a turn's end, a Stop and a session's end settle only the rows no
// agent owns; the agent's end settles its own. Every sequence is the
// router.Handle shape the parser produces, reusing the park helpers.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/pathlinks"
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
	if queued := strings.Join(queuedRows(router, "t1"), ","); queued != "complete:tu-a,complete:tu-a-shell" {
		t.Fatalf("queued rows = %s, want the agent's sibling, then the shell's sibling", queued)
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
	for _, stop := range findItemsByKind(t, st, "t1", itemKindBackgroundDone) {
		if stop.Status == store.ItemStatusParked {
			t.Fatalf("a killed agent parked: %+v", stop)
		}
	}
	if bells := findItemsByKind(t, st, "t1", itemKindNotification); len(bells) != 0 {
		t.Fatalf("an agent's stop rang a bell: %+v", bells)
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
	if got := parkCompletions(t, st, "t1")["tu-a"]; got.Status != statusKilled || decodeItemMetaMap(t, got.Meta)["status_source"] != "session_died" {
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

// An agent's end can land while a block stop's settle of the agent's
// text is in flight: before that settle flushed the stream's last
// window, or between its read of the row and its write. The row settles
// once, as a row settle writes it: with its whole text, its code spans
// and path refs, and a settle patch to the client. The end's rule decides
// its status, since the end's write lands first.
func TestAgentEndSettlesARowItsStopIsSettling(t *testing.T) {
	const head, tail = "Worker read src/foo.ts:", "\n```go\nfunc main() {}\n```"
	text := head + tail
	for _, end := range []struct {
		name         string
		run          func(t *testing.T, router *Router)
		status, want string
	}{
		{"completed", func(t *testing.T, router *Router) { parkStop(t, router, "t1", "tu-a", "task-a", "Done.", "u-end") }, statusCompleted, text},
		{"killed", func(t *testing.T, router *Router) { agentKilled(t, router, "t1", "tu-a", "task-a", "") }, statusErrored, stoppedSummary(text)},
	} {
		for _, order := range []struct {
			name string
			// deltas streams the text. The flush of a window writes path
			// refs of its own, so a stream whose first delta carries the
			// whole text leaves them to the settle.
			deltas []string
			// stopAndEnd runs the stop of the agent's text with the end
			// landing inside its settle.
			stopAndEnd func(t *testing.T, router *Router, turnIndex int, end func())
		}{
			{"before the stop's flush", []string{head, tail}, func(t *testing.T, router *Router, turnIndex int, end func()) {
				// The stop's two halves (settleStreamingTextAsync).
				itemID, taken := router.takeActiveTextBlock("t1", turnIndex, "tu-a", "")
				if !taken {
					t.Fatal("the agent's stream is not open")
				}
				end()
				if err := router.doSettleStreamingText("t1", "tu-a", itemID, statusCompleted, "", false, nil); err != nil {
					t.Fatal(err)
				}
			}},
			{"between the stop's read and write", []string{text}, func(t *testing.T, router *Router, _ int, end func()) {
				hold := newBlockSettleHold(t, router)
				if err := router.Handle(provider.ProviderEvent{
					Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "tu-a",
					Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-hold.held:
				case <-time.After(5 * time.Second):
					t.Fatal("the stop's settle never reached its write")
				}
				end()
				hold.release()
				router.WaitForPendingSettles()
			}},
		} {
			t.Run(end.name+"/"+order.name, func(t *testing.T) {
				router, st, emissions := newTestRouter(t)
				router.SetCodeSpanEnricher(func(string) json.RawMessage { return json.RawMessage(`{"spans":1}`) })
				createWorkspaceThread(t, st, "t1", "src/foo.ts")
				parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
				parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
				for _, delta := range order.deltas {
					parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ParentToolUseID: "tu-a", Content: delta})
				}
				texts := agentTexts(t, st, "t1", "tu-a")
				if len(texts) != 1 || texts[0].Summary != order.deltas[0] {
					t.Fatalf("agent text = %+v, want one row with its first delta, the rest in its flush window", texts)
				}
				id := texts[0].ID

				emissions.reset()
				order.stopAndEnd(t, router, texts[0].TurnIndex, func() {
					end.run(t, router)
					if !router.hasActiveStreamingItem("t1") || scopeStreams(router, "t1", "tu-a") != 1 {
						t.Fatal("the end released the count the stop's settle holds until it finishes")
					}
				})

				got := mustItem(t, st, "t1", id)
				if got.Status != end.status || got.Summary != end.want {
					t.Fatalf("agent text = %s %q, want %s %q", got.Status, got.Summary, end.status, end.want)
				}
				var meta map[string]json.RawMessage
				if err := json.Unmarshal([]byte(got.Meta), &meta); err != nil {
					t.Fatalf("agent text meta %q: %v", got.Meta, err)
				}
				if meta[codeSpansMetaKey] == nil || !strings.Contains(string(meta[pathlinks.MetaKey]), "src/foo.ts") {
					t.Fatalf("agent text meta = %s, want its code spans and path refs", got.Meta)
				}
				var patches int
				for _, evt := range itemStreamEventsFor(t, emissions, id) {
					switch evt.Action {
					case itemStreamActionPatch:
						patches++
						if evt.Patch.Status == nil || *evt.Patch.Status != end.status {
							t.Fatalf("settle patch = %+v, want status %s", evt.Patch.ItemPatchFields, end.status)
						}
					case itemStreamActionUpsert:
						t.Fatalf("agent text sent as a %s upsert, want a settle patch", evt.Item.Status)
					}
				}
				if patches != 1 {
					t.Fatalf("agent text sent %d settle patches, want 1", patches)
				}
				if n := scopeStreams(router, "t1", "tu-a"); n != 0 {
					t.Fatalf("the agent's stream is still counted: %d", n)
				}
				if open := openRows(t, st, "t1"); len(open) != 0 {
					t.Fatalf("rows left open: %v", open)
				}
			})
		}
	}
}

// createWorkspaceThread creates thread id in a temporary workspace that
// holds the files at paths, so a path to one of them validates.
func createWorkspaceThread(t *testing.T, st *store.Store, id string, paths ...string) {
	t.Helper()
	root := t.TempDir()
	for _, path := range paths {
		file := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID: id, ProjectID: triageTestProjectID, Title: "Test", Provider: "claude",
		WorkspacePath: root, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
}
