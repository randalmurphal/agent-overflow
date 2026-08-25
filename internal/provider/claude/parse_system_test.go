package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func requireWireTurnComplete(t *testing.T, events []provider.ProviderEvent) provider.WireTurnCompleteMeta {
	t.Helper()
	for _, evt := range events {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		switch meta := evt.TurnComplete.(type) {
		case *provider.WireTurnCompleteMeta:
			if meta != nil {
				return *meta
			}
		default:
			t.Fatalf("EventTurnComplete meta type = %T, want WireTurnCompleteMeta", evt.TurnComplete)
		}
	}
	t.Fatalf("no EventTurnComplete in %+v", events)
	return provider.WireTurnCompleteMeta{}
}

func TestParseSystem_InitCarriesMCPServers(t *testing.T) {
	// Real-shape `system/init` payload captured from the Claude CLI. The
	// parser must promote `mcp_servers` into SessionInfo so the app
	// layer can feed mcpstatus without re-parsing the wire.
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess1","cwd":"/tmp","tools":["Read","Bash"],"model":"claude-opus-4-7[1m]","mcp_servers":[{"name":"github","status":"connected"},{"name":"linear","status":"needs-auth"},{"name":"slow-starter","status":"pending"}],"claude_code_version":"2.1.139","uuid":"u1"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventInit {
		t.Fatalf("events = %+v, want one EventInit", events)
	}

	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(info.MCPServers) != 3 {
		t.Fatalf("MCPServers len = %d, want 3 (got %+v)", len(info.MCPServers), info.MCPServers)
	}
	wantStatuses := map[string]string{
		"github":       "connected",
		"linear":       "needs-auth",
		"slow-starter": "pending",
	}
	for _, s := range info.MCPServers {
		want, ok := wantStatuses[s.Name]
		if !ok {
			t.Errorf("unexpected server: %q", s.Name)
			continue
		}
		if s.Status != want {
			t.Errorf("%q: status = %q, want %q", s.Name, s.Status, want)
		}
	}
}

func TestParseSystem_InitWithoutMCPServersIsBenign(t *testing.T) {
	// `mcp_servers` is absent when the user has no MCPs configured;
	// the parser must not blow up or invent entries.
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess1","cwd":"/tmp","model":"claude-opus-4-7[1m]","uuid":"u1"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(info.MCPServers) != 0 {
		t.Fatalf("MCPServers should be empty, got %+v", info.MCPServers)
	}
}

func TestParseSystem_APIRetryTopLevelPayload(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","attempt":10,"max_retries":10,"retry_delay_ms":35073.75745568816,"error_status":529,"error":"rate_limit","session_id":"s1","uuid":"u1"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventAPIRetry {
		t.Fatalf("events = %+v, want one api_retry", events)
	}
	if events[0].Failure == nil || events[0].Failure.Class != provider.FailureTransient {
		t.Fatalf("failure = %+v, want normalized transient", events[0].Failure)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["attempt"] != float64(10) {
		t.Fatalf("attempt = %v, want 10", meta["attempt"])
	}
	if meta["max_retries"] != float64(10) {
		t.Fatalf("max_retries = %v, want 10", meta["max_retries"])
	}
	if meta["error"] != "rate_limit" {
		t.Fatalf("error = %v, want rate_limit", meta["error"])
	}
}

func TestParseSystem_APIRetryConnectionResetIsTransient(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","data":{"attempt":1,"error":{"connection":{"code":"ECONNRESET"}}}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Failure == nil || events[0].Failure.Class != provider.FailureTransient {
		t.Fatalf("events = %+v, want normalized transient", events)
	}
}

func TestParseSystem_ModelRefusalFallbackCarriesReasonAndModels(t *testing.T) {
	parser := NewParser()
	parser.SetModel("claude-fable-5")
	line := []byte(`{"type":"system","subtype":"model_refusal_fallback","content":"Fable 5's safeguards flagged this message. Switched to Opus 4.8.","trigger":"refusal","originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","requestId":"req_fallback_1","apiRefusalCategory":"cyber","apiRefusalExplanation":"security-sensitive request","refusedUserMessageUuid":"user-1","uuid":"system-1"}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventModelFallback {
		t.Fatalf("events = %+v, want one model fallback", events)
	}
	evt := events[0]
	if evt.ItemID != "model-fallback:model_refusal_fallback:req_fallback_1" {
		t.Fatalf("ItemID = %q", evt.ItemID)
	}
	if evt.Content != "Fable 5's safeguards flagged this message. Switched to Opus 4.8." {
		t.Fatalf("Content = %q", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["originalModel"] != "claude-fable-5" || meta["fallbackModel"] != "claude-opus-4-8" {
		t.Fatalf("model metadata = %+v", meta)
	}
	if meta["apiRefusalCategory"] != "cyber" || meta["refusedUserMessageUuid"] != "user-1" {
		t.Fatalf("reason metadata = %+v", meta)
	}
	if meta["apiRefusalExplanation"] != "security-sensitive request" {
		t.Fatalf("explanation metadata = %+v", meta)
	}
	if got := parser.currentModel(); got != "claude-opus-4-8" {
		t.Fatalf("current model = %q, want fallback model", got)
	}
}

func TestParseSystem_ModelRefusalFallbackRejectsMissingFallbackModel(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"model_refusal_fallback","content":"flagged","originalModel":"claude-fable-5"}`)
	if _, err := ParseLine(testThread, line); err == nil {
		t.Fatal("parse error = nil, want missing fallbackModel failure")
	}
}

