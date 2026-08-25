package claude

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
)

// TestAppendToolResultBlock_TaskOutputEmitsOwnIDCompleteBeforeEnrichment
// pins the TaskOutput event ordering — invariant 20 says every
// `tool_use_id` on a `tool_result` MUST receive an
// `EventToolComplete` for its own id, and that emission is always the
// first event. The additive `EventBackgroundTaskTerminal` for the
// underlying backgrounded task comes second. This ordering matters
// for the triage router's interrupt-queue behaviour, which drains in
// arrival order.
func TestAppendToolResultBlock_TaskOutputEmitsOwnIDCompleteBeforeEnrichment(t *testing.T) {
	parser := NewParser()

	// Prime: Agent subagent launched with run_in_background:true.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-agent","tool_use_id":"tool-agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	// TaskOutput tool_use.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-taskoutput","name":"TaskOutput","input":{"task_id":"task-agent"}}]}}`)); err != nil {
		t.Fatalf("taskoutput start: %v", err)
	}

	// TaskOutput result with terminal task.
	line := []byte(`{"type":"user","tool_use_result":{"retrieval_status":"success","task":{"task_id":"task-agent","task_type":"local_agent","status":"completed","output":"done","exitCode":0}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-taskoutput","content":"Task completed"}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("taskoutput result: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events (own-id complete + bg terminal), got %d: %+v", len(events), events)
	}

	// First: own-id EventToolComplete.
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("events[0].Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-taskoutput" {
		t.Fatalf("events[0].ItemID: got %q, want tool-taskoutput", events[0].ItemID)
	}

	// Second: EventBackgroundTaskTerminal.
	if events[1].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("events[1].Kind: got %q, want %q", events[1].Kind, provider.EventBackgroundTaskTerminal)
	}
	if events[1].ItemID != "tool-agent" {
		t.Fatalf("events[1].ItemID: got %q, want tool-agent", events[1].ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[1].Meta, &meta); err != nil {
		t.Fatalf("enrichment unmarshal: %v", err)
	}
	if meta["task_id"] != "task-agent" {
		t.Fatalf("enrichment.task_id: got %v", meta["task_id"])
	}
	if meta["tool_use_id"] != "tool-agent" {
		t.Fatalf("enrichment.tool_use_id: got %v", meta["tool_use_id"])
	}
	if meta["exit_code"] != float64(0) {
		t.Fatalf("enrichment.exit_code: got %v", meta["exit_code"])
	}
}

// TestAppendToolResultBlock_PlaceholderEmitsCompleteWithBackgroundFlag
// covers the backgrounded placeholder path. The `tool_use_result`
// sibling carries `backgroundTaskId` but no `task.status`, so the
// enrichment helper must return false (no terminal) while the own-id
// completion fires with is_background=true.
func TestAppendToolResultBlock_PlaceholderEmitsCompleteWithBackgroundFlag(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"sleep 1","run_in_background":true}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"backgroundTaskId":"task-bg"},"message":{"role":"user","content":[{"tool_use_id":"tool-bg","type":"tool_result","content":"Command running in background with ID: task-bg. Output is being written to: /tmp/task-bg.output","is_error":false}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-bg" {
		t.Fatalf("ItemID: got %q, want tool-bg", events[0].ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Fatalf("is_background: got %v, want true", meta["is_background"])
	}
}

// TestAppendToolResultBlock_TimeoutAutoBackgroundSetsBackgroundFlag pins
// the timeout auto-background path: the CLI moves a FOREGROUND Bash to the
// background once it exceeds its timeout. Unlike the run_in_background:true
// case, the tool_use input carries NO background flag, so the launch-time
// `backgroundToolUses` hint is empty — the `tool_use_result.backgroundTaskId`
// wire marker is the ONLY signal. The completion must still emit
// is_background=true so triage keeps the launch row running (and the later
// task_updated terminal writes the sibling completion). Captured from a real
// session: input keys are {command, description}; the sibling carries
// `assistantAutoBackgrounded:false` alongside `backgroundTaskId`. See
// claude-wire.md §E2.
func TestAppendToolResultBlock_TimeoutAutoBackgroundSetsBackgroundFlag(t *testing.T) {
	parser := NewParser()

	// Foreground launch — NO run_in_background in the input.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"make check","description":"Run full type check"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if parser.isBackground("tool-bg") {
		t.Fatal("precondition: a launch without run_in_background must not be flagged in backgroundToolUses")
	}

	line := []byte(`{"type":"user","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,"backgroundTaskId":"bhulux5j4","assistantAutoBackgrounded":false},"message":{"role":"user","content":[{"tool_use_id":"tool-bg","type":"tool_result","content":"Command running in background with ID: bhulux5j4. Output is being written to: /tmp/bhulux5j4.output","is_error":false}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-bg" {
		t.Fatalf("ItemID: got %q, want tool-bg", events[0].ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Fatalf("is_background: got %v, want true (backgroundTaskId marker)", meta["is_background"])
	}
	// The wire-marker path must not touch backgroundToolUses — there was
	// never a launch-time entry to allocate or release.
	if len(parser.backgroundToolUses) != 0 {
		t.Errorf("backgroundToolUses must stay empty for the timeout path; got %v", parser.backgroundToolUses)
	}
}

// TestAppendToolResultBlock_InlineToolNoBackgroundFlag confirms the
// negative case — a plain inline tool's completion must NOT carry
// is_background, so triage can flip status=errored in the
// force-close safety net (invariant 23).
func TestAppendToolResultBlock_InlineToolNoBackgroundFlag(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-inline","name":"Read","input":{"file_path":"/etc/hosts"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tool-inline","type":"tool_result","content":"127.0.0.1"}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["is_background"]; ok && v != false {
		t.Fatalf("inline tool should not carry is_background=true, got %v", v)
	}
}

// TestAppendToolResultBlock_BareToolUseResultPlumbedToMeta pins the
// fix for: Edit / Write / MultiEdit / NotebookEdit ship a bare-object
// `tool_use_result` (e.g. {filePath, structuredPatch, ...}) with no
// `tool_use_id` field. `indexToolUseResults` returns an empty map for
// that shape (it indexes only by tool_use_id), so the resolution must
// fall back to `raw["tool_use_result"]`. Without this fallback, the
// triage Claude file-change extractor never sees the structured
// patch and no diff renders in the UI.
func TestAppendToolResultBlock_BareToolUseResultPlumbedToMeta(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-edit","name":"Edit","input":{"file_path":"/tmp/scratch.txt","old_string":"a","new_string":"b"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	// Bare-object tool_use_result with no tool_use_id field — the
	// shape FileEditTool/types.ts ships per claude-code-source-code.
	line := []byte(`{"type":"user","tool_use_result":{"filePath":"/tmp/scratch.txt","oldString":"a","newString":"b","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-a","+b"]}]},"message":{"role":"user","content":[{"tool_use_id":"tool-edit","type":"tool_result","content":"The file /tmp/scratch.txt has been updated successfully."}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	tur, ok := meta["tool_use_result"]
	if !ok {
		t.Fatalf("expected tool_use_result in meta, got keys %v", slices.Sorted(maps.Keys(meta)))
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(tur, &payload); err != nil {
		t.Fatalf("unmarshal tool_use_result: %v", err)
	}
	if _, ok := payload["structuredPatch"]; !ok {
		t.Fatalf("expected structuredPatch in tool_use_result, got keys %v", slices.Sorted(maps.Keys(payload)))
	}
}

// TestAppendToolResultBlock_OrphanToolResultDropped covers invariant E4
// from claude-wire.md — a tool_result whose tool_use_id has no matching
// tool_use earlier in the stream is dropped silently (no ghost row).
// Note: the universal invariant says every tool_use_id emits, but an
// EMPTY tool_use_id means there is no correlation target.
func TestAppendToolResultBlock_OrphanToolResultDroppedWhenIDMissing(t *testing.T) {
	parser := NewParser()
	// Missing tool_use_id entirely.
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"orphaned"}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for orphan tool_result (no tool_use_id), got %+v", events)
	}
}

// TestAppendToolResultBlock_BackgroundFlagClearedOnCorrelation pins
// the one-shot correlation lifecycle: once a backgrounded tool_use's
// placeholder tool_result has been echoed, the parser releases the
// is_background flag for that tool_use_id. Without this, the
// backgroundToolUses map grows unbounded across a long session — every
// backgrounded launch leaves an entry that only Close() clears. The
// second parse of a matching tool_result would also re-stamp
// is_background on a subsequent echo, which is meaningless (the
// placeholder is one-shot) and sends noise through triage.
func TestAppendToolResultBlock_BackgroundFlagClearedOnCorrelation(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"sleep 1","run_in_background":true}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if !parser.isBackground("tool-bg") {
		t.Fatal("expected tool-bg flagged as background after run_in_background tool_use")
	}

	line := []byte(`{"type":"user","tool_use_result":{"stdout":"","stderr":"","interrupted":false,"backgroundTaskId":"task-bg"},"message":{"role":"user","content":[{"tool_use_id":"tool-bg","type":"tool_result","content":"Command running in background with ID: task-bg.","is_error":false}]}}`)
	if _, err := parser.ParseLine(testThread, line); err != nil {
		t.Fatalf("parse placeholder: %v", err)
	}

	if parser.isBackground("tool-bg") {
		t.Error("backgroundToolUses[tool-bg] must be cleared after placeholder tool_result correlation")
	}
	if len(parser.backgroundToolUses) != 0 {
		t.Errorf("backgroundToolUses must be empty after single correlation; got %v", parser.backgroundToolUses)
	}
}

// TestAppendToolResultBlock_BackgroundFlagNoLeakOverManyLaunches is a
// shape-level leak test — parse N background launches + echoes and
// assert the correlation map stays bounded. Without clearBackground on
// correlation the map would hold N entries until Close(); with the
// clear, it stays at zero between pairs.
func TestAppendToolResultBlock_BackgroundFlagNoLeakOverManyLaunches(t *testing.T) {
	parser := NewParser()

	const pairs = 50
	for i := 0; i < pairs; i++ {
		id := fmt.Sprintf("tool-bg-%d", i)
		start := fmt.Sprintf(`{"type":"assistant","message":{"id":"msg-%d","role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"sleep 1","run_in_background":true}}]}}`, i, id)
		if _, err := parser.ParseLine(testThread, []byte(start)); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		result := fmt.Sprintf(`{"type":"user","tool_use_result":{"backgroundTaskId":"task-%d"},"message":{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":"running","is_error":false}]}}`, i, id)
		if _, err := parser.ParseLine(testThread, []byte(result)); err != nil {
			t.Fatalf("result %d: %v", i, err)
		}
	}
	if len(parser.backgroundToolUses) != 0 {
		t.Errorf("backgroundToolUses leaked across %d pairs: got len=%d", pairs, len(parser.backgroundToolUses))
	}
}

// TestAppendToolResultBlock_ErroredCompletionPropagatesIsError pins the
// meta shape for a failed inline tool: is_error=true lands on the meta
// and exit_code is surfaced when present.
func TestAppendToolResultBlock_ErroredCompletionPropagatesIsError(t *testing.T) {
	parser := NewParser()

	line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tool-err","type":"tool_result","content":"command not found","is_error":true}]},"tool_use_result":{"tool_use_id":"tool-err","exit_code":127}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_error"] != true {
		t.Fatalf("is_error: got %v, want true", meta["is_error"])
	}
	if meta["exit_code"] != float64(127) {
		t.Fatalf("exit_code: got %v, want 127", meta["exit_code"])
	}
}

// TestAppendToolResultBlock_MonitorLaunchSetsBackgroundFlag pins the
// Monitor watch-task launch ack (claude-wire.md §E7): the harness runs
// the command as a background `local_bash` task and acks immediately
// with `tool_use_result: {taskId, timeoutMs, persistent}` — no
// `run_in_background` in the input, no `backgroundTaskId` on the wire.
// The completion must carry is_background=true so triage keeps the
// launch row running and the reaper's ListRunningBackgroundToolCalls
// sees the live watch (missing this signal is how a live
// Monitor-watched session read as reap-idle; captured 2026-07-28,
// session d946175f). Both persistent variants share the shape.
func TestAppendToolResultBlock_MonitorLaunchSetsBackgroundFlag(t *testing.T) {
	cases := []struct {
		name          string
		toolUseResult string
	}{
		{"persistent", `{"taskId":"bpzc8uiti","timeoutMs":0,"persistent":true}`},
		{"non-persistent", `{"persistent":false,"taskId":"bpzc8uiti","timeoutMs":600000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()

			if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-monitor","name":"Monitor","input":{"command":"bash chain.sh | grep PHASE","persistent":true,"timeout_ms":3600000}}]}}`)); err != nil {
				t.Fatalf("assistant tool_use: %v", err)
			}

			line := []byte(`{"type":"user","tool_use_result":` + tc.toolUseResult + `,"message":{"role":"user","content":[{"tool_use_id":"tool-monitor","type":"tool_result","content":"Monitor started (task bpzc8uiti, persistent — runs until TaskStop or session end).","is_error":false}]}}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
			}
			if events[0].Kind != provider.EventToolComplete {
				t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
			}
			var meta map[string]any
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta["is_background"] != true {
				t.Fatalf("is_background: got %v, want true (Monitor launch ack)", meta["is_background"])
			}
			if meta["watch_task"] != true {
				t.Fatalf("watch_task: got %v, want true (a Monitor is a watch — must not block the flush queue)", meta["watch_task"])
			}

			// The ack also seeds the task_id ↔ tool_use_id correlation, so
			// a later task_updated terminal routes to this launch even when
			// a reconnect-fresh parser missed system/task_started.
			terminal, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"bpzc8uiti","patch":{"status":"killed"}}`))
			if err != nil {
				t.Fatalf("task_updated: %v", err)
			}
			if len(terminal) != 1 || terminal[0].Kind != provider.EventBackgroundTaskTerminal {
				t.Fatalf("task_updated must emit one terminal, got %+v", terminal)
			}
			if terminal[0].ItemID != "tool-monitor" {
				t.Fatalf("terminal ItemID: got %q, want tool-monitor", terminal[0].ItemID)
			}
		})
	}
}

