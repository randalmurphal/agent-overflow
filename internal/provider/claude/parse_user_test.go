package claude

import (
	"encoding/json"
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
