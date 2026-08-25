package claude

import (
	"encoding/json"
	"reflect"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseRateLimitEvent_FiveHourUtilization pins the wire round-trip
// for `rate_limit_event` envelopes carrying a `five_hour` window with
// utilization. The previous parser hardcoded UsedPercent based on
// `status` ("allowed" → 0, otherwise → 100), discarding the wire's
// `utilization` field — that meant the UI ring would always read 0%
// while healthy and pin to 100% the moment Claude warned. This test
// guards against regressing to that shape.
func TestParseRateLimitEvent_FiveHourUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776981600,"rateLimitType":"five_hour","utilization":0.42,"isUsingOverage":false}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventRateLimits {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventRateLimits)
	}

	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.Provider != string(provider.Claude) {
		t.Errorf("Provider: got %q, want %q", snap.Provider, provider.Claude)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("Limits len: got %d, want 1", len(snap.Limits))
	}
	got := snap.Limits[0]
	if got.LimitID != "session" {
		t.Errorf("LimitID: got %q, want %q", got.LimitID, "session")
	}
	if got.LimitName != "Current session" {
		t.Errorf("LimitName: got %q, want %q", got.LimitName, "Current session")
	}
	if got.UsedPercent != 42 {
		t.Errorf("UsedPercent: got %v, want 42 (utilization 0.42 × 100)", got.UsedPercent)
	}
	if got.WindowMins != 300 {
		t.Errorf("WindowMins: got %d, want 300 (five_hour)", got.WindowMins)
	}
	if got.ResetsAt != 1776981600 {
		t.Errorf("ResetsAt: got %d, want 1776981600", got.ResetsAt)
	}
}

// TestParseRateLimitEvent_SevenDayUtilization mirrors the five-hour
// case for the seven-day window. seven_day → 10080 minutes.
func TestParseRateLimitEvent_SevenDayUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776981600,"rateLimitType":"seven_day","utilization":0.51,"isUsingOverage":false}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(snap.Limits) != 1 {
		t.Fatalf("Limits len: got %d, want 1", len(snap.Limits))
	}
	got := snap.Limits[0]
	if got.LimitID != "weekly_all" {
		t.Errorf("LimitID: got %q, want %q", got.LimitID, "weekly_all")
	}
	if got.LimitName != "All models" {
		t.Errorf("LimitName: got %q, want %q", got.LimitName, "All models")
	}
	if got.UsedPercent != 51 {
		t.Errorf("UsedPercent: got %v, want 51", got.UsedPercent)
	}
	if got.WindowMins != 10080 {
		t.Errorf("WindowMins: got %d, want 10080 (seven_day)", got.WindowMins)
	}
}

// TestParseRateLimitEvent_MissingUtilization covers the wire shape
// observed during normal usage (e.g. fixtures
// local_agent_outlives.ndjson) where Claude emits status:"allowed"
// envelopes without a `utilization` field. Claude only populates
// utilization once we cross the warning threshold, so most sessions
// never see one over the stream wire. The steady-state percentages come from
// the OAuth usage endpoint, with unified response headers as a compatibility
// fallback (`ratelimits_probe.go`); the parser must drop these wire events so
// a 0% fallback doesn't race the probe's real reading and visibly clobber it
// on first turn.
func TestParseRateLimitEvent_MissingUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1777920000,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events (utilization absent → drop, probe owns steady-state), got %d", len(events))
	}
}

// TestParseRateLimitEvent_FullUtilization pins the upper bound: 1.0
// utilization renders as 100. Without the parser fix, anything except
// `status:"allowed"` already hit 100 by accident — make sure the
// utilization path produces it correctly.
func TestParseRateLimitEvent_FullUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"overloaded","resetsAt":1776981600,"rateLimitType":"five_hour","utilization":1.0}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Limits[0].UsedPercent != 100 {
		t.Errorf("UsedPercent: got %v, want 100", snap.Limits[0].UsedPercent)
	}
}

// TestParseRateLimitEvent_ExplicitZeroUtilization confirms that an
// explicit `utilization: 0.0` on the wire emits with UsedPercent=0 —
// distinct from the absent-utilization case which drops entirely. The
// *float64 parsing distinguishes "present but zero" from "absent"; an
// explicit 0.0 is a real wire reading, so emit it; an absent field
// means "no usable percentage" and gets dropped so the probe's
// HTTP-header reading isn't clobbered.
func TestParseRateLimitEvent_ExplicitZeroUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1776981600,"rateLimitType":"five_hour","utilization":0.0}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (explicit 0.0 is a real reading), got %d", len(events))
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Limits[0].UsedPercent != 0 {
		t.Errorf("UsedPercent: got %v, want 0", snap.Limits[0].UsedPercent)
	}
	if snap.Limits[0].WindowMins != 300 {
		t.Errorf("WindowMins: got %d, want 300", snap.Limits[0].WindowMins)
	}
}