func TestClaudeFallbackFieldsAreBounded(t *testing.T) {
	unsafeID := strings.Repeat("request/", 100)
	itemID := claudeFallbackItemID("model_refusal_fallback", unsafeID)
	wantPrefix := "model-fallback:model_refusal_fallback:sha256:"
	if !strings.HasPrefix(itemID, wantPrefix) || len(itemID) != len(wantPrefix)+32 {
		t.Fatalf("hashed item ID = %q", itemID)
	}
	if got := boundedClaudeFallbackField(strings.Repeat("m", maxClaudeFallbackModelRunes+20), maxClaudeFallbackModelRunes); len([]rune(got)) != maxClaudeFallbackModelRunes {
		t.Fatalf("bounded model runes = %d", len([]rune(got)))
	}
}

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
// the typed turn-complete payload. The id resets after
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

	meta := requireWireTurnComplete(t, events)
	if meta.AssistantMessageID != "msg_second" {
		t.Fatalf("assistant_message_id: got %v, want msg_second", meta.AssistantMessageID)
	}
	if meta.StopReason != "end_turn" {
		t.Fatalf("stop_reason: got %v", meta.StopReason)
	}
	if meta.Usage == nil {
		t.Fatal("usage missing")
	}
	if meta.Usage.InputTokens != 50 || meta.Usage.OutputTokens != 100 {
		t.Fatalf("usage = %+v, want input=50 output=100", meta.Usage)
	}
	if meta.Usage.TotalCostUSD != 0.12 {
		t.Fatalf("usage total cost = %v, want 0.12", meta.Usage.TotalCostUSD)
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
		meta, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
		if !ok || meta == nil {
			t.Fatalf("second turn meta type = %T, want WireTurnCompleteMeta", evt.TurnComplete)
		}
		if meta.AssistantMessageID != "" {
			t.Fatalf("second turn still carries assistant_message_id=%v — take should have cleared it", meta.AssistantMessageID)
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
// Aborted=true.
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
	meta := requireWireTurnComplete(t, events)
	if !meta.Aborted {
		t.Fatalf("Aborted: got false, want true")
	}
	if meta.StopReason != "interrupted" {
		t.Fatalf("StopReason: got %v, want interrupted", meta.StopReason)
	}
	if meta.AssistantMessageID != "msg_x" {
		t.Fatalf("assistant_message_id: got %v, want msg_x", meta.AssistantMessageID)
	}
}

// TestParseResult_NonInterruptedErrorPopulatesMetaError — an
// `error_during_execution` subtype whose errors[] does NOT match the
// interrupt heuristic still produces a turn-complete error: ErrorMessage is
// populated from errors[], stop_reason flips to "error", and Aborted stays
// unset. Hard failures and user aborts split here.
func TestParseResult_NonInterruptedErrorPopulatesMetaError(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":false,"errors":["disk full"]}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	if meta.Aborted {
		t.Fatal("non-interrupt errors[] should not trigger aborted flag")
	}
	if meta.StopReason != "error" {
		t.Fatalf("StopReason: got %v, want error", meta.StopReason)
	}
	if meta.ErrorMessage != "disk full" {
		t.Fatalf("ErrorMessage: got %v, want \"disk full\"", meta.ErrorMessage)
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
// produce stop_reason="error" + ErrorMessage from errors[], so triage
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
			meta := requireWireTurnComplete(t, events)
			if meta.StopReason != "error" {
				t.Fatalf("StopReason: got %v, want error", meta.StopReason)
			}
			if meta.ErrorMessage != "limit reached" {
				t.Fatalf("ErrorMessage: got %v, want \"limit reached\"", meta.ErrorMessage)
			}
			// Aborted must NOT be set — these are hard failures, not user
			// interrupts. Only error_during_execution + matching errors[]
			// flips aborted, and "limit reached" doesn't match.
			if meta.Aborted {
				t.Fatal("hard error should not set aborted")
			}
		})
	}
}

// TestParseResult_SuccessSubtypeIsErrorTruePopulatesMetaError covers
// the carve-out where Claude follows a fatal `assistant.error` with a
// `result{subtype:"success", is_error:true}` envelope (the agent SDK's
// documented shape — see parse_assistant.go for the producer). The
// "success" label is the API call's transport status, but is_error
// flags the turn outcome. We must populate ErrorMessage from errors[]
// so the working indicator clears as a failure, not a success.
func TestParseResult_SuccessSubtypeIsErrorTruePopulatesMetaError(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"result","subtype":"success","is_error":true,"errors":["rate_limit"]}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	meta := requireWireTurnComplete(t, events)
	if meta.StopReason != "error" {
		t.Fatalf("StopReason: got %v, want error", meta.StopReason)
	}
	if meta.ErrorMessage != "rate_limit" {
		t.Fatalf("ErrorMessage: got %v, want rate_limit", meta.ErrorMessage)
	}
}

