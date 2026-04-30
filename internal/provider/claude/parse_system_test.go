package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseTaskLifecycleEvent_CompletedPatchEmitsBackgroundTerminal is
// the basic happy-path assertion: a task_updated envelope with
// `patch.status=completed` emits exactly one
// `EventBackgroundTaskTerminal`. No `EventToolComplete` is produced —
// the tool-lifecycle completion was already emitted by the
// backgrounded placeholder's `tool_result`.
func TestParseTaskLifecycleEvent_CompletedPatchEmitsBackgroundTerminal(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu1"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}

	events, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed","end_time":1776577311261}}`))
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventBackgroundTaskTerminal)
	}
	if events[0].ItemID != "tu1" {
		t.Fatalf("ItemID: got %q, want tu1", events[0].ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["task_id"] != "t1" {
		t.Fatalf("meta.task_id: got %v", meta["task_id"])
	}
	if meta["status"] != "completed" {
		t.Fatalf("meta.status: got %v", meta["status"])
	}
	if meta["end_time"] != float64(1776577311261) {
		t.Fatalf("meta.end_time: got %v", meta["end_time"])
	}
}

// TestParseTaskLifecycleEvent_FailedPatchMarksIsError covers the
// `failed` terminal mapping — the event is normalized to status=failed
// and is_error=true. `killed` has its own distinct status (see
// TestParseTaskLifecycleEvent_KilledPatchPreservesStatus) so it can
// render as a gray "Stopped" badge rather than the red "Failed" bucket.
func TestParseTaskLifecycleEvent_FailedPatchMarksIsError(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu1"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	line := []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"failed"}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("Kind: got %q", events[0].Kind)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["status"] != "failed" {
		t.Fatalf("meta.status: got %v, want failed", meta["status"])
	}
	if meta["is_error"] != true {
		t.Fatalf("meta.is_error: got %v, want true", meta["is_error"])
	}
}

// TestParseTaskLifecycleEvent_KilledPatchPreservesStatus confirms the
// `killed` wire value survives the parser normalizer untouched. The CLI
// emits this on the follow-up task_updated after a successful
// stop_task control_request; triage maps it onto the distinct
// `statusKilled` row state so the UI can render a "Stopped" badge
// instead of conflating user-initiated stops with errors.
func TestParseTaskLifecycleEvent_KilledPatchPreservesStatus(t *testing.T) {
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu1"}`)); err != nil {
		t.Fatalf("task_started: %v", err)
	}
	line := []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"killed","end_time":1776915081647}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("Kind: got %q", events[0].Kind)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["status"] != "killed" {
		t.Fatalf("meta.status: got %v, want killed", meta["status"])
	}
	if meta["is_error"] != true {
		t.Fatalf("meta.is_error: got %v, want true (killed is still a non-completed terminal)", meta["is_error"])
	}
	if meta["end_time"] != float64(1776915081647) {
		t.Fatalf("meta.end_time: got %v, want 1776915081647", meta["end_time"])
	}
}

