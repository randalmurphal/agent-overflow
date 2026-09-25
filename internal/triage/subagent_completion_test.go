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
	notifyAgentWithSummary(t, router, threadID, itemID, taskID, outputFile, usage, `Agent "review the file" completed`)
}

// notifyAgentWithSummary is notifyAgent with the envelope's `summary`
// chosen by the test: for a local_agent task the first envelope carries
// the agent's final report there, a later one only the finished bell.
func notifyAgentWithSummary(t *testing.T, router *Router, threadID, itemID, taskID, outputFile string, usage map[string]any, summary string) {
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
		Meta: meta, Content: summary, Timestamp: time.Now(),
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

// deliverSubagentPrompt drives the parented `user`/text envelope the CLI
// echoes when an agent runs INLINE (and only then: an async agent never
// echoes its prompt, so its launch input writes a provisional one).
func deliverSubagentPrompt(t *testing.T, router *Router, threadID, scope, providerItemID, content string) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"provider_item_id": providerItemID})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: threadID, Role: "user",
		Content: content, ContentPresent: true, Meta: meta,
		ParentToolUseID: scope, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent prompt %s: %v", providerItemID, err)
	}
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
// Q11: an agent's stop is its card, never a bell.
// ---------------------------------------------------------------------

// A top-level agent's notification writes its completion sibling, whose
// card is the timeline record, and no bell.
func TestTaskNotificationWritesNoBellForATopLevelAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startAgentLaunch(t, router, "t1", "agent-top", "", "task-top")
	stashAgentTerminal(t, router, "t1", "agent-top", "task-top")
	notifyAgent(t, router, "t1", "agent-top", "task-top", "", nil)

	if notifications := findItemsByKind(t, st, "t1", itemKindNotification); len(notifications) != 0 {
		t.Fatalf("a top-level agent's stop wrote %d notification rows, want none", len(notifications))
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 1 {
		t.Fatalf("expected 1 completion sibling, got %d", len(dones))
	}
}

// A NESTED agent's completion updates its card silently: no bell row,
// but every other effect of the notification still happens: the stash
// drains into a sibling and the sibling is enriched with the output
// payload and its output state.
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
	// Never created: completion does not read an agent's output file.
	notifyAgent(t, router, "t1", "agent-inner", "task-inner", filepath.Join(t.TempDir(), "agent-inner.jsonl"), nil)

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

func TestTaskNotificationUsageLandsOnTheCompletionSibling(t *testing.T) {
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

	// Wire order for an async agent: task_updated{completed} settles the
	// launch onto its completion sibling, then task_notification reports
	// the run's usage.
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-usage", "tool_use_id": "agent-usage", "status": "completed", "source": "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent-usage",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated terminal: %v", err)
	}
	notifyAgent(t, router, "t1", "agent-usage", "task-usage", "", map[string]any{
		"totalTokens": 4321, "toolUses": 7, "durationMs": 42000,
	})

	// The launch row is the immutable spawn event: nothing lands on it.
	launch, ok, err := st.GetThreadItem("t1", "agent-usage")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	if got := persistedSubagentProgress(launch.Meta); got != (provider.SubagentProgressMeta{}) {
		t.Fatalf("launch row carries final progress %+v; the completion sibling owns it", got)
	}
	completion, ok, err := st.GetThreadItem("t1", ToolCompletionID("agent-usage"))
	if err != nil || !ok {
		t.Fatalf("lookup completion sibling: ok=%v err=%v", ok, err)
	}
	progress := persistedSubagentProgress(completion.Meta)
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
// The completion's collapsed answer line.
// ---------------------------------------------------------------------