// -- system/status tests (claude-wire.md §system/status; envelopes verified
// against real 2.1.219 captures, manual /compact + auto-compact runs of
// 2026-08-05) --

func requireCompactionStatus(t *testing.T, events []provider.ProviderEvent) provider.CompactionStatusMeta {
	t.Helper()
	if len(events) != 1 || events[0].Kind != provider.EventCompactionStatus {
		t.Fatalf("events = %+v, want one EventCompactionStatus", events)
	}
	var meta provider.CompactionStatusMeta
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return meta
}

func TestParseSystem_StatusCompactingOpensWindow(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"status","status":"compacting","session_id":"s1","uuid":"u1"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	meta := requireCompactionStatus(t, events)
	if !meta.Active || meta.Result != "" || meta.ErrorMessage != "" {
		t.Fatalf("meta = %+v, want bare Active=true", meta)
	}
}

func TestParseSystem_StatusCompactResultSuccessClosesWindow(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"status","status":null,"compact_result":"success","session_id":"s1","uuid":"u2"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	meta := requireCompactionStatus(t, events)
	if meta.Active || meta.Result != "success" || meta.ErrorMessage != "" {
		t.Fatalf("meta = %+v, want inactive success", meta)
	}
}

func TestParseSystem_StatusCompactResultFailedCarriesError(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"status","status":null,"compact_result":"failed","compact_error":"API Error: Request was aborted.","session_id":"s1","uuid":"u3"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	meta := requireCompactionStatus(t, events)
	if meta.Active || meta.Result != "failed" {
		t.Fatalf("meta = %+v, want inactive failed", meta)
	}
	if meta.ErrorMessage != "API Error: Request was aborted." {
		t.Fatalf("ErrorMessage = %q", meta.ErrorMessage)
	}
}

