package triage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// ---------------------------------------------------------------------
// Sidechain transcript fixtures.
//
// Hand-written rows under t.TempDir(), never a copy of a real transcript
// and never a read of a real ~/.claude (internal/AGENTS.md §Testing).
// t.TempDir() is inside the output-file allow-list, which is what lets a
// notification name one of these as its `output_file`.
// ---------------------------------------------------------------------

func writeSubagentTranscript(t *testing.T, name string, rows ...map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	var body []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal transcript row: %v", err)
		}
		body = append(body, encoded...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write transcript %s: %v", path, err)
	}
	return path
}

func sidechainStamp(seconds int) string {
	return fmt.Sprintf("2026-01-01T00:00:%02d.000Z", seconds)
}

func sidechainPromptRow(uuid, text string, seconds int) map[string]any {
	return map[string]any{
		"type": "user", "uuid": uuid, "parentUuid": nil, "isSidechain": true,
		"timestamp": sidechainStamp(seconds),
		"message":   map[string]any{"role": "user", "content": text},
	}
}

func sidechainTextRow(uuid, parent, messageID, text string, seconds int) map[string]any {
	return map[string]any{
		"type": "assistant", "uuid": uuid, "parentUuid": parent, "isSidechain": true,
		"timestamp": sidechainStamp(seconds),
		"message": map[string]any{
			"role": "assistant", "id": messageID, "model": "claude-test-1",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
}

func sidechainToolUseRow(uuid, parent, messageID, toolUseID, toolName string, seconds int) map[string]any {
	return map[string]any{
		"type": "assistant", "uuid": uuid, "parentUuid": parent, "isSidechain": true,
		"timestamp": sidechainStamp(seconds),
		"message": map[string]any{
			"role": "assistant", "id": messageID, "model": "claude-test-1",
			"content": []any{map[string]any{
				"type": "tool_use", "id": toolUseID, "name": toolName,
				"input": map[string]any{"file_path": "/repo/a.go"},
			}},
		},
	}
}

func sidechainToolResultRow(uuid, parent, toolUseID, content string, seconds int) map[string]any {
	return map[string]any{
		"type": "user", "uuid": uuid, "parentUuid": parent, "isSidechain": true,
		"timestamp": sidechainStamp(seconds),
		"message": map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": toolUseID, "content": content,
		}}},
	}
}

// fullAgentTranscript is the shared fixture: text, a tool call, its
// result, and a closing text — four rows the live stream may have
// delivered any prefix of.
func fullAgentTranscript(t *testing.T, name string) string {
	t.Helper()
	return writeSubagentTranscript(t, name,
		sidechainPromptRow("s1", "the task prompt", 1),
		sidechainTextRow("s2", "s1", "msg_open", "reading the file first", 2),
		sidechainToolUseRow("s3", "s2", "msg_tool", "toolu_sub_read", "Read", 3),
		sidechainToolResultRow("s4", "s3", "toolu_sub_read", "package main", 4),
		sidechainTextRow("s5", "s4", "msg_close", "done: it is a main package", 5),
	)
}

// ---------------------------------------------------------------------
// Router-side helpers.
// ---------------------------------------------------------------------

// startAgentLaunch drives the wire sequence an async `local_agent`
// launch produces: the Agent tool_use, the meta-only task_started that
// binds the task id, and the §E5 ack that marks it backgrounded.
func startAgentLaunch(t *testing.T, router *Router, threadID, itemID, parentID, taskID string) {
	t.Helper()
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input":    map[string]any{"description": "review the file"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID,
		ItemType: "Agent", Meta: startMeta, ParentToolUseID: parentID, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent launch start: %v", err)
	}
	taskStarted, _ := json.Marshal(map[string]any{"task_id": taskID})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID,
		Meta: taskStarted, ParentToolUseID: parentID, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent task_started: %v", err)
	}
	ack, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: itemID,
		Content: "Async agent launched successfully.", Meta: ack,
		ParentToolUseID: parentID, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("agent async ack: %v", err)
	}
}

// stashAgentTerminal is the `task_updated` terminal that waits in the
// stash until the notification drains it into a completion sibling.
func stashAgentTerminal(t *testing.T, router *Router, threadID, itemID, taskID string) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{
		"task_id": taskID, "tool_use_id": itemID, "status": "completed", "source": "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: threadID,
		Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}
}