func outputFilePreview(t *testing.T, st *store.Store, threadID, launchID string) (string, bool) {
	t.Helper()
	sibling, ok, err := st.GetThreadItem(threadID, ToolCompletionID(launchID))
	if err != nil || !ok {
		t.Fatalf("lookup sibling: ok=%v err=%v", ok, err)
	}
	if sibling.PayloadID == "" {
		t.Fatalf("sibling carries no payload, meta=%s", sibling.Meta)
	}
	payload, err := st.GetPayloadMeta(threadID, sibling.PayloadID)
	if err != nil {
		t.Fatalf("payload meta: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(payload.Meta), &meta); err != nil {
		t.Fatalf("decode payload meta %s: %v", payload.Meta, err)
	}
	preview, present := meta["preview"]
	text, _ := preview.(string)
	return text, present
}

// The card's collapsed answer line is the agent's final report, which the
// notification envelope carries in `summary`. The output file is never
// read, so a notification without a report (the later `Agent "…" finished`
// bell, or no summary at all) leaves the completion without a preview.
func TestSubagentOutputFilePreviewIsTheNotificationReport(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		want    string
		present bool
	}{
		{"the envelope's report, flattened", "\n\nReviewed 3 files.\nNo blocking issues.\n", "Reviewed 3 files. No blocking issues.", true},
		{"the finished bell is not a report", `Agent "review the file" finished`, "", false},
		{"no summary, no report", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)

			startAgentLaunch(t, router, "t1", "agent-report", "", "task-report")
			stashAgentTerminal(t, router, "t1", "agent-report", "task-report")
			// Never created: completion does not read an agent's output file.
			outputFile := filepath.Join(t.TempDir(), "agent-report.jsonl")
			notifyAgentWithSummary(t, router, "t1", "agent-report", "task-report", outputFile, nil, tc.summary)
			router.WaitForPendingSettles()

			preview, present := outputFilePreview(t, st, "t1", "agent-report")
			if preview != tc.want || present != tc.present {
				t.Fatalf("preview = %q (present %v), want %q (present %v)", preview, present, tc.want, tc.present)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Completion never reads or replays a transcript.
// ---------------------------------------------------------------------

// An agent's rows are the ones the live stream and the session mirror
// delivered. Its completion settles the launch from the notification and
// the stash alone: it opens no transcript and writes no child row, so its
// cost does not grow with the agent. The output file here holds a row the
// thread never saw; a completion that read it would add that row.
func TestAgentCompletionReadsNoTranscript(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// completeAgent settles one launch and reports the store reads and
	// item upserts its notification cost.
	completeAgent := func(launchID, taskID string, children int) (uint64, int, time.Duration) {
		t.Helper()
		startAgentLaunch(t, router, "t1", launchID, "", taskID)
		now := time.Now().UnixMilli()
		for i := 0; i < children; i++ {
			kind, toolName := itemKindAssistantText, ""
			if i%2 == 1 {
				kind, toolName = itemKindToolCall, "Read"
			}
			if _, err := appendSeed(st, store.Item{
				ID: fmt.Sprintf("%s-child-%03d", launchID, i), ThreadID: "t1", TurnIndex: 0,
				Kind: kind, Role: "assistant", Status: statusCompleted, ToolName: toolName,
				Summary: "mirrored row", ParentID: launchID, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("append mirrored child %d: %v", i, err)
			}
		}
		stashAgentTerminal(t, router, "t1", launchID, taskID)
		router.WaitForPendingSettles()

		transcript := writeTestFile(t, launchID+".jsonl", `{"type":"assistant","uuid":"only-in-file","isSidechain":true,`+
			`"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"assistant","id":"msg_file_only","model":"m",`+
			`"content":[{"type":"text","text":"a row the thread never saw"}]}}`+"\n")
		emissions.reset()
		reads := st.ReadCount()
		start := time.Now()
		notifyAgentWithSummary(t, router, "t1", launchID, taskID, transcript, nil, "Reviewed.")
		elapsed := time.Since(start)
		reads = st.ReadCount() - reads
		upserts := len(itemUpserts(emissions.snapshot()))

		if got := len(childrenOfLaunch(t, st, "t1", launchID, 0)); got != children {
			t.Fatalf("%s: completion changed the agent's rows: %d, want %d", launchID, got, children)
		}
		if _, found, err := st.GetThreadItem("t1", "text:0:"+launchID+":provider:msg_file_only#0"); err != nil || found {
			t.Fatalf("%s: completion replayed the transcript: found=%v err=%v", launchID, found, err)
		}
		sibling, found, err := st.GetThreadItem("t1", ToolCompletionID(launchID))
		if err != nil || !found {
			t.Fatalf("%s: completion sibling: found=%v err=%v", launchID, found, err)
		}
		if state := decodeItemMetaMap(t, sibling.Meta)["notification_output_state"]; state != "loaded" {
			t.Fatalf("%s: notification_output_state = %v, want loaded", launchID, state)
		}
		return reads, upserts, elapsed
	}

	emptyReads, emptyUpserts, _ := completeAgent("agent-empty", "task-empty", 0)
	bigReads, bigUpserts, elapsed := completeAgent("agent-big", "task-big", 500)
	t.Logf("completion of a 500-row agent: %v, %d store reads, %d item upserts", elapsed, bigReads, bigUpserts)
	if bigReads != emptyReads || bigUpserts != emptyUpserts {
		t.Fatalf("completion cost grew with the agent: 500 rows took %d reads and %d upserts, 0 rows %d and %d",
			bigReads, bigUpserts, emptyReads, emptyUpserts)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("completion of a 500-row agent took %v", elapsed)
	}
}

func writeTestFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