// `status:"requesting"` fires on every API request during ordinary turns;
// it must stay dropped — turn activity is already wire-pushed through the
// round lifecycle. Unknown values take the same path: this channel is
// additive and a new value must not become an event nothing routes.
func TestParseSystem_StatusRequestingAndUnknownDropped(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"status","status":"requesting","session_id":"s1","uuid":"u4"}`,
		`{"type":"system","subtype":"status","status":"defragmenting","session_id":"s1","uuid":"u5"}`,
		`{"type":"system","subtype":"status","status":null,"session_id":"s1","uuid":"u6"}`,
	} {
		events, err := ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		if len(events) != 0 {
			t.Fatalf("events for %s = %+v, want none", line, events)
		}
	}
}

// TestParseSystem_ModelRefusalFallbackReadsWireSnakeCase is the
// regression guard for a casing mismatch that made every REAL
// `model_refusal_fallback` envelope a parse error. The stream-json wire
// spells this family snake_case (verified in the serializers of 2.1.214,
// 2.1.219 and 2.1.237: `original_model`, `fallback_model`,
// `api_refusal_category`, …); the parser read camelCase only, which is
// the CLI's INTERNAL object shape, so `fallback_model` never resolved and
// the line was rejected with "empty fallback_model". Both spellings are
// accepted now — the camelCase half is what the passthrough branch of one
// serializer path emits verbatim.
func TestParseSystem_ModelRefusalFallbackReadsWireSnakeCase(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"system","subtype":"model_refusal_fallback","content":"Switched to Opus 4.8.","trigger":"refusal","direction":"retry","scope":"session","original_model":"claude-fable-5","fallback_model":"claude-opus-4-8","request_id":"req_snake_1","api_refusal_category":"cyber","api_refusal_explanation":"security-sensitive request","refused_user_message_uuid":"user-1","uuid":"system-1"}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventModelFallback {
		t.Fatalf("events = %+v, want one model fallback", events)
	}
	if events[0].ItemID != "model-fallback:model_refusal_fallback:req_snake_1" {
		t.Fatalf("ItemID = %q, want the snake_case request_id", events[0].ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["originalModel"] != "claude-fable-5" || meta["fallbackModel"] != "claude-opus-4-8" {
		t.Fatalf("model metadata = %+v", meta)
	}
	if meta["apiRefusalCategory"] != "cyber" || meta["apiRefusalExplanation"] != "security-sensitive request" {
		t.Fatalf("refusal metadata = %+v", meta)
	}
	if got := parser.currentModel(); got != "claude-opus-4-8" {
		t.Fatalf("current model = %q, want fallback model", got)
	}
}

// TestParseSystem_ModelFallbackRoutesLikeARefusalFallback covers
// `model_fallback` (the availability sibling: model_not_found /
// permission_denied / model_blocked). The user-visible consequence is
// identical to a refusal fallback — the turn continues on another model —
// so it lands on the same event; only meta.kind and the trigger differ.
func TestParseSystem_ModelFallbackRoutesLikeARefusalFallback(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"system","subtype":"model_fallback","uuid":"sys-2","trigger":"model_not_found","original_model":"claude-fable-5","fallback_model":"claude-opus-4-8","content":"Switched to Opus 4.8 because Fable 5 is not available"}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventModelFallback {
		t.Fatalf("events = %+v, want one model fallback", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["kind"] != "model_fallback" || meta["trigger"] != "model_not_found" {
		t.Fatalf("meta = %+v", meta)
	}
	if events[0].Content != "Switched to Opus 4.8 because Fable 5 is not available" {
		t.Fatalf("Content = %q", events[0].Content)
	}
	if got := parser.currentModel(); got != "claude-opus-4-8" {
		t.Fatalf("current model = %q, want fallback model", got)
	}
}

// TestParseSystem_ModelConsentFallbackCarriesChoiceAndPersistence covers
// `model_consent_fallback` — the credits/consent sibling. Its two extra
// fields are what make the notice actionable: WHICH consent choice
// applied, and whether it was written back as the account default rather
// than for this session only.
func TestParseSystem_ModelConsentFallbackCarriesChoiceAndPersistence(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"model_consent_fallback","uuid":"sys-3","choice":"fallback","original_model":"claude-fable-5","fallback_model":"claude-opus-4-8","persisted_as_default":true,"content":"Switched to Opus 4.8 — now your default model"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventModelFallback {
		t.Fatalf("events = %+v, want one model fallback", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["kind"] != "model_consent_fallback" || meta["choice"] != "fallback" {
		t.Fatalf("meta = %+v", meta)
	}
	if persisted, _ := meta["persistedAsDefault"].(bool); !persisted {
		t.Fatalf("persistedAsDefault = %v, want true", meta["persistedAsDefault"])
	}
}

// TestParseSystem_ModelRefusalNoFallbackIsAnError is the one member of
// the family whose turn produces nothing: the request was refused and no
// fallback route matched. Core principle 5 — that is user-facing error
// state, not an informational "now running as X" notice.
func TestParseSystem_ModelRefusalNoFallbackIsAnError(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"model_refusal_no_fallback","uuid":"sys-4","original_model":"claude-fable-5","request_id":"req_nf_1","api_refusal_category":"cyber","api_refusal_explanation":"security-sensitive request","refused_user_message_uuid":"user-9","content":""}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventError {
		t.Fatalf("events = %+v, want one EventError", events)
	}
	// The live producer sends content:"" — a notice with no sentence is
	// worse than a composed one, so the copy is built from the fields.
	if !strings.Contains(events[0].Content, "claude-fable-5") ||
		!strings.Contains(events[0].Content, "no fallback model") {
		t.Fatalf("Content = %q, want composed refusal copy naming the model", events[0].Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["kind"] != "model_refusal_no_fallback" || meta["apiRefusalCategory"] != "cyber" {
		t.Fatalf("meta = %+v", meta)
	}
	// Triage reads a top-level `fatal` as "the provider process is gone"
	// and a top-level `error` string as an SDK error enum. Neither is true
	// here: only the turn died.
	if _, present := meta["fatal"]; present {
		t.Fatalf("meta must not claim fatal: %+v", meta)
	}
	if _, present := meta["error"]; present {
		t.Fatalf("meta must not carry a top-level error enum: %+v", meta)
	}
}

// TestParseSystem_APIErrorNormalizesOntoTheRetrySurface covers the
// richer wire twin of `api_retry`. Both envelopes are emitted for one
// retryable failure, and triage upserts a single per-turn `retry:<n>`
// row, so routing them to the same event collapses the pair instead of
// double-rendering. The twin spells every counter differently, and
// carries the display string (`error.formatted`) plus the status nested
// under `error` rather than flat.
func TestParseSystem_APIErrorNormalizesOntoTheRetrySurface(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_error","level":"error","uuid":"sys-5","retryAttempt":4,"maxRetries":10,"retryInMs":2000,"error":{"message":"Overloaded","status":529,"formatted":"API is temporarily overloaded","connection":null,"is_network_down":false,"rate_limits":null}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventAPIRetry {
		t.Fatalf("events = %+v, want one EventAPIRetry", events)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if attempt, _ := meta["attempt"].(float64); attempt != 4 {
		t.Fatalf("attempt = %v, want 4 from retryAttempt", meta["attempt"])
	}
	if maxRetries, _ := meta["max_retries"].(float64); maxRetries != 10 {
		t.Fatalf("max_retries = %v", meta["max_retries"])
	}
	if delay, _ := meta["retry_after_ms"].(float64); delay != 2000 {
		t.Fatalf("retry_after_ms = %v, want 2000 from retryInMs", meta["retry_after_ms"])
	}
	// `formatted` is the CLI's own display string and wins over the raw
	// `message`, which is what the retry banner shows.
	if meta["error"] != "API is temporarily overloaded" {
		t.Fatalf("error = %v, want the formatted display string", meta["error"])
	}
	// A 529 nested under `error.status` must classify transient exactly
	// like the flat `error_status` on the api_retry twin.
	if events[0].Failure == nil || events[0].Failure.Class != provider.FailureTransient {
		t.Fatalf("Failure = %+v, want transient for a nested 529", events[0].Failure)
	}
}

// TestExtractSessionInfo_MCPServerErrors pins the parse half of the
// startup-refusal path. The affected servers are absent from
// `mcp_servers[]` by contract, so this array is the ONLY wire surface
// carrying why they are missing.
func TestExtractSessionInfo_MCPServerErrors(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-8","output_style":"Explanatory","mcp_servers":[{"name":"good","status":"connected"}],"mcp_server_errors":[{"name":"broken","type":"url_missing_type","message":"entry has a url but no type"},{"name":"","type":"invalid_config","message":"nameless"}]}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventInit {
		t.Fatalf("events = %+v, want one init", events)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	// The nameless entry is dropped: the name is the cache key, so an
	// entry without one can only ever be a mystery row.
	if len(info.MCPServerErrors) != 1 {
		t.Fatalf("MCPServerErrors = %+v, want exactly the named entry", info.MCPServerErrors)
	}
	got := info.MCPServerErrors[0]
	if got.Name != "broken" || got.Type != "url_missing_type" || got.Message != "entry has a url but no type" {
		t.Fatalf("MCPServerErrors[0] = %+v", got)
	}
	// The healthy list is untouched, and by upstream contract the two
	// never name the same server.
	if len(info.MCPServers) != 1 || info.MCPServers[0].Name != "good" {
		t.Fatalf("MCPServers = %+v", info.MCPServers)
	}
	// The style echo rides the same envelope; it is what proves a
	// settings-source style AO never sent is nonetheless in effect.
	if info.OutputStyle != "Explanatory" {
		t.Fatalf("OutputStyle = %q", info.OutputStyle)
	}
}

// --- system/permission_denied + system/permission_retry -------------------
//
// Fixtures are the exact object literal the 2.1.237 engine emits
// (`_e({type:"system",subtype:"permission_denied",tool_name:…,
// tool_use_id:…,agent_id:…,decision_reason_type:…,decision_reason:…,
// message:…})`), plus the `uuid`/`session_id` the control-protocol
// producer adds.

func TestParseSystem_PermissionDeniedIsANoticeKeyedToTheToolCall(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_01","agent_id":"agent_7","decision_reason_type":"rule","decision_reason":"Denied by alwaysDenyRules: Bash(rm:*)","message":"Permission to use Bash has been denied.","uuid":"sys-pd-1","session_id":"sess-1"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventNotification {
		t.Fatalf("events = %+v, want one notification", events)
	}
	// Namespaced, NOT the bare tool_use_id: the tool_call row already
	// owns that id and triage upserts notification rows by id.
	if events[0].ItemID != "permission-denied:toolu_01" {
		t.Fatalf("ItemID = %q", events[0].ItemID)
	}
	if events[0].Content != "Bash was denied by a permission rule" {
		t.Fatalf("Content = %q", events[0].Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["kind"] != "permission_denied" {
		t.Fatalf("meta.kind = %v", meta["kind"])
	}
	if meta["toolName"] != "Bash" || meta["toolUseId"] != "toolu_01" || meta["agentId"] != "agent_7" {
		t.Fatalf("correlation meta = %+v", meta)
	}
	if meta["decisionReasonType"] != "rule" {
		t.Fatalf("decisionReasonType = %v", meta["decisionReasonType"])
	}
	// decision_reason wins over message — the deciding component's own
	// words, matching the CLI's own debug renderer's preference order.
	if meta["decisionReason"] != "Denied by alwaysDenyRules: Bash(rm:*)" {
		t.Fatalf("decisionReason = %v", meta["decisionReason"])
	}
	if _, present := meta["workspaceBoundary"]; present {
		t.Fatalf("a rule denial must not claim a workspace boundary: %+v", meta)
	}
}

// A denial that carries no decision_reason falls back to `message`, the
// sentence the CLI hands the model in the tool_result.
func TestParseSystem_PermissionDeniedFallsBackToMessage(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"permission_denied","tool_name":"Write","tool_use_id":"toolu_02","message":"Claude requested permission to write, and it was denied."}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["decisionReason"] != "Claude requested permission to write, and it was denied." {
		t.Fatalf("decisionReason = %v", meta["decisionReason"])
	}
	// Unknown/absent reason type still composes a sentence — the
	// PermissionDecisionReason union is open.
	if events[0].Content != "Write was denied by the permission system" {
		t.Fatalf("Content = %q", events[0].Content)
	}
}

// `workingDir` is the workspace-boundary denial: the remedy is adding
// the directory to the session (the CLI's own suggestions for that
// reason are addDirectories / a Read rule over `<dir>/**`), never a
// Bash(...) rule. The copy and the flag both have to say so.
func TestParseSystem_PermissionDeniedWorkingDirIsAWorkspaceBoundary(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"permission_denied","tool_name":"Read","tool_use_id":"toolu_03","decision_reason_type":"workingDir","decision_reason":"Path is outside allowed working directories","message":"Claude requested permissions to read from /etc/hosts, but you haven't granted it yet."}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if boundary, _ := meta["workspaceBoundary"].(bool); !boundary {
		t.Fatalf("workspaceBoundary = %v, want true", meta["workspaceBoundary"])
	}
	if !strings.Contains(events[0].Content, "outside this workspace's allowed directories") {
		t.Fatalf("Content = %q, want the boundary copy, not a rule sentence", events[0].Content)
	}
}

// No tool_use_id (schema says the field is required, but the notice must
// survive a producer that omits it) → no namespaced id, so triage falls
// back to its per-turn notification counter instead of minting
// "permission-denied:".
func TestParseSystem_PermissionDeniedWithoutToolUseIDHasNoItemID(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"permission_denied","tool_name":"Bash","decision_reason_type":"classifier","decision_reason":"Auto mode classifier denied this command"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].ItemID != "" {
		t.Fatalf("events = %+v, want one notice with no ItemID", events)
	}
	if events[0].Content != "Bash was denied by the permission classifier" {
		t.Fatalf("Content = %q", events[0].Content)
	}
}