// notifyAgent fires the `system/task_notification` envelope, optionally
// naming an output file and carrying the run's final usage.
func notifyAgent(t *testing.T, router *Router, threadID, itemID, taskID, outputFile string, usage map[string]any) {
	t.Helper()
	fields := map[string]any{
		"task_id": taskID, "tool_use_id": itemID, "status": "completed", "source": "task_notification",
		"uuid": "notif-" + taskID,
	}
	if outputFile != "" {
		fields["output_file"] = outputFile
	}
	if usage != nil {
		fields["usage"] = usage
	}
	meta, _ := json.Marshal(fields)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: threadID, ItemID: itemID,
		Meta: meta, Content: `Agent "review the file" completed`, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification: %v", err)
	}
}

// deliverSubagentBlock replays what the live wire delivers for one whole
// subagent assistant block (the CLI emits no partial stream events for a
// sidechain message — parse_assistant.go's appendRecoveredBlockEvent).
func deliverSubagentBlock(t *testing.T, router *Router, threadID, scope, providerItemID, blockType, content string) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"blockType": blockType, "index": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: threadID, ItemID: providerItemID,
		Content: content, ContentPresent: true, Meta: meta,
		ParentToolUseID: scope, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent block stop %s: %v", providerItemID, err)
	}
	router.WaitForPendingSettles()
}

func childrenOfLaunch(t *testing.T, st *store.Store, threadID, launchID string, turnIndex int) []store.Item {
	t.Helper()
	items, err := st.ListTurnItemsSansPayload(threadID, turnIndex)
	if err != nil {
		t.Fatalf("list turn %d items: %v", turnIndex, err)
	}
	var out []store.Item
	for _, item := range items {
		if item.ParentID == launchID {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemIndex < out[j].ItemIndex })
	return out
}

func childIDs(items []store.Item) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

// ---------------------------------------------------------------------
// Q11: the bell is top-level only.
// ---------------------------------------------------------------------

// A top-level agent's notification IS the thread's bell, so it persists.
func TestTaskNotificationWritesTheBellForATopLevelLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-top", "", "task-top")
	stashAgentTerminal(t, router, "t1", "agent-top", "task-top")
	notifyAgent(t, router, "t1", "agent-top", "task-top", "", nil)

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification row for a top-level launch, got %d", len(notifications))
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 1 {
		t.Fatalf("expected 1 completion sibling, got %d", len(dones))
	}
}

// A NESTED agent's completion updates its card silently: no bell row,
// but every other effect of the notification still happens — the stash
// drains into a sibling and the sibling is enriched with the output
// file's payload and its output state.
func TestTaskNotificationWritesNoBellForANestedLaunchButStillEnrichesTheSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-outer", "", "task-outer")
	startAgentLaunch(t, router, "t1", "agent-inner", "agent-outer", "task-inner")

	launch, ok, err := st.GetThreadItem("t1", "agent-inner")
	if err != nil || !ok {
		t.Fatalf("lookup nested launch: ok=%v err=%v", ok, err)
	}
	if launch.ParentID != "agent-outer" {
		t.Fatalf("nested launch parent = %q, want agent-outer", launch.ParentID)
	}

	stashAgentTerminal(t, router, "t1", "agent-inner", "task-inner")
	transcript := fullAgentTranscript(t, "agent-inner.jsonl")
	notifyAgent(t, router, "t1", "agent-inner", "task-inner", transcript, nil)

	for _, row := range findItemsByKind(t, st, "t1", itemKindNotification) {
		meta := decodeItemMetaMap(t, row.Meta)
		if meta["task_id"] == "task-inner" {
			t.Fatalf("a nested launch must not write a notification bell, got row %s", row.ID)
		}
	}

	sibling, ok, err := st.GetThreadItem("t1", ToolCompletionID("agent-inner"))
	if err != nil || !ok {
		t.Fatalf("lookup nested completion sibling: ok=%v err=%v", ok, err)
	}
	if sibling.PayloadID == "" {
		t.Fatal("nested sibling lost its output-file payload")
	}
	if state := decodeItemMetaMap(t, sibling.Meta)["notification_output_state"]; state != "loaded" {
		t.Fatalf("nested sibling notification_output_state = %v, want loaded", state)
	}
}

