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