func TestClaudeDenialDeciderCoversTheDocumentedUnion(t *testing.T) {
	// The full PermissionDecisionReason union in 2.1.237. Each member
	// must produce a distinct, non-generic phrase except `other`, which
	// is the CLI's own catch-all.
	generic := claudeDenialDecider("other")
	for _, reasonType := range []string{
		"rule", "mode", "subcommandResults", "permissionPromptTool", "hook",
		"asyncAgent", "sandboxOverride", "workingDir", "safetyCheck", "classifier",
	} {
		if reasonType == "workingDir" {
			// workingDir never reaches the decider: the boundary copy
			// replaces the whole sentence.
			continue
		}
		if got := claudeDenialDecider(reasonType); got == generic {
			t.Errorf("claudeDenialDecider(%q) fell through to the generic phrase", reasonType)
		}
	}
	if claudeDenialDecider("some_future_type") != generic {
		t.Error("an unknown reason type must compose the generic phrase, not an empty one")
	}
}

// permission_retry carries NO tool_use_id and NO attempt count — it is
// per command NAME. Producer: `u$m(e)` → `{content:"Allowed …",
// commands:[…], level:"info"}`.
func TestParseSystem_PermissionRetryCarriesAllowedCommands(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"permission_retry","content":"Allowed git status, ls","commands":["git status","ls"],"level":"info","uuid":"sys-pr-1"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventNotification {
		t.Fatalf("events = %+v, want one notification", events)
	}
	if events[0].ItemID != "" {
		t.Fatalf("ItemID = %q, want empty (no tool_use_id on this subtype)", events[0].ItemID)
	}
	if events[0].Content != "Allowed git status, ls" {
		t.Fatalf("Content = %q", events[0].Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["kind"] != "permission_retry" {
		t.Fatalf("meta.kind = %v", meta["kind"])
	}
	commands, _ := meta["commands"].([]any)
	if len(commands) != 2 || commands[0] != "git status" || commands[1] != "ls" {
		t.Fatalf("meta.commands = %+v", meta["commands"])
	}
}

func TestParseSystem_PermissionRetryComposesWhenContentIsEmpty(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"permission_retry","content":"","commands":["npm test"]}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if events[0].Content != "Retrying — allowed npm test" {
		t.Fatalf("Content = %q", events[0].Content)
	}
}

func TestParseSystem_PermissionNoticeFieldsAreBounded(t *testing.T) {
	long := strings.Repeat("z", 4000)
	names := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		names = append(names, long)
	}
	encoded, err := json.Marshal(map[string]any{
		"type": "system", "subtype": "permission_retry",
		"content": long, "commands": names,
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	events, err := ParseLine(testThread, encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len([]rune(events[0].Content)) > maxClaudePermissionReasonRunes+3 {
		t.Fatalf("content not bounded: %d runes", len([]rune(events[0].Content)))
	}
	var meta struct {
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Commands) != maxClaudePermissionCommands {
		t.Fatalf("commands = %d, want capped at %d", len(meta.Commands), maxClaudePermissionCommands)
	}
	for _, name := range meta.Commands {
		if len([]rune(name)) > maxClaudePermissionCommandRunes {
			t.Fatalf("command name not bounded: %d runes", len([]rune(name)))
		}
	}
}

// --- system/init.capabilities ---------------------------------------------

func TestExtractSessionInfo_Capabilities(t *testing.T) {
	raw := map[string]json.RawMessage{
		"capabilities": json.RawMessage(`["interrupt_receipt_v1","interrupt_cancel_queued_v1","msg_lifecycle_v1"]`),
	}
	info := extractSessionInfo(raw)
	want := []string{"interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1"}
	if len(info.Capabilities) != len(want) {
		t.Fatalf("Capabilities = %+v, want %+v", info.Capabilities, want)
	}
	for i, name := range want {
		if info.Capabilities[i] != name {
			t.Fatalf("Capabilities[%d] = %q, want %q (wire order preserved)", i, info.Capabilities[i], name)
		}
	}
}

func TestExtractSessionInfo_CapabilitiesDedupeAndBound(t *testing.T) {
	names := []string{"  msg_lifecycle_v1  ", "msg_lifecycle_v1", "", strings.Repeat("q", 200)}
	for i := 0; i < 80; i++ {
		names = append(names, "cap_"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	info := extractSessionInfo(map[string]json.RawMessage{"capabilities": encoded})
	if len(info.Capabilities) > maxClaudeInitCapabilities {
		t.Fatalf("Capabilities count = %d, want <= %d", len(info.Capabilities), maxClaudeInitCapabilities)
	}
	seen := map[string]bool{}
	for _, name := range info.Capabilities {
		if name == "" {
			t.Fatal("empty capability token survived normalization")
		}
		if len([]rune(name)) > maxClaudeInitCapabilityRunes {
			t.Fatalf("token not bounded: %d runes", len([]rune(name)))
		}
		if seen[name] {
			t.Fatalf("duplicate token %q survived normalization", name)
		}
		seen[name] = true
	}
	if info.Capabilities[0] != "msg_lifecycle_v1" {
		t.Fatalf("Capabilities[0] = %q, want the trimmed first occurrence", info.Capabilities[0])
	}
}

// Absence is "this build said nothing", never "no capabilities". The
// init literal spreads the key conditionally, so an older CLI simply
// omits it — and a malformed value must not take the session id down.
func TestExtractSessionInfo_CapabilitiesAbsentOrMalformedLeavesNil(t *testing.T) {
	if info := extractSessionInfo(map[string]json.RawMessage{
		"session_id": json.RawMessage(`"sess-1"`),
	}); info.Capabilities != nil {
		t.Fatalf("Capabilities = %+v, want nil when the key is absent", info.Capabilities)
	}
	info := extractSessionInfo(map[string]json.RawMessage{
		"session_id":   json.RawMessage(`"sess-1"`),
		"capabilities": json.RawMessage(`{"not":"an array"}`),
	})
	if info.Capabilities != nil {
		t.Fatalf("Capabilities = %+v, want nil for a malformed value", info.Capabilities)
	}
	if info.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q — a cosmetic array must not take the session id down", info.SessionID)
	}
}

func TestParseSystem_InitEventCarriesCapabilities(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-9","model":"claude-fable-5","capabilities":["interrupt_receipt_v1","msg_lifecycle_v1"]}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventInit {
		t.Fatalf("events = %+v, want one init", events)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(info.Capabilities) != 2 || info.Capabilities[0] != "interrupt_receipt_v1" {
		t.Fatalf("Capabilities = %+v", info.Capabilities)
	}
}

// TestParseSystem_ModelFallbackSubtypesShareARequestIDWithoutColliding
// pins the reason the SUBTYPE is part of the row id.
//
// `request_id` names the API request, not the notice, and one request can
// produce more than one member of this family: the CLI moves the session
// on a credits/consent choice and then reports the refusal fallback for
// the same request. Triage upserts on ItemID, so two events sharing one
// id would render as one row and the earlier notice would be silently
// overwritten.
func TestParseSystem_ModelFallbackSubtypesShareARequestIDWithoutColliding(t *testing.T) {
	parser := NewParser()
	parser.SetModel("claude-fable-5")

	consent := []byte(`{"type":"system","subtype":"model_consent_fallback","content":"Out of Fable 5 credits. Continuing on Opus 4.8.","choice":"continue_on_fallback","original_model":"claude-fable-5","fallback_model":"claude-opus-4-8","request_id":"req_shared_1","persisted_as_default":true}`)
	refusal := []byte(`{"type":"system","subtype":"model_refusal_fallback","content":"Fable 5's safeguards flagged this message. Switched to Opus 4.8.","trigger":"refusal","original_model":"claude-fable-5","fallback_model":"claude-opus-4-8","request_id":"req_shared_1","api_refusal_category":"cyber"}`)

	first, err := parser.ParseLine(testThread, consent)
	if err != nil {
		t.Fatalf("parse consent: %v", err)
	}
	second, err := parser.ParseLine(testThread, refusal)
	if err != nil {
		t.Fatalf("parse refusal: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("events = %d + %d, want one each", len(first), len(second))
	}
	if want := "model-fallback:model_consent_fallback:req_shared_1"; first[0].ItemID != want {
		t.Fatalf("consent ItemID = %q, want %q", first[0].ItemID, want)
	}
	if want := "model-fallback:model_refusal_fallback:req_shared_1"; second[0].ItemID != want {
		t.Fatalf("refusal ItemID = %q, want %q", second[0].ItemID, want)
	}
	if first[0].ItemID == second[0].ItemID {
		t.Fatal("both subtypes produced one row id — the second notice would overwrite the first")
	}
}

// The envelope's top-level `uuid` is what identifies a task_notification
// EVENT rather than its task. A persistent Monitor (claude-wire.md §E7)
// fires one per output event of the stream it watches, all sharing one
// task_id, so triage needs the uuid to key one row per event instead of
// overwriting a single row. Verified present on every task_notification
// in docs/references/fixtures/claude/interactive_outlives_taskoutput_monitor.ndjson.
func TestParseTaskNotificationEvent_CarriesEnvelopeUUIDOntoMeta(t *testing.T) {
	parser := NewParser()

	line := []byte(`{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu1","status":"completed","summary":"done","uuid":"b733fbc6-41f5-4b1c-927d-d9018dad4909"}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta["uuid"] != "b733fbc6-41f5-4b1c-927d-d9018dad4909" {
		t.Fatalf("meta uuid = %v, want the envelope uuid (meta=%s)", meta["uuid"], events[0].Meta)
	}

	// An older CLI omits it; the key must then be ABSENT rather than
	// empty, so triage's fallback reads as "no uuid on the wire".
	events, err = parser.ParseLine(testThread, []byte(`{"type":"system","subtype":"task_notification","task_id":"t2","status":"completed","summary":"done"}`))
	if err != nil {
		t.Fatalf("parse without uuid: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event without uuid, got %d", len(events))
	}
	meta = nil
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if _, ok := meta["uuid"]; ok {
		t.Fatalf("absent envelope uuid must not emit the key, got %s", events[0].Meta)
	}
}

func TestParseSystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"abc-123","model":"claude-opus-4-6","cwd":"/home/user","tools":["Bash","Edit"],"claude_code_version":"2.0.0"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventInit)
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}

	var info provider.SessionInfo
	if err := json.Unmarshal(evt.Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if info.SessionID != "abc-123" {
		t.Errorf("sessionID: got %q, want %q", info.SessionID, "abc-123")
	}
	if info.Model != "claude-opus-4-6" {
		t.Errorf("model: got %q, want %q", info.Model, "claude-opus-4-6")
	}
	if info.CWD != "/home/user" {
		t.Errorf("cwd: got %q, want %q", info.CWD, "/home/user")
	}
	if len(info.Tools) != 2 {
		t.Errorf("tools: got %d, want 2", len(info.Tools))
	}
	if info.Version != "2.0.0" {
		t.Errorf("version: got %q, want %q", info.Version, "2.0.0")
	}
}

func TestParseSystemSkippedSubtypes(t *testing.T) {
	skipped := []string{
		"hook_started", "hook_progress", "hook_response",
		"notification", "files_persisted",
		"tool_use_summary", "memory_recall", "local_command_output",
	}

	for _, subtype := range skipped {
		line := []byte(`{"type":"system","subtype":"` + subtype + `"}`)
		events, err := ParseLine(testThread, line)
		if err != nil {
			t.Errorf("subtype %q: unexpected error: %v", subtype, err)
		}
		if len(events) != 0 {
			t.Errorf("subtype %q: expected 0 events, got %d", subtype, len(events))
		}
	}
}

func TestParseToolProgressDropped(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress","item_id":"item-1","content":{"progress":{"current":5,"total":10,"message":"Reading..."}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected tool_progress to be dropped, got %d event(s)", len(events))
	}
}

func TestParseToolProgressNoContentDropped(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected tool_progress to be dropped, got %d event(s)", len(events))
	}
}

func TestParseCompactBoundary(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"compact-1","content":"Conversation compacted","data":{"context_window":{"used_tokens":50000,"max_tokens":200000,"used_percentage":25,"total_processed":120000}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventCompactBoundary)
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}
	if evt.ItemID != "compact-1" {
		t.Errorf("itemID: got %q, want compact-1", evt.ItemID)
	}
	if evt.Content != "Conversation compacted" {
		t.Errorf("content: got %q, want Conversation compacted", evt.Content)
	}

	var meta provider.ContextWindow
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.UsedTokens != 50000 {
		t.Errorf("UsedTokens: got %d, want 50000", meta.UsedTokens)
	}
	if meta.MaxTokens != 200000 {
		t.Errorf("MaxTokens: got %d, want 200000", meta.MaxTokens)
	}
	if meta.UsedPercentage != 25 {
		t.Errorf("UsedPercentage: got %f, want 25", meta.UsedPercentage)
	}
}

func TestParseCompactBoundaryPreservesCompactMetadata(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"compact-2","content":"Conversation compacted","compactMetadata":{"trigger":"auto","durationMs":111814}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.ItemID != "compact-2" {
		t.Errorf("itemID: got %q, want compact-2", evt.ItemID)
	}
	if evt.Content != "Conversation compacted" {
		t.Errorf("content: got %q, want Conversation compacted", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["trigger"] != "auto" {
		t.Fatalf("trigger = %v, want auto", meta["trigger"])
	}
	if meta["durationMs"] != float64(111814) {
		t.Fatalf("durationMs = %v, want 111814", meta["durationMs"])
	}
}

func TestParseCompactBoundaryPreservesCompactMetadataWhenDataIsNotContextWindow(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"compact-3","content":"Conversation compacted","data":{"note":"not a context window"},"compactMetadata":{"trigger":"auto","durationMs":222}}`)

	events, err := ParseLine(testThread, line)
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
	if meta["trigger"] != "auto" {
		t.Fatalf("trigger = %v, want auto", meta["trigger"])
	}
	if meta["durationMs"] != float64(222) {
		t.Fatalf("durationMs = %v, want 222", meta["durationMs"])
	}
}

func TestParseCompactBoundaryNoData(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventCompactBoundary)
	}
	if evt.Meta != nil {
		t.Errorf("meta: got %s, want nil", evt.Meta)
	}
}

func TestParseApiRetry(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","data":{"attempt":2,"max_retries":10,"retry_after_ms":5000,"error":{"message":"server overloaded"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventAPIRetry {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventAPIRetry)
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["attempt"] != float64(2) {
		t.Errorf("attempt: got %v, want 2", meta["attempt"])
	}
	if meta["max_retries"] != float64(10) {
		t.Errorf("max_retries: got %v, want 10", meta["max_retries"])
	}
	if meta["retry_after_ms"] != float64(5000) {
		t.Errorf("retry_after_ms: got %v, want 5000", meta["retry_after_ms"])
	}
	if meta["error"] != "server overloaded" {
		t.Errorf("error: got %v, want \"server overloaded\"", meta["error"])
	}
}

func TestParseUnknownSystemSubtype(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"future_feature"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}