// TestParseRateLimitEvent_RejectedCarriesNoUtilization pins the one exception to
// the drop-without-utilization rule. `status:"rejected"` is the envelope the CLI
// emits when the API refused the request with a 429: its limits are built from
// the response headers, which carry `anthropic-ratelimit-unified-reset` but no
// utilization at all. Dropping it would erase the structured reset boundary
// from account-usage surfaces. A refused window is spent by definition, so it
// records as 100%; workflow control flow uses the separately normalized typed
// refusal and treats this boundary as advisory.
func TestParseRateLimitEvent_RejectedCarriesNoUtilization(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1776981600,"rateLimitType":"five_hour"}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (a refusal is not a stale reading), got %d", len(events))
	}
	var snap provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Limits[0].UsedPercent != 100 || snap.Limits[0].ResetsAt != 1776981600 ||
		snap.Limits[0].WindowMins != 300 {
		t.Fatalf("limit = %+v, want a spent five-hour window with its reset boundary", snap.Limits[0])
	}
}

// A rejection that names no boundary says only "later", which is what every
// consumer already assumes — and admitting it at 100% would clobber the probe's
// real percentage with a reading nothing can act on.
func TestParseRateLimitEvent_RejectedWithoutABoundaryIsDropped(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events (no reset boundary → nothing to act on), got %d", len(events))
	}
}

// TestParseRateLimitEvent_UnknownRateLimitType — when the wire emits a
// rateLimitType we don't recognise, the snapshot is dropped. The
// frontend rings key off WindowMins (300/10080) so a snapshot with
// WindowMins=0 would never render anyway, and emitting one risks
// polluting the process-global store with a row no UI can use.
func TestParseRateLimitEvent_UnknownRateLimitType(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1776981600,"rateLimitType":"thirty_day","utilization":0.1}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events (unknown rateLimitType → drop), got %d", len(events))
	}
}

// TestParseRateLimitEvent_MissingInfo — the envelope without a
// `rate_limit_info` payload is dropped silently. This matches the
// existing parser contract (return nil events, no error).
func TestParseRateLimitEvent_MissingInfo(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event"}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestParseControlRequestCanUseTool(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
	if evt.ItemID != "req_1_abc" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "req_1_abc")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.RequestID != "req_1_abc" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "req_1_abc")
	}
	if approval.ToolName != "Bash" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "Bash")
	}
	if string(approval.Input) != `{"command":"rm -rf /"}` {
		t.Errorf("input: got %s, want %s", approval.Input, `{"command":"rm -rf /"}`)
	}
}

func TestParseControlRequestAskUserQuestionNormalizesQuestionIDs(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"header":"Framework","question":"Pick one","options":[{"label":"React","description":""}]},{"question":"Pick mode","options":[{"label":"Plan","description":""}]},{"id":"__proto__","header":"constructor","question":"Reserved","options":[{"label":"Safe","description":""}]}]}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventUserInputRequest {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventUserInputRequest)
	}

	var request provider.UserInputRequest
	if err := json.Unmarshal(events[0].Meta, &request); err != nil {
		t.Fatalf("unmarshal user input: %v", err)
	}
	if len(request.Questions) != 3 {
		t.Fatalf("questions len = %d, want 3", len(request.Questions))
	}
	got := []string{request.Questions[0].ID, request.Questions[1].ID, request.Questions[2].ID}
	want := []string{"Framework", "Pick mode", "Reserved"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("question IDs = %#v, want %#v", got, want)
	}
}

func TestParseControlRequestMalformedAskUserQuestionFallsBackToApproval(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-bad-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[]}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
}

func TestParseControlRequestUnknownSubtype(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req_1","request":{"subtype":"unknown_subtype"}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

// TestParseLine_ControlResponseSuccess confirms that a control_response
// line (the CLI's reply to our outbound stop_task control_request) is
// routed silently through the top-level dispatch: no ProviderEvent is
// emitted, the line isn't misclassified as a control_request, and no
// parse error surfaces. The session-level readLoop handles routing to
// pending StopTask callers via handleControlResponseLine; ParseLine's
// job is just to not mangle the envelope.
func TestParseLine_ControlResponseSuccess(t *testing.T) {
	successLine := []byte(`{"type":"control_response","response":{"subtype":"success","request_id":"so-1","response":{}}}`)
	events, err := ParseLine(testThread, successLine)
	if err != nil {
		t.Fatalf("parse success: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("control_response must emit 0 events, got %d: %+v", len(events), events)
	}

	// Error form must parse cleanly too — the parser never mistakes it
	// for an inbound control_request (which would misroute it into
	// parseControlRequest as a malformed approval).
	errorLine := []byte(`{"type":"control_response","response":{"subtype":"error","request_id":"so-2","error":"boom"}}`)
	events, err = ParseLine(testThread, errorLine)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("control_response(error) must emit 0 events, got %d: %+v", len(events), events)
	}
}
