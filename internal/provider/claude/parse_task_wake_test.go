package claude

// Tests for the §E6b wake: a `system/task_started` with the SAME task_id
// and NO tool_use_id, which the CLI emits when it resumes a PARKED async
// agent (idle with an owned background shell still running) with that
// shell's `<task-notification>` as the prompt. Captured on 2.1.261
// (local_agent_owned_shell_wake_20260908.ndjson). Before 2026-09-08 the
// parser dropped the envelope on its empty tool_use_id, so the launch
// settled at the agent's first stop and every later round leaked under
// the settled card.

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

const wakePromptXML = "<task-notification>\n<task-id>b2ken8z52</task-id>\n<tool-use-id>toolu_shell</tool-use-id>\n<output-file>/tmp/x/tasks/b2ken8z52.output</output-file>\n<status>completed</status>\n<summary>Background command \"sleep 12; echo ONE\" completed (exit code 0)</summary>\n</task-notification>"

func wakeLine(taskID string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "task_started", "task_id": taskID,
		"task_type": "local_agent", "is_backgrounded": true, "spawn_depth": 1,
		"prompt": wakePromptXML,
	})
	return raw
}

func decodeEventMeta(t *testing.T, evt provider.ProviderEvent) map[string]any {
	t.Helper()
	meta := map[string]any{}
	if len(evt.Meta) == 0 {
		return meta
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return meta
}

func TestParseTaskStartedEvent_WakeWithoutToolUseOpensWokenRound(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu-agent","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("launch task_started: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`)); err != nil {
		t.Fatalf("stop: %v", err)
	}

	events, err := parser.ParseLine(testThread, wakeLine("t1"))
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one event for the wake, got %d: %+v", len(events), events)
	}
	wake := events[0]
	if wake.Kind != provider.EventUserText || wake.Role != "user" {
		t.Fatalf("wake = %s/%s, want user_text/user", wake.Kind, wake.Role)
	}
	if wake.ItemID != provider.SubagentWakePromptItemID("toolu_shell") {
		t.Errorf("ItemID = %q, want the waking shell's scope", wake.ItemID)
	}
	if wake.ParentToolUseID != "tu-agent" {
		t.Errorf("ParentToolUseID = %q, want the bound tool_use", wake.ParentToolUseID)
	}
	if want := `Background command "sleep 12; echo ONE" completed (exit code 0)`; wake.Content != want || !wake.ContentPresent {
		t.Errorf("Content = %q, want the block's <summary>", wake.Content)
	}
	meta := decodeEventMeta(t, wake)
	for key, want := range map[string]any{
		"wire_only": true, provider.MetaSubagentWakePromptKey: true, "task_id": "t1",
		provider.MetaWakeTaskIDKey: "b2ken8z52", provider.MetaWakeToolUseIDKey: "toolu_shell", provider.MetaWakeStatusKey: "completed",
	} {
		if meta[key] != want {
			t.Errorf("meta[%s] = %v, want %v", key, meta[key], want)
		}
	}
	for _, absent := range []string{provider.MetaSubagentResumePromptKey, provider.MetaSubagentPromptProvisionalKey, provider.MetaResumeCarrierIDKey, provider.MetaTranscriptRootIDKey} {
		if _, has := meta[absent]; has {
			t.Errorf("meta must not carry %s: a wake is not a resume and nothing ever binds it", absent)
		}
	}

	// The binding did not move: the woken round's terminal still resolves
	// to the launch, and the wake re-armed the agent's liveness.
	if !parser.hasLiveAgentTask("tu-agent") {
		t.Error("wake must mark the bound tool_use live again")
	}
	terminal, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`))
	if err != nil {
		t.Fatalf("woken round terminal: %v", err)
	}
	if len(terminal) != 1 || terminal[0].ItemID != "tu-agent" {
		t.Fatalf("terminal after wake = %+v, want one event on tu-agent", terminal)
	}
}

// A wake after a §E6 rebind names the CARRIER as its parent (the row the
// lifecycle is bound to) and carries the root on the meta, exactly like
// the resume prompt, so triage can place it without a lookup.
func TestParseTaskStartedEvent_WakeAfterRebindCarriesTheRoot(t *testing.T) {
	parser := NewParser()
	for _, line := range []string{
		// The launch tool_use precedes its task_started on the wire; the
		// parser's rebind rule reads a local_agent binding to a tool_use
		// it never saw launch as a resume (the reconnect edge).
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu-agent","name":"Agent","input":{"description":"d","prompt":"p"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu-agent","task_type":"local_agent"}`,
		`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`,
		`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu-carrier","task_type":"local_agent","description":"d","prompt":"PING"}`,
		`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`,
	} {
		if _, err := parser.ParseLine(testThread, []byte(line)); err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
	}
	events, err := parser.ParseLine(testThread, wakeLine("t1"))
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one event, got %d", len(events))
	}
	if events[0].ParentToolUseID != "tu-carrier" {
		t.Errorf("ParentToolUseID = %q, want tu-carrier", events[0].ParentToolUseID)
	}
	if got := decodeEventMeta(t, events[0])[provider.MetaTranscriptRootIDKey]; got != "tu-agent" {
		t.Errorf("transcript_root_id = %v, want tu-agent", got)
	}
}

