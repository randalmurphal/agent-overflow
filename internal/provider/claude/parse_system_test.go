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
	if evt.ItemID != "model-fallback:req_fallback_1" {
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
	itemID := claudeFallbackItemID(unsafeID)
	if !strings.HasPrefix(itemID, "model-fallback:sha256:") || len(itemID) != len("model-fallback:sha256:")+32 {
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