// TestAppendToolResultBlock_TaskListAckNotBackground guards the Monitor
// discriminator against the task-list tools: TaskCreate/TaskUpdate acks
// carry a top-level `taskId` too, but describe a bookkeeping row, not a
// process — `taskId` alone must never classify as backgrounded. The
// launch here deliberately does NOT stage a pending task mutation (that
// carve-out intercepts real TaskCreate/TaskUpdate flows before the
// classifier), so this pins the classifier itself against the ack shape.
func TestAppendToolResultBlock_TaskListAckNotBackground(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-tasklist","name":"SomeTool","input":{}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","tool_use_result":{"statusChange":"pending -> in_progress","success":true,"taskId":"3","updatedFields":["status"]},"message":{"role":"user","content":[{"tool_use_id":"tool-tasklist","type":"tool_result","content":"Updated task #3","is_error":false}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatalf("is_background must be absent for a task-list ack; meta=%v", meta)
	}
}

// TestAppendToolResultBlock_ScheduleWakeupEmitsSessionWakeup pins the
// ScheduleWakeup ack pair (claude-wire.md §E8): a schedule ack emits
// EventSessionWakeup carrying the absolute fire time, and the
// `{stop:true}` ack emits the clearing event (ScheduledForUnixMs 0).
// Shapes captured live 2026-07-24 (session d946175f).
func TestAppendToolResultBlock_ScheduleWakeupEmitsSessionWakeup(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-wakeup","name":"ScheduleWakeup","input":{"delaySeconds":1500,"prompt":"continue the loop"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","tool_use_result":{"clampedDelaySeconds":1500,"scheduledFor":1784917860000,"wasClamped":false},"message":{"role":"user","content":[{"tool_use_id":"tool-wakeup","type":"tool_result","content":"Next wakeup scheduled for 14:31:00 (in 1559s).","is_error":false}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected completion + wakeup, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("events[0].Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	var completeMeta map[string]any
	if err := json.Unmarshal(events[0].Meta, &completeMeta); err != nil {
		t.Fatalf("unmarshal completion meta: %v", err)
	}
	if _, ok := completeMeta["is_background"]; ok {
		t.Fatal("a ScheduleWakeup ack is not a background launch — is_background must be absent")
	}
	if events[1].Kind != provider.EventSessionWakeup {
		t.Fatalf("events[1].Kind: got %q, want %q", events[1].Kind, provider.EventSessionWakeup)
	}
	var wakeMeta provider.SessionWakeupMeta
	if err := json.Unmarshal(events[1].Meta, &wakeMeta); err != nil {
		t.Fatalf("unmarshal wakeup meta: %v", err)
	}
	if wakeMeta.ScheduledForUnixMs != 1784917860000 {
		t.Fatalf("ScheduledForUnixMs: got %d, want 1784917860000", wakeMeta.ScheduledForUnixMs)
	}

	// Stop ack clears: same tool, `{stop:true}` input, ack reports
	// scheduledFor 0 + stopped true.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"tool_use","id":"tool-wakeup-stop","name":"ScheduleWakeup","input":{"stop":true}}]}}`)); err != nil {
		t.Fatalf("assistant stop tool_use: %v", err)
	}
	stopLine := []byte(`{"type":"user","tool_use_result":{"scheduledFor":0,"clampedDelaySeconds":0,"wasClamped":false,"stopped":true},"message":{"role":"user","content":[{"tool_use_id":"tool-wakeup-stop","type":"tool_result","content":"Loop stopped — no further wakeups scheduled.","is_error":false}]}}`)
	stopEvents, err := parser.ParseLine(testThread, stopLine)
	if err != nil {
		t.Fatalf("parse stop: %v", err)
	}
	if len(stopEvents) != 2 || stopEvents[1].Kind != provider.EventSessionWakeup {
		t.Fatalf("expected completion + clearing wakeup, got %+v", stopEvents)
	}
	var stopMeta provider.SessionWakeupMeta
	if err := json.Unmarshal(stopEvents[1].Meta, &stopMeta); err != nil {
		t.Fatalf("unmarshal stop wakeup meta: %v", err)
	}
	if stopMeta.ScheduledForUnixMs != 0 {
		t.Fatalf("stop ScheduledForUnixMs: got %d, want 0", stopMeta.ScheduledForUnixMs)
	}
}

// TestAppendToolResultBlock_StringToolUseResultNoNewSignals confirms a
// string-valued `tool_use_result` (harness InputValidationError acks
// arrive this way) decodes to zero signals: no background flag, no
// wakeup event, no parse error.
func TestAppendToolResultBlock_StringToolUseResultNoNewSignals(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-err","name":"Monitor","input":{"command":"x","timeout":1}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","tool_use_result":"InputValidationError: Unrecognized key \"timeout\"","message":{"role":"user","content":[{"tool_use_id":"tool-err","type":"tool_result","content":"<tool_use_error>InputValidationError</tool_use_error>","is_error":true}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatal("error ack must not classify as background")
	}
}

// sidechainAsyncAckText is the ack BODY of a real depth-2 async agent
// launch, captured 2026-08-19 (agent a126ec31b78a8dfc6; the transcript
// path and prompt text are sanitized, the sentence structure and the
// `agentId:` line are verbatim). It is written JSON-escaped so it can be
// embedded straight into the envelope literals below.
const sidechainAsyncAckText = `Async agent launched successfully. (This tool result is internal metadata — never quote or paste any part of it, including the agentId below, into a user-facing reply.)\nagentId: a126ec31b78a8dfc6 (internal ID - do not mention to user. Use SendMessage with to: 'a126ec31b78a8dfc6', summary: '<5-10 word recap>' to continue this agent.)\nThe agent is working in the background. You will be notified automatically when it completes. You know nothing about its results until that notification arrives — do not report, assume, or predict them.\nDo not duplicate this agent's work — avoid working with the same files or topics it is using.\noutput_file: /tmp/claude/00000000-0000-0000-0000-000000000000/tasks/a126ec31b78a8dfc6.output\nDo NOT Read or tail this file via the shell tool — it is the full subagent JSONL transcript and reading it will overflow your context.`

// TestAppendToolResultBlock_SidechainAsyncLaunchAckPromotesToBackground
// pins claude-wire.md §E5b: on a SIDECHAIN line (a subagent launching
// its own async agent — depth 2) Claude omits the `tool_use_result`
// envelope entirely, so every structured §E5 signal misses. Before this
// path existed the ack settled the launch in place with the internal
// metadata text as the card body, and every later task_updated /
// task_notification for that agent was dropped at triage's foreground
// gates. Envelope shape verified from a live capture (2026-08-19,
// session ed8a5c81): the only top-level keys are message /
// parent_tool_use_id / session_id / subagent_type / task_description /
// timestamp / type / uuid.
func TestAppendToolResultBlock_SidechainAsyncLaunchAckPromotesToBackground(t *testing.T) {
	parser := NewParser()

	// Depth-2 launch: an Agent tool_use on a SIDECHAIN assistant
	// envelope (parent_tool_use_id set), with no run_in_background.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","parent_tool_use_id":"toolu_parent","message":{"id":"msg-side","role":"assistant","content":[{"type":"tool_use","id":"toolu_launch","name":"Agent","input":{"description":"Angle A: line-by-line scan","subagent_type":"general-purpose","prompt":"review the diff"}}]}}`)); err != nil {
		t.Fatalf("sidechain assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","parent_tool_use_id":"toolu_parent","session_id":"ed8a5c81-d3ac-4433-a958-b0a0b99217f2","uuid":"09a90d76-82db-426d-a197-3f6d62c1ef1c","timestamp":"2026-08-20T03:23:57.854Z","subagent_type":"general-purpose","task_description":"Angle A: line-by-line scan","message":{"role":"user","content":[{"tool_use_id":"toolu_launch","type":"tool_result","content":[{"type":"text","text":"` + sidechainAsyncAckText + `"}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse sidechain ack: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "toolu_launch" {
		t.Fatalf("ItemID: got %q, want toolu_launch", events[0].ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	// The whole point: identical is_background meta to the top-level
	// §E5 ack, so triage's keep-running flip holds the launch row at
	// status=running and never writes the internal ack text as the
	// card's body.
	if meta["is_background"] != true {
		t.Fatalf("is_background: got %v, want true (sidechain async launch ack)", meta["is_background"])
	}
	if _, ok := meta["tool_use_result"]; ok {
		t.Fatal("the sidechain ack carries no tool_use_result — meta must not invent one")
	}
	if _, ok := meta["watch_task"]; ok {
		t.Fatal("an async agent launch is not a Monitor watch")
	}

	// Task binding: the ack's own agentId is the task_id the later
	// lifecycle addresses. No system/task_started was fed here, so a
	// resolved terminal proves the binding came from the ACK — the
	// reconnect-safe property §E5 already gives the top-level path.
	terminal, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"a126ec31b78a8dfc6","patch":{"status":"killed","end_time":1787196430286}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(terminal) != 1 || terminal[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("task_updated must emit one terminal, got %+v", terminal)
	}
	if terminal[0].ItemID != "toolu_launch" {
		t.Fatalf("terminal ItemID: got %q, want toolu_launch (ack must seed task_id ↔ tool_use_id)", terminal[0].ItemID)
	}
}

// TestAppendToolResultBlock_AsyncAckStructuredResultStaysAuthoritative
// is the first false-positive bound: when a `tool_use_result` IS
// present it is the SOLE authority and the text is never sniffed. The
// worst case is an INLINE agent's real completion — same tool, same
// `agentId` field, `status:"completed"` — whose output happens to open
// with the sentinel (an agent reporting on this very code path).
// Promoting it would strand a finished agent's card at "running"
// forever, since no task terminal follows an inline completion that
// triage would accept.
func TestAppendToolResultBlock_AsyncAckStructuredResultStaysAuthoritative(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_inline","name":"Agent","input":{"description":"inline review","subagent_type":"general-purpose","prompt":"p"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}

	line := []byte(`{"type":"user","tool_use_result":{"agentId":"a0e27f56d74e34245","agentType":"general-purpose","status":"completed","totalDurationMs":431917,"totalTokens":129893},"message":{"role":"user","content":[{"tool_use_id":"toolu_inline","type":"tool_result","content":[{"type":"text","text":"` + sidechainAsyncAckText + `"}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatalf("a present tool_use_result is the sole authority — text must not promote; meta=%v", meta)
	}
}

// TestAppendToolResultBlock_AsyncAckTextNotPromotedWithoutLaunchTool is
// the second false-positive bound: ordinary tool output that merely
// carries the ack text cannot promote, because the fallback additionally
// requires the tool_use to have been observed as Claude's agent-launch
// tool (`Agent`/`Task`). A `cat` of a transcript, a grep hit on this
// file, or a Monitor ack quoting it all fail here.
func TestAppendToolResultBlock_AsyncAckTextNotPromotedWithoutLaunchTool(t *testing.T) {
	for _, toolName := range []string{"Bash", "Read", "Monitor", "TaskUpdate"} {
		t.Run(toolName, func(t *testing.T) {
			parser := NewParser()

			if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_other","name":"`+toolName+`","input":{}}]}}`)); err != nil {
				t.Fatalf("assistant tool_use: %v", err)
			}

			line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_other","type":"tool_result","content":[{"type":"text","text":"` + sidechainAsyncAckText + `"}]}]}}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			// TaskCreate/TaskUpdate are carved out upstream and emit no
			// completion at all; every other tool emits one with no
			// background flag. Neither may classify as async.
			for _, evt := range events {
				if evt.Kind != provider.EventToolComplete {
					continue
				}
				var meta map[string]any
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal meta: %v", err)
				}
				if _, ok := meta["is_background"]; ok {
					t.Fatalf("%s output carrying the ack text must not classify as an async launch; meta=%v", toolName, meta)
				}
			}
		})
	}
}

// TestAppendToolResultBlock_AsyncAckTextVariantsNotPromoted pins the two
// remaining refusals on the launch tool ITSELF, where the agent-launch
// marker is present and only the text decides:
//
//   - sentinel present but NOT at position 0 (an agent reporting on the
//     ack rather than acking) — the test is a prefix match, never a
//     contains;
//   - sentinel at position 0 but no recoverable `agentId:` line. An
//     unpromotable ack cannot be lifecycle-correlated, so a permanently
//     "running" card would be strictly worse than today's
//     instantly-done one.
func TestAppendToolResultBlock_AsyncAckTextVariantsNotPromoted(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"sentinel not at start", `The subagent replied: Async agent launched successfully. (...)\nagentId: a126ec31b78a8dfc6 (internal ID)`},
		{"no agentId line", `Async agent launched successfully. (This tool result is internal metadata.)\nThe agent is working in the background.`},
		{"agentId line not on a line boundary", `Async agent launched successfully.\nsee agentId: a126ec31b78a8dfc6 for details`},
		{"agentId value empty", `Async agent launched successfully.\nagentId: (redacted)`},
		{"resume ack shape (E6), no agentId", `Async agent launched successfully. Agent resumed from transcript in the background.`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()

			if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","parent_tool_use_id":"toolu_parent","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_launch","name":"Agent","input":{"description":"d","subagent_type":"general-purpose","prompt":"p"}}]}}`)); err != nil {
				t.Fatalf("assistant tool_use: %v", err)
			}

			line := []byte(`{"type":"user","parent_tool_use_id":"toolu_parent","message":{"role":"user","content":[{"tool_use_id":"toolu_launch","type":"tool_result","content":[{"type":"text","text":"` + tc.text + `"}]}]}}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
			}
			var meta map[string]any
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if _, ok := meta["is_background"]; ok {
				t.Fatalf("must not promote (%s); meta=%v", tc.name, meta)
			}
		})
	}
}

// TestAsyncLaunchAckAgentID covers the extractor directly, including the
// id shape the ack uses (lowercase hex followed by the parenthetical
// note) and the refusal to assert a length.
func TestAsyncLaunchAckAgentID(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"captured ack", "Async agent launched successfully. (…)\nagentId: a126ec31b78a8dfc6 (internal ID - do not mention to user.)\noutput_file: /tmp/x", "a126ec31b78a8dfc6"},
		{"short id", "Async agent launched successfully.\nagentId: abc123", "abc123"},
		{"crlf body", "Async agent launched successfully.\r\nagentId: a126ec31b78a8dfc6 (internal)\r\n", "a126ec31b78a8dfc6"},
		{"no tab-or-space padding", "Async agent launched successfully.\nagentId:a126ec31b78a8dfc6", "a126ec31b78a8dfc6"},
		{"uppercase is not the observed shape", "Async agent launched successfully.\nagentId: A126EC31B", ""},
		{"beyond the scan bound", "Async agent launched successfully." + repeatLines(asyncLaunchAckMaxScanLines+2) + "agentId: a126ec31b78a8dfc6", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := asyncLaunchAckAgentID(tc.text)
			if tc.want == "" {
				if ok {
					t.Fatalf("expected no match, got %q", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, true)", got, ok, tc.want)
			}
		})
	}
}

func repeatLines(n int) string {
	out := make([]byte, 0, n*2)
	for range n {
		out = append(out, '\n', 'x')
	}
	return string(out) + "\n"
}

// TestAppendToolResultBlock_LiveAgentTaskPromotesRewordedAck pins
// background signal (5), the wire-typed twin of §E5b: when
// `system/task_started` (task_type local_agent) has fired for the
// tool_use and no terminal `task_updated` has resolved it, a
// tool_result with NO `tool_use_result` promotes to background even
// when its text passes none of §E5b's checks — here a reworded ack
// with no sentinel prefix and no `agentId:` line, the exact shapes a
// CLI wording change (or the refused no-agentId ack) produces. The
// terminal correlates through the task_started binding, which is why
// promoting without a recoverable agentId is safe on this path.
func TestAppendToolResultBlock_LiveAgentTaskPromotesRewordedAck(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","parent_tool_use_id":"toolu_parent","message":{"id":"msg-side","role":"assistant","content":[{"type":"tool_use","id":"toolu_launch","name":"Agent","input":{"description":"Angle A","subagent_type":"general-purpose","prompt":"p"}}]}}`)); err != nil {
		t.Fatalf("sidechain assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-angle","tool_use_id":"toolu_launch","task_type":"local_agent","description":"Angle A"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	line := []byte(`{"type":"user","parent_tool_use_id":"toolu_parent","message":{"role":"user","content":[{"tool_use_id":"toolu_launch","type":"tool_result","content":[{"type":"text","text":"Agent dispatched; running in the background. You will be notified on completion."}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse ack: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Fatalf("is_background: got %v, want true (live local_agent task)", meta["is_background"])
	}

	// The terminal routes through the task_started binding.
	terminal, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-angle","patch":{"status":"killed"}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(terminal) != 1 || terminal[0].Kind != provider.EventBackgroundTaskTerminal || terminal[0].ItemID != "toolu_launch" {
		t.Fatalf("terminal must route to toolu_launch, got %+v", terminal)
	}
}

// TestAppendToolResultBlock_TerminalClearsLiveAgentTask pins the
// awaited half of signal (5)'s contract: an awaited agent's terminal
// `task_updated` always precedes its real tool_result (0–45ms across
// every awaited completion in three weeks of wire logs), and that
// terminal must clear the liveness flag so the real result — which on
// a sidechain line also carries no `tool_use_result` — completes the
// row in place instead of reading as a background ack.
func TestAppendToolResultBlock_TerminalClearsLiveAgentTask(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","parent_tool_use_id":"toolu_parent","message":{"id":"msg-side","role":"assistant","content":[{"type":"tool_use","id":"toolu_awaited","name":"Agent","input":{"description":"awaited","subagent_type":"general-purpose","prompt":"p","run_in_background":false}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-awaited","tool_use_id":"toolu_awaited","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-awaited","patch":{"status":"completed"}}`)); err != nil {
		t.Fatalf("task_updated: %v", err)
	}

	line := []byte(`{"type":"user","parent_tool_use_id":"toolu_parent","message":{"role":"user","content":[{"tool_use_id":"toolu_awaited","type":"tool_result","content":[{"type":"text","text":"Findings: none. All hunks reviewed."}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatalf("awaited result after terminal must complete in place; meta=%v", meta)
	}
}

// TestAppendToolResultBlock_TaskOutputTerminalClearsLiveAgentTask pins
// the second disarm path for signal (5): when a terminal task_updated
// is lost (reconnect gap) and the task's completion surfaces only
// through TaskOutput enrichment, that terminal evidence must clear the
// liveness flag too — otherwise the original tool_use's later
// no-`tool_use_result` result would misclassify as a background ack.
func TestAppendToolResultBlock_TaskOutputTerminalClearsLiveAgentTask(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_agent","name":"Agent","input":{"description":"d","subagent_type":"general-purpose","prompt":"p"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-agent","tool_use_id":"toolu_agent","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"tool_use","id":"toolu_taskoutput","name":"TaskOutput","input":{"task_id":"task-agent"}}]}}`)); err != nil {
		t.Fatalf("taskoutput tool_use: %v", err)
	}
	// No task_updated arrives; the completion comes via TaskOutput.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"user","tool_use_result":{"retrieval_status":"success","task":{"task_id":"task-agent","task_type":"local_agent","status":"completed","output":"done","exitCode":0}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_taskoutput","content":"Task completed"}]}}`)); err != nil {
		t.Fatalf("taskoutput result: %v", err)
	}

	if parser.hasLiveAgentTask("toolu_agent") {
		t.Fatalf("TaskOutput terminal must clear the liveness flag for toolu_agent")
	}

	// The original tool_use's bare result now completes in place.
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_agent","type":"tool_result","content":[{"type":"text","text":"Findings: none."}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse original result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatalf("result after TaskOutput terminal must complete in place; meta=%v", meta)
	}
}

// TestAppendToolResultBlock_LocalBashTaskDoesNotMarkLive pins the
// task_type gate: every foreground Bash also emits task_started (task
// type "local_bash"), and a foreground Bash result's ordering against
// its terminal is not part of the verified local_agent contract — so
// local_bash must never arm signal (5).
func TestAppendToolResultBlock_LocalBashTaskDoesNotMarkLive(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"echo hi"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-bash","tool_use_id":"toolu_bash","task_type":"local_bash"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_bash","type":"tool_result","content":"hi","is_error":false}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatalf("foreground Bash must not promote via task liveness; meta=%v", meta)
	}
}