// TestParseTaskLifecycleEvent_NonTerminalPatchIsNoOp confirms that an
// intermediate `patch.status` (e.g. `running`, `pending`) does not
// produce an event. Only the terminal statuses are authoritative.
func TestParseTaskLifecycleEvent_NonTerminalPatchIsNoOp(t *testing.T) {
	parser := NewParser()
	for _, wireStatus := range []string{"pending", "running", "queued", ""} {
		t.Run(wireStatus, func(t *testing.T) {
			line := []byte(`{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"` + wireStatus + `"}}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("expected 0 events for non-terminal status %q, got %+v", wireStatus, events)
			}
		})
	}
}

// TestParseTaskNotificationEvent_EmitsNotificationOnly is the invariant-21
// guard: task_notification emits a distinct notification event, never a
// lifecycle terminal.
func TestParseTaskNotificationEvent_EmitsNotificationOnly(t *testing.T) {
	parser := NewParser()

	cases := []string{
		`{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu1","status":"completed","summary":"done"}`,
		`{"type":"system","subtype":"task_notification","task_id":"t1","status":"failed","summary":"whoops"}`,
		`{"type":"system","subtype":"task_notification","task_id":"t-fg","tool_use_id":"tu-fg","status":"completed","summary":"Background command \"echo\" completed (exit code 0)","output_file":""}`, // foreground bash form
	}
	for _, line := range cases {
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if len(events) != 1 || events[0].Kind != provider.EventBackgroundTaskNotification {
			t.Errorf("task_notification must emit one notification event for %s, got %+v", line, events)
			continue
		}
		if events[0].Kind == provider.EventBackgroundTaskTerminal {
			t.Fatalf("task_notification emitted lifecycle terminal: %+v", events[0])
		}
	}

	for _, line := range []string{
		`{"type":"system","subtype":"task_notification","tool_use_id":"tu1"}`,
		`{"type":"system","subtype":"task_notification"}`,
	} {
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if len(events) != 0 {
			t.Errorf("malformed task_notification should emit no event for %s, got %+v", line, events)
		}
	}
}

// TestParseResult_PopulatesAssistantMessageID verifies that the
// parser tracks the last `assistant.message.id` and stamps it on
// `EventTurnComplete.Meta.assistant_message_id`. The id resets after
// emission so the next turn's result starts fresh.
func TestParseResult_PopulatesAssistantMessageID(t *testing.T) {
	parser := NewParser()

	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg_first","role":"assistant","content":[{"type":"text","text":"hi"}]}}`)); err != nil {
		t.Fatalf("first assistant: %v", err)
	}
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg_second","role":"assistant","content":[{"type":"text","text":"still me"}]}}`)); err != nil {
		t.Fatalf("second assistant: %v", err)
	}

	events, err := parser.ParseLine(testThread, []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","duration_ms":12345,"total_cost_usd":0.12,"usage":{"input_tokens":50,"output_tokens":100}}`))
	if err != nil {
		t.Fatalf("result: %v", err)
	}

	var turn provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			turn = evt
			break
		}
	}
	if turn.Kind != provider.EventTurnComplete {
		t.Fatalf("no EventTurnComplete in %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(turn.Meta, &meta); err != nil {
		t.Fatalf("unmarshal turn meta: %v", err)
	}
	if meta["assistant_message_id"] != "msg_second" {
		t.Fatalf("assistant_message_id: got %v, want msg_second", meta["assistant_message_id"])
	}
	if meta["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason: got %v", meta["stop_reason"])
	}
	if meta["duration_ms"] != float64(12345) {
		t.Fatalf("duration_ms: got %v", meta["duration_ms"])
	}
	if meta["total_cost_usd"] != 0.12 {
		t.Fatalf("total_cost_usd: got %v", meta["total_cost_usd"])
	}

	// Emit a second result. lastAssistantMessageID must have cleared
	// so the next turn's meta does not carry stale id.
	events, err = parser.ParseLine(testThread, []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatalf("second result: %v", err)
	}
	for _, evt := range events {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		var m2 map[string]any
		if err := json.Unmarshal(evt.Meta, &m2); err != nil {
			t.Fatalf("unmarshal second turn meta: %v", err)
		}
		if v, ok := m2["assistant_message_id"]; ok {
			t.Fatalf("second turn still carries assistant_message_id=%v — take should have cleared it", v)
		}
	}
}

func TestParseResultUsageNeverEmitsContextWindow(t *testing.T) {
	tests := []struct {
		name  string
		prime []byte
	}{
		{
			name: "no prior usage",
		},
		{
			name:  "after assistant usage",
			prime: []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[],"usage":{"input_tokens":100,"output_tokens":50}}}`),
		},
		{
			name:  "after message_delta usage",
			prime: []byte(`{"type":"stream_event","event":"message_delta","data":{"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50}}}`),
		},
	}
	resultLine := []byte(`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":40,"cache_creation_input_tokens":10,"iterations":[{"input_tokens":700,"output_tokens":100},{"input_tokens":800,"output_tokens":200,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}]}}`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			if len(tt.prime) > 0 {
				if _, err := parser.ParseLine(testThread, tt.prime); err != nil {
					t.Fatalf("prime: %v", err)
				}
			}

			events, err := parser.ParseLine(testThread, resultLine)
			if err != nil {
				t.Fatalf("result: %v", err)
			}

			var complete provider.ProviderEvent
			for _, evt := range events {
				switch evt.Kind {
				case provider.EventTokenUsage:
					t.Fatalf("result.usage emitted context update: %+v", evt)
				case provider.EventTurnComplete:
					complete = evt
				}
			}
			if complete.Kind != provider.EventTurnComplete {
				t.Fatalf("expected EventTurnComplete, got %+v", events)
			}
		})
	}
}