// A watch task is exempt at any depth. Its notification rows are not a
// bell — they ARE its event history (claude-wire.md §E7), so suppressing
// a nested one would delete content no other row carries.
func TestTaskNotificationKeepsEveryWatchTaskRowEvenWhenNested(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-outer", "", "task-outer")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "Read output file for task b1"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "nested-monitor",
		ItemType: "Bash", Meta: startMeta, ParentToolUseID: "agent-outer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("nested monitor start: %v", err)
	}
	taskStarted, _ := json.Marshal(map[string]any{"task_id": "task-monitor"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "nested-monitor",
		Meta: taskStarted, ParentToolUseID: "agent-outer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("nested monitor task_started: %v", err)
	}
	ack, _ := json.Marshal(map[string]any{"is_background": true, "watch_task": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "nested-monitor",
		Meta: ack, ParentToolUseID: "agent-outer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("nested monitor launch ack: %v", err)
	}

	sendTaskNotification(t, router, "t1", "nested-monitor", "task-monitor", "uuid-1", "Monitor event 1")
	sendTaskNotification(t, router, "t1", "nested-monitor", "task-monitor", "uuid-2", "Monitor event 2")

	watchRows := 0
	for _, row := range findItemsByKind(t, st, "t1", itemKindNotification) {
		if decodeItemMetaMap(t, row.Meta)["watch_task"] == true {
			watchRows++
		}
	}
	if watchRows != 2 {
		t.Fatalf("expected both nested watch-task notification rows, got %d", watchRows)
	}
}

// ---------------------------------------------------------------------
// The notification's `usage` is the run's authoritative final numbers.
// ---------------------------------------------------------------------

func TestTaskNotificationUsageLandsOnTheLaunchRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-usage", "", "task-usage")

	// A live tick first: its activity line is what the card renders
	// while the agent runs, and it must NOT survive the terminal.
	tick, _ := json.Marshal(provider.SubagentProgressMeta{
		TaskID: "task-usage", ToolUses: 2, TotalTokens: 900, DurationMs: 1000,
		Activity: "Reading a.go", LastToolName: "Read",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentProgress, ThreadID: "t1", ItemID: "agent-usage",
		Meta: tick, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_progress: %v", err)
	}

	notifyAgent(t, router, "t1", "agent-usage", "task-usage", "", map[string]any{
		"totalTokens": 4321, "toolUses": 7, "durationMs": 42000,
	})

	launch, ok, err := st.GetThreadItem("t1", "agent-usage")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	progress := persistedSubagentProgress(launch.Meta)
	if progress.TotalTokens != 4321 || progress.ToolUses != 7 || progress.DurationMs != 42000 {
		t.Fatalf("persisted progress = %+v, want the notification's numbers", progress)
	}
	if progress.Activity != "" {
		t.Fatalf("a settled launch must carry no live activity line, got %q", progress.Activity)
	}
	if progress.LastToolName != "Read" {
		t.Fatalf("final progress lost the live tick's last tool: %+v", progress)
	}
}

// ---------------------------------------------------------------------
// Transcript backfill.
// ---------------------------------------------------------------------

// An agent launched ASYNC streams its whole sidechain, so by the time
// its notification arrives every row is already on the thread and the
// backfill writes nothing. This is the common path: it must not
// duplicate a single row.
func TestSubagentTranscriptBackfillWritesNothingWhenTheLiveStreamDeliveredEverything(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-async", "", "task-async")
	deliverSubagentBlock(t, router, "t1", "agent-async", "msg_open#0", "text", "reading the file first")
	toolStart, _ := json.Marshal(map[string]any{
		"toolName": "Read", "input": map[string]any{"file_path": "/repo/a.go"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_sub_read",
		ItemType: "Read", Meta: toolStart, ParentToolUseID: "agent-async", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("nested tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "toolu_sub_read",
		Content: "package main", ParentToolUseID: "agent-async", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("nested tool complete: %v", err)
	}
	deliverSubagentBlock(t, router, "t1", "agent-async", "msg_close#0", "text", "done: it is a main package")

	before := childIDs(childrenOfLaunch(t, st, "t1", "agent-async", 0))

	stashAgentTerminal(t, router, "t1", "agent-async", "task-async")
	notifyAgent(t, router, "t1", "agent-async", "task-async", fullAgentTranscript(t, "agent-async.jsonl"), nil)
	router.WaitForPendingSettles()

	after := childIDs(childrenOfLaunch(t, st, "t1", "agent-async", 0))
	if len(after) != len(before) {
		t.Fatalf("backfill added rows to a fully streamed agent:\nbefore %v\nafter  %v", before, after)
	}
}

// An agent BACKGROUNDED mid-flight streams nothing further: its work
// exists only in the sidechain JSONL the notification names. Exactly the
// rows after the cut are replayed, under the launch, on the launch's own
// turn, with the ids the live path would have minted — and a second
// notification adds nothing.
func TestSubagentTranscriptBackfillReplaysOnlyTheRowsAfterTheCut(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-cut", "", "task-cut")
	// Everything the live stream delivered before the agent was
	// backgrounded: one whole assistant text block.
	deliverSubagentBlock(t, router, "t1", "agent-cut", "msg_open#0", "text", "reading the file first")

	// The turn moves on while the agent runs in the background. The
	// backfilled rows must still land on the LAUNCH's turn (invariant 10).
	seedOpenTurn(t, router, st, "t1", 1)

	stashAgentTerminal(t, router, "t1", "agent-cut", "task-cut")
	notifyAgent(t, router, "t1", "agent-cut", "task-cut", fullAgentTranscript(t, "agent-cut.jsonl"), nil)
	router.WaitForPendingSettles()

	children := childrenOfLaunch(t, st, "t1", "agent-cut", 0)
	got := childIDs(children)
	want := []string{
		TextItemID(0, "agent-cut", 1),
		"toolu_sub_read",
		TextItemID(0, "agent-cut", 2),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("backfilled children = %v, want %v", got, want)
	}

	launch, _, err := st.GetThreadItem("t1", "agent-cut")
	if err != nil {
		t.Fatalf("lookup launch: %v", err)
	}
	lastIndex := launch.ItemIndex
	for _, child := range children {
		if child.TurnIndex != 0 {
			t.Fatalf("child %s landed on turn %d, want the launch's turn 0", child.ID, child.TurnIndex)
		}
		if child.ItemIndex <= lastIndex {
			t.Fatalf("child %s item_index %d is not after %d", child.ID, child.ItemIndex, lastIndex)
		}
		lastIndex = child.ItemIndex
	}

	// The replayed text carries the provider's own block identity, which
	// is what a later live lookup finds it by.
	replayed := children[2]
	if replayed.Kind != itemKindAssistantText || replayed.Summary != "done: it is a main package" {
		t.Fatalf("replayed text row = %+v", replayed)
	}
	if got := decodeItemMetaMap(t, replayed.Meta)["provider_item_id"]; got != "msg_close#0" {
		t.Fatalf("replayed text provider_item_id = %v, want msg_close#0", got)
	}
	if tool := children[1]; tool.Kind != itemKindToolCall || tool.Status != statusCompleted {
		t.Fatalf("replayed tool row = %+v, want a completed tool_call", tool)
	}

	// Idempotent: the CLI can re-deliver the same envelope on a reconnect.
	notifyAgent(t, router, "t1", "agent-cut", "task-cut", fullAgentTranscript(t, "agent-cut-replay.jsonl"), nil)
	router.WaitForPendingSettles()
	if again := childIDs(childrenOfLaunch(t, st, "t1", "agent-cut", 0)); fmt.Sprint(again) != fmt.Sprint(want) {
		t.Fatalf("second notification changed the children: %v, want %v", again, want)
	}
}

// A NESTED agent gets the same backfill — its depth changes only whether
// a bell is written, never whether its transcript completes.
func TestSubagentTranscriptBackfillRunsForANestedLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-outer", "", "task-outer")
	startAgentLaunch(t, router, "t1", "agent-inner", "agent-outer", "task-inner")

	stashAgentTerminal(t, router, "t1", "agent-inner", "task-inner")
	notifyAgent(t, router, "t1", "agent-inner", "task-inner", fullAgentTranscript(t, "agent-inner.jsonl"), nil)
	router.WaitForPendingSettles()

	got := childIDs(childrenOfLaunch(t, st, "t1", "agent-inner", 0))
	want := []string{
		TextItemID(0, "agent-inner", 1),
		"toolu_sub_read",
		TextItemID(0, "agent-inner", 2),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("nested backfill children = %v, want %v", got, want)
	}
}

// A transcript that cannot be projected is reported, not silently
// skipped: a half-complete agent transcript reads exactly like a
// complete one and no second signal would ever correct it.
func TestSubagentTranscriptBackfillReportsAnUnprojectableTranscript(t *testing.T) {
	t.Run("over the ceiling", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)

		startAgentLaunch(t, router, "t1", "agent-huge", "", "task-huge")
		// The payload read truncates at the ceiling and still succeeds;
		// a projection cannot, so an oversized transcript is refused.
		oversized := filepath.Join(t.TempDir(), "agent-huge.jsonl")
		if err := os.WriteFile(oversized, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("create oversized transcript: %v", err)
		}
		if err := os.Truncate(oversized, claudeTaskOutputFileMaxBytes+1); err != nil {
			t.Fatalf("grow oversized transcript: %v", err)
		}

		stashAgentTerminal(t, router, "t1", "agent-huge", "task-huge")
		notifyAgent(t, router, "t1", "agent-huge", "task-huge", oversized, nil)
		router.WaitForPendingSettles()

		if children := childrenOfLaunch(t, st, "t1", "agent-huge", 0); len(children) != 0 {
			t.Fatalf("a refused transcript must write no rows, got %v", childIDs(children))
		}
		sibling, ok, err := st.GetThreadItem("t1", ToolCompletionID("agent-huge"))
		if err != nil || !ok {
			t.Fatalf("lookup sibling: ok=%v err=%v", ok, err)
		}
		meta := decodeItemMetaMap(t, sibling.Meta)
		if meta["notification_output_state"] != "error" {
			t.Fatalf("sibling output state = %v, want error", meta["notification_output_state"])
		}
		if readError, _ := meta["notification_output_error"].(string); readError == "" {
			t.Fatalf("an unprojectable transcript must name its reason, meta=%s", sibling.Meta)
		}
	})

	t.Run("nothing convertible", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)

		startAgentLaunch(t, router, "t1", "agent-garbage", "", "task-garbage")
		garbage := filepath.Join(t.TempDir(), "agent-garbage.jsonl")
		if err := os.WriteFile(garbage, []byte("not json\n{\"type\":\"nonsense\"}\n"), 0o600); err != nil {
			t.Fatalf("write garbage transcript: %v", err)
		}

		stashAgentTerminal(t, router, "t1", "agent-garbage", "task-garbage")
		notifyAgent(t, router, "t1", "agent-garbage", "task-garbage", garbage, nil)
		router.WaitForPendingSettles()

		if children := childrenOfLaunch(t, st, "t1", "agent-garbage", 0); len(children) != 0 {
			t.Fatalf("unreadable rows must not become partial rows, got %v", childIDs(children))
		}
		// The bytes still loaded, so the payload state is honest about
		// what happened: the file was read, it simply held no rows.
		sibling, ok, err := st.GetThreadItem("t1", ToolCompletionID("agent-garbage"))
		if err != nil || !ok {
			t.Fatalf("lookup sibling: ok=%v err=%v", ok, err)
		}
		if state := decodeItemMetaMap(t, sibling.Meta)["notification_output_state"]; state != "loaded" {
			t.Fatalf("sibling output state = %v, want loaded", state)
		}
	})
}

