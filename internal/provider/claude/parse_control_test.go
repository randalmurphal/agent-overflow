package claude

import (
	"encoding/json"
	"reflect"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseRateLimitEventProducesNoQuota pins that no rate_limit_event shape
// reaches the usage meters. The event cannot say which account's quota it
// reports: a response to a request sent before an account switch lands after
// it, and attributing it to the session's current account files one account's
// usage under another. The meters read /api/oauth/usage.
func TestParseRateLimitEventProducesNoQuota(t *testing.T) {
	for name, line := range map[string]string{
		"warning seven_day": `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1792537200,"rateLimitType":"seven_day","utilization":0.28,"isUsingOverage":false}}`,
		"warning five_hour": `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776981600,"rateLimitType":"five_hour","utilization":0.92}}`,
		"rejected":          `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1776981600,"rateLimitType":"seven_day"}}`,
		"unified windows":   `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1788316200,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.12,"resetsAt":1788316200},"seven_day":{"utilization":0.02,"resetsAt":1788782400}}}}`,
		"missing info":      `{"type":"rate_limit_event"}`,
	} {
		t.Run(name, func(t *testing.T) {
			events, err := NewParser().ParseLine(testThread, []byte(line))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("rate_limit_event produced %d events, want none: %+v", len(events), events)
			}
		})
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