// A coalesced wake (several blocks in one prompt) lists every summary and
// keys the row on the first block; a prompt with no block at all falls
// back to the raw text.
func TestParseTaskStartedEvent_WakeContentShapes(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu-agent","task_type":"local_agent"}`)); err != nil {
		t.Fatalf("launch: %v", err)
	}
	two := wakePromptXML + "\n" + strings.NewReplacer("b2ken8z52", "b2zxblcim", "toolu_shell", "toolu_other", "sleep 12; echo ONE", "sleep 35; echo TWO").Replace(wakePromptXML)
	raw, _ := json.Marshal(map[string]any{"type": "system", "subtype": "task_started", "task_id": "t1", "task_type": "local_agent", "prompt": two})
	events, err := parser.ParseLine(testThread, raw)
	if err != nil || len(events) != 1 {
		t.Fatalf("coalesced wake: %v / %d events", err, len(events))
	}
	if events[0].ItemID != provider.SubagentWakePromptItemID("toolu_shell") {
		t.Errorf("ItemID = %q, want the first block's scope", events[0].ItemID)
	}
	if want := "Background command \"sleep 12; echo ONE\" completed (exit code 0)\nBackground command \"sleep 35; echo TWO\" completed (exit code 0)"; events[0].Content != want {
		t.Errorf("Content = %q, want both summaries", events[0].Content)
	}

	raw, _ = json.Marshal(map[string]any{"type": "system", "subtype": "task_started", "task_id": "t1", "task_type": "local_agent", "prompt": "plain text"})
	events, err = parser.ParseLine(testThread, raw)
	if err != nil || len(events) != 1 {
		t.Fatalf("plain wake: %v / %d events", err, len(events))
	}
	if events[0].Content != "plain text" || !strings.HasPrefix(events[0].ItemID, "user:subagent-wake:t1:") {
		t.Errorf("plain wake = %q / %q, want the raw prompt keyed on the task", events[0].Content, events[0].ItemID)
	}
}

func TestParseTaskStartedEvent_WakeWithoutBindingOrForShellIsNoOp(t *testing.T) {
	parser := NewParser()
	if events, err := parser.ParseLine(testThread, wakeLine("unknown")); err != nil || len(events) != 0 {
		t.Fatalf("unknown task: %v / %+v, want nothing (no row to place it under)", err, events)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"tu-shell","task_type":"local_bash"}`)); err != nil {
		t.Fatalf("shell task_started: %v", err)
	}
	line, _ := json.Marshal(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "task_type": "local_bash", "prompt": "x"})
	if events, err := parser.ParseLine(testThread, line); err != nil || len(events) != 0 {
		t.Fatalf("local_bash without tool_use_id: %v / %+v, want nothing", err, events)
	}
}

// Replays the 2.1.261 capture end to end: launch, two owned background
// shells, the agent's park at its first stop, two shell-driven wakes
// each followed by a stop. Pins the event shape triage's park model reads.
func TestReplayLocalAgentOwnedShellWake(t *testing.T) {
	const launch = "toolu_01RBwwkzBxziR7C1gV2rGxPp"
	events := replayFixture(t, "../../../docs/references/fixtures/claude/local_agent_owned_shell_wake_20260908.ndjson")

	var wakes []provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventUserText && decodeEventMeta(t, evt)[provider.MetaSubagentWakePromptKey] == true {
			wakes = append(wakes, evt)
		}
	}
	if len(wakes) != 2 {
		t.Fatalf("expected 2 wake prompts (one per owned shell), got %d", len(wakes))
	}
	wantIDs := []string{
		provider.SubagentWakePromptItemID("toolu_01C1sBpMABwBJ4cT5J1PhGqR"),
		provider.SubagentWakePromptItemID("toolu_01PexqSz5zkz2F8pUD8F8K72"),
	}
	for i, wake := range wakes {
		if wake.ItemID != wantIDs[i] || wake.ParentToolUseID != launch {
			t.Errorf("wake %d = %s under %s, want %s under %s", i, wake.ItemID, wake.ParentToolUseID, wantIDs[i], launch)
		}
	}
	if !strings.Contains(wakes[1].Content, "failed with exit code 3") {
		t.Errorf("second wake content = %q, want the failed shell's summary", wakes[1].Content)
	}

	// Three agent stops, all on the launch (the wake never rebinds), two
	// shell terminals on their own tool_uses.
	agentTerminals, agentNotifications := 0, 0
	for _, evt := range events {
		meta := decodeEventMeta(t, evt)
		switch evt.Kind {
		case provider.EventBackgroundTaskTerminal:
			if meta["task_id"] == "ac1bb517c3154d44b" {
				if evt.ItemID != launch {
					t.Errorf("agent terminal on %q, want %s", evt.ItemID, launch)
				}
				agentTerminals++
			}
		case provider.EventBackgroundTaskNotification:
			if meta["task_id"] == "ac1bb517c3154d44b" {
				if evt.ItemID != launch {
					t.Errorf("agent notification on %q, want %s", evt.ItemID, launch)
				}
				agentNotifications++
			}
		}
	}
	if agentTerminals != 3 || agentNotifications != 3 {
		t.Errorf("agent terminals/notifications = %d/%d, want 3/3 (one per stop)", agentTerminals, agentNotifications)
	}
}