// A background Bash's `output_file` is captured stdout, not a sidechain
// transcript: the command_output payload path owns it and the backfill
// must not touch it.
func TestSubagentTranscriptBackfillSkipsABackgroundCommand(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash", "is_background": true,
		"input": map[string]any{"command": "sleep 1; echo hi"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-command",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bash start: %v", err)
	}
	output := filepath.Join(t.TempDir(), "task.output")
	if err := os.WriteFile(output, []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write command output: %v", err)
	}

	stashAgentTerminal(t, router, "t1", "bg-command", "task-command")
	notifyAgent(t, router, "t1", "bg-command", "task-command", output, nil)
	router.WaitForPendingSettles()

	if children := childrenOfLaunch(t, st, "t1", "bg-command", 0); len(children) != 0 {
		t.Fatalf("a command's output file must not project into rows, got %v", childIDs(children))
	}
	sibling, ok, err := st.GetThreadItem("t1", ToolCompletionID("bg-command"))
	if err != nil || !ok {
		t.Fatalf("lookup sibling: ok=%v err=%v", ok, err)
	}
	if sibling.PayloadID == "" {
		t.Fatal("background command lost its command_output payload")
	}
	if state := decodeItemMetaMap(t, sibling.Meta)["notification_output_state"]; state != "loaded" {
		t.Fatalf("sibling output state = %v, want loaded", state)
	}
}