// TestAppendToolResultBlock_StructuredResultOverridesLiveTask pins the
// authority order between signal (5) and a present `tool_use_result`:
// even with the task nominally live in the parser's mirror, a
// tool_result that CARRIES the structured envelope is judged by that
// envelope alone. An inline completion whose terminal task_updated was
// lost (reconnect) must still complete in place rather than strand at
// "running" on the liveness flag.
func TestAppendToolResultBlock_StructuredResultOverridesLiveTask(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_inline","name":"Agent","input":{"description":"inline","subagent_type":"general-purpose","prompt":"p"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-inline","tool_use_id":"toolu_inline","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	// No terminal fed — the task is still "live" in the mirror, but the
	// result carries the structured completion envelope.
	line := []byte(`{"type":"user","tool_use_result":{"agentId":"a0e27f56d74e34245","agentType":"general-purpose","status":"completed","totalDurationMs":1000,"totalTokens":10},"message":{"role":"user","content":[{"tool_use_id":"toolu_inline","type":"tool_result","content":[{"type":"text","text":"done"}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if _, ok := meta["is_background"]; ok {
		t.Fatalf("present tool_use_result must stay authoritative over liveness; meta=%v", meta)
	}
}

// TestAppendToolResultBlock_NonTerminalUpdateKeepsLiveAgentTask pins
// where the liveness clear sits: a NON-terminal task_updated (progress
// patches normalize to "" and no-op) must not disarm signal (5) — only
// the terminal does. Guards against the clear migrating above the
// terminal-status guard in parseTaskLifecycleEvent.
func TestAppendToolResultBlock_NonTerminalUpdateKeepsLiveAgentTask(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","parent_tool_use_id":"toolu_parent","message":{"id":"msg-side","role":"assistant","content":[{"type":"tool_use","id":"toolu_launch","name":"Agent","input":{"description":"d","subagent_type":"general-purpose","prompt":"p"}}]}}`)); err != nil {
		t.Fatalf("assistant tool_use: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"task-live","tool_use_id":"toolu_launch","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	// Non-terminal patch: must not disarm.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"task-live","patch":{"status":"running"}}`)); err != nil {
		t.Fatalf("non-terminal task_updated: %v", err)
	}

	line := []byte(`{"type":"user","parent_tool_use_id":"toolu_parent","message":{"role":"user","content":[{"tool_use_id":"toolu_launch","type":"tool_result","content":[{"type":"text","text":"Agent dispatched; running in the background."}]}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse ack: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Fatalf("non-terminal task_updated must not disarm liveness; meta=%v", meta)
	}
}

// A SCOPED user envelope with no tool_result in it is the subagent's own
// conversation — the task prompt the CLI handed the agent. It is the only
// statement of what the agent was asked to do, so it becomes an
// EventUserText under the launching tool_use.
//
// Captured from a live 2.1.237 spike (/tmp/ao-spike-fst, 2026-08-23): the
// CLI emits it on the INLINE path with `parent_tool_use_id`,
// `subagent_type` and `task_description` alongside, and no `isReplay`.
func TestParseUser_ScopedPromptBecomesUserTextUnderTheLaunch(t *testing.T) {
	parser := NewParser()

	line := []byte(`{"type":"user","uuid":"16630705-4c37-4ca6-8308-61d136289247","parent_tool_use_id":"toolu_01SjD","subagent_type":"Explore","task_description":"find it","session_id":"s","message":{"role":"user","content":[{"type":"text","text":"Say the word BANANA and nothing else."}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("scoped prompt: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	evt := events[0]
	if evt.Kind != provider.EventUserText {
		t.Fatalf("kind = %s, want %s", evt.Kind, provider.EventUserText)
	}
	if evt.Role != "user" || !evt.ContentPresent {
		t.Fatalf("role = %q contentPresent = %v", evt.Role, evt.ContentPresent)
	}
	if evt.Content != "Say the word BANANA and nothing else." {
		t.Fatalf("content = %q", evt.Content)
	}
	if evt.ParentToolUseID != "toolu_01SjD" {
		t.Fatalf("parentToolUseID = %q, want the launch", evt.ParentToolUseID)
	}
	// Triage keys the row AND its dedup on the meta id, never ItemID.
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("decode meta %s: %v", evt.Meta, err)
	}
	if meta["provider_item_id"] != "16630705-4c37-4ca6-8308-61d136289247" {
		t.Fatalf("meta = %v, want the envelope uuid", meta)
	}
}

// String content is the same fact in the other wire shape: the CLI writes
// a bare string when the prompt carries no attachments.
func TestParseUser_ScopedStringPromptBecomesUserText(t *testing.T) {
	parser := NewParser()

	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"user","uuid":"u-1","parent_tool_use_id":"toolu_x","message":{"role":"user","content":"go read the parser"}}`))
	if err != nil {
		t.Fatalf("scoped string prompt: %v", err)
	}
	if len(events) != 1 || events[0].Content != "go read the parser" {
		t.Fatalf("events = %+v", events)
	}
}

// Everything the scoped branch must NOT claim. Each of these would put a
// user bubble under an agent card for something nobody said to it.
func TestParseUser_ScopedPromptExclusions(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"top level prose belongs to the replay echo", `{"type":"user","uuid":"u-1","message":{"role":"user","content":"what the user typed"}}`},
		{"isMeta caveat", `{"type":"user","uuid":"u-1","parent_tool_use_id":"t","isMeta":true,"message":{"role":"user","content":"Caveat: ..."}}`},
		{"compaction summary", `{"type":"user","uuid":"u-1","parent_tool_use_id":"t","isCompactSummary":true,"message":{"role":"user","content":"summary"}}`},
		// The SDK's agent_progress mapper drops isCompactSummary, but preserves
		// isVisibleInTranscriptOnly as isSynthetic. This is provider-declared
		// internal context, not a prompt sent to the child.
		{"synthetic SDK context", `{"type":"user","uuid":"u-1","parent_tool_use_id":"t","isSynthetic":true,"message":{"role":"user","content":"This session is being continued from a previous conversation..."}}`},
		{"transcript-only injection", `{"type":"user","uuid":"u-1","parent_tool_use_id":"t","isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"injected"}}`},
		{"no uuid to dedup on", `{"type":"user","parent_tool_use_id":"t","message":{"role":"user","content":"unkeyed"}}`},
		{"blank text", `{"type":"user","uuid":"u-1","parent_tool_use_id":"t","message":{"role":"user","content":"   "}}`},
		{"image-only content", `{"type":"user","uuid":"u-1","parent_tool_use_id":"t","message":{"role":"user","content":[{"type":"image","source":{}}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := NewParser().ParseLine(testThread, []byte(tc.line))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("expected no events, got %+v", events)
			}
		})
	}
}

// A scoped envelope carrying a tool_result is the agent's own tool echo,
// and stays exactly what it was: one completion, no prompt row. The text
// block riding alongside it is the tool's context, not a message.
func TestParseUser_ScopedToolResultIsStillOnlyACompletion(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","parent_tool_use_id":"toolu_agent","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/a.go"}}]}}`)); err != nil {
		t.Fatalf("nested tool_use: %v", err)
	}

	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"user","uuid":"u-1","parent_tool_use_id":"toolu_agent","message":{"role":"user","content":[{"type":"text","text":"here you go"},{"type":"tool_result","tool_use_id":"toolu_read","content":"package main"}]}}`))
	if err != nil {
		t.Fatalf("scoped tool result: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventToolComplete {
		t.Fatalf("events = %+v, want one tool completion", events)
	}
}

// A tool_result the completion path could not use is still a tool echo.
// Reading it back as prose would put the tool's own output — here the
// text block riding alongside it — into a user bubble under the agent.
func TestParseUser_ScopedMalformedToolResultIsNotAPrompt(t *testing.T) {
	events, err := NewParser().ParseLine(testThread, []byte(
		`{"type":"user","uuid":"u-1","parent_tool_use_id":"toolu_agent","message":{"role":"user","content":[{"type":"text","text":"here you go"},{"type":"tool_result","content":"package main"}]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events, got %+v", events)
	}
}