// TestParseResult_InterruptedMarksAbortedAndStopReason verifies the
// interrupted-turn heuristic (forge sdkMessageParsing.ts:112-125):
// subtype=error_during_execution + is_error=false + errors[] containing
// "aborted" promotes stop_reason to "interrupted" and sets
// meta.aborted=true.
func TestParseResult_InterruptedMarksAbortedAndStopReason(t *testing.T) {
	parser := NewParser()

	// Seed an assistant id so we can also verify the id flows into meta.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg_x","role":"assistant","content":[{"type":"text","text":"working"}]}}`)); err != nil {
		t.Fatalf("assistant: %v", err)
	}

	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":false,"errors":["user aborted the turn"],"duration_ms":999}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	var turn provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			turn = evt
			break
		}
	}
	if turn.Kind != provider.EventTurnComplete {
		t.Fatalf("expected EventTurnComplete, got %+v", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(turn.Meta, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["aborted"] != true {
		t.Fatalf("meta.aborted: got %v, want true", meta["aborted"])
	}
	if meta["stop_reason"] != "interrupted" {
		t.Fatalf("meta.stop_reason: got %v, want interrupted", meta["stop_reason"])
	}
	if meta["assistant_message_id"] != "msg_x" {
		t.Fatalf("assistant_message_id: got %v, want msg_x", meta["assistant_message_id"])
	}
}

// TestParseResult_NonInterruptedErrorPopulatesMetaError — an
// `error_during_execution` subtype whose errors[] does NOT match the
// interrupt heuristic still produces a turn-complete error: meta.error
// is populated from errors[], stop_reason flips to "error", and
// meta.aborted stays unset. Hard failures and user aborts split here.
func TestParseResult_NonInterruptedErrorPopulatesMetaError(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":false,"errors":["disk full"]}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	var turn provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			turn = evt
			break
		}
	}
	if turn.Kind != provider.EventTurnComplete {
		t.Fatalf("expected EventTurnComplete, got %+v", events)
	}
	var meta map[string]any
	_ = json.Unmarshal(turn.Meta, &meta)
	if v, ok := meta["aborted"]; ok && v == true {
		t.Fatalf("non-interrupt errors[] should not trigger aborted flag, got %v", v)
	}
	if meta["stop_reason"] != "error" {
		t.Fatalf("meta.stop_reason: got %v, want error", meta["stop_reason"])
	}
	if meta["error"] != "disk full" {
		t.Fatalf("meta.error: got %v, want \"disk full\"", meta["error"])
	}
	if meta["subtype"] != "error_during_execution" {
		t.Fatalf("meta.subtype: got %v, want error_during_execution", meta["subtype"])
	}
}

// TestParseResult_IsErrorTrueWithoutSubtypeEmitsTurnComplete pins
// behaviour for the bare `is_error:true` shape (no subtype). The
// envelope settles the turn but does NOT populate meta.error: the
// error-routing branches require either an error_* subtype or
// `subtype:"success"`. Defensive — keeps the parser from inventing an
// error message for an undocumented wire shape.
func TestParseResult_IsErrorTrueWithoutSubtypeEmitsTurnComplete(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","is_error":true,"errors":["boom"]}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnComplete {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventTurnComplete)
	}
}

// TestParseResult_AllErrorSubtypesPopulateMetaError covers the four
// SDKResultError subtypes documented in the agent SDK. Each one must
// produce stop_reason="error" + meta.error from errors[], so triage
// can route the turn-complete to the same error-handling path
// regardless of which limit/cause tripped.
func TestParseResult_AllErrorSubtypesPopulateMetaError(t *testing.T) {
	subtypes := []string{
		"error_during_execution",
		"error_max_turns",
		"error_max_budget_usd",
		"error_max_structured_output_retries",
	}
	for _, subtype := range subtypes {
		t.Run(subtype, func(t *testing.T) {
			parser := NewParser()
			line := []byte(`{"type":"result","subtype":"` + subtype + `","is_error":false,"errors":["limit reached"]}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("result: %v", err)
			}
			var turn provider.ProviderEvent
			for _, evt := range events {
				if evt.Kind == provider.EventTurnComplete {
					turn = evt
					break
				}
			}
			if turn.Kind != provider.EventTurnComplete {
				t.Fatalf("expected EventTurnComplete, got %+v", events)
			}
			var meta map[string]any
			_ = json.Unmarshal(turn.Meta, &meta)
			if meta["stop_reason"] != "error" {
				t.Fatalf("meta.stop_reason: got %v, want error", meta["stop_reason"])
			}
			if meta["error"] != "limit reached" {
				t.Fatalf("meta.error: got %v, want \"limit reached\"", meta["error"])
			}
			if meta["subtype"] != subtype {
				t.Fatalf("meta.subtype: got %v, want %s", meta["subtype"], subtype)
			}
			// Aborted must NOT be set — these are hard failures, not user
			// interrupts. Only error_during_execution + matching errors[]
			// flips aborted, and "limit reached" doesn't match.
			if v, ok := meta["aborted"]; ok && v == true {
				t.Fatalf("hard error should not set aborted, got %v", v)
			}
		})
	}
}

// TestParseResult_SuccessSubtypeIsErrorTruePopulatesMetaError covers
// the carve-out where Claude follows a fatal `assistant.error` with a
// `result{subtype:"success", is_error:true}` envelope (the agent SDK's
// documented shape — see parse_assistant.go for the producer). The
// "success" label is the API call's transport status, but is_error
// flags the turn outcome. We must populate meta.error from errors[]
// so the working indicator clears as a failure, not a success.
func TestParseResult_SuccessSubtypeIsErrorTruePopulatesMetaError(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"success","is_error":true,"errors":["rate_limit"]}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	var turn provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			turn = evt
			break
		}
	}
	if turn.Kind != provider.EventTurnComplete {
		t.Fatalf("expected EventTurnComplete, got %+v", events)
	}
	var meta map[string]any
	_ = json.Unmarshal(turn.Meta, &meta)
	if meta["stop_reason"] != "error" {
		t.Fatalf("meta.stop_reason: got %v, want error", meta["stop_reason"])
	}
	if meta["error"] != "rate_limit" {
		t.Fatalf("meta.error: got %v, want rate_limit", meta["error"])
	}
}

// TestParseResult_TerminalReasonPassesThroughOnMeta — `terminal_reason`
// is a 12-value enum we pass through on Meta for telemetry / forward
// compat. Triage doesn't branch on it, but the field must not get
// dropped on the round-trip.
func TestParseResult_TerminalReasonPassesThroughOnMeta(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"end_turn"}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	var turn provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			turn = evt
			break
		}
	}
	if turn.Kind != provider.EventTurnComplete {
		t.Fatalf("expected EventTurnComplete, got %+v", events)
	}
	var meta map[string]any
	_ = json.Unmarshal(turn.Meta, &meta)
	if meta["terminal_reason"] != "end_turn" {
		t.Fatalf("meta.terminal_reason: got %v, want end_turn", meta["terminal_reason"])
	}
}
