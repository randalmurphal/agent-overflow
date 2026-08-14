package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// TestAdvisorEndToEndProducesToolCallRowAndPayload exercises the
// parser → triage → store seam for Claude's server-side `advisor`
// tool. Two NDJSON envelopes feed through the real Claude parser
// (so the `server_tool_use` / `advisor_tool_result` content-block
// arms in parse_assistant.go are part of the path under test) and
// the resulting events are routed through the live Router into an
// in-memory store. We then assert:
//
//   - One `tool_call` row keyed by the `srvtoolu_*` id with
//     ToolName="advisor", running after the start, completed after
//     the result.
//   - Item meta preserves `advisor_model` and `assistant_message_id`
//     across the persistence boundary so AdvisorRow can read them.
//   - A `tool-call-result:srvtoolu_...` payload row carries the
//     advisor text as data, with a `preview` field on the payload
//     header (what the collapsed row pulls via item.payloadMeta).
//
// This is the integration assertion called out in the
// "yeah-its-not-like-delightful-pike" advisor plan; the parser-only
// unit tests in internal/provider/claude/parse_assistant_test.go
// already pin the wire→event boundary.
func TestAdvisorEndToEndProducesToolCallRowAndPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t-advisor")

	parser := claude.NewParser()

	if _, err := parser.ParseLine("t-advisor", []byte(
		`{"type":"system","subtype":"init","session_id":"sess-adv","model":"claude-opus-4-7","tools":["advisor"],"cwd":"/tmp"}`,
	)); err != nil {
		t.Fatalf("init parse: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t-advisor",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	const advisorText = "Reviewer suggests two small refactors before merging.\n" +
		"1. Extract the validation helper.\n" +
		"2. Add a regression test for the empty-input branch."

	callLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_e2e","name":"advisor","input":{}}]}}`)
	startEvents, err := parser.ParseLine("t-advisor", callLine)
	if err != nil {
		t.Fatalf("call parse: %v", err)
	}
	if len(startEvents) == 0 {
		t.Fatalf("expected at least one EventToolStart from server_tool_use")
	}
	var sawStart bool
	for _, evt := range startEvents {
		if evt.Kind == provider.EventToolStart && evt.ItemType == "advisor" {
			sawStart = true
		}
		if err := router.Handle(evt); err != nil {
			t.Fatalf("route start event %s: %v", evt.Kind, err)
		}
	}
	if !sawStart {
		t.Fatalf("parser did not emit an advisor EventToolStart; events=%+v", startEvents)
	}

	running, ok, err := st.GetThreadItem("t-advisor", "srvtoolu_e2e")
	if err != nil || !ok {
		t.Fatalf("post-start lookup: ok=%v err=%v", ok, err)
	}
	if running.Kind != "tool_call" {
		t.Fatalf("post-start kind: got %q, want tool_call", running.Kind)
	}
	if running.ToolName != "advisor" {
		t.Fatalf("post-start toolName: got %q, want advisor", running.ToolName)
	}
	if running.Status != statusRunning {
		t.Fatalf("post-start status: got %q, want %q", running.Status, statusRunning)
	}

	// The unknown `advisor_model` / `assistant_message_id` fields are
	// preserved verbatim through validJSONObjectString — the typed
	// ToolStartMeta struct doesn't enumerate them, but the raw
	// round-trip keeps every top-level key. AdvisorRow reads these
	// directly off item.meta to render the "Advisor (Opus 4.7)" affix.
	var runningMeta map[string]any
	if err := json.Unmarshal([]byte(running.Meta), &runningMeta); err != nil {
		t.Fatalf("running meta unmarshal: %v (meta=%q)", err, running.Meta)
	}
	if runningMeta["advisor_model"] != "claude-opus-4-7" {
		t.Fatalf("running meta.advisor_model: got %v, want claude-opus-4-7", runningMeta["advisor_model"])
	}
	if runningMeta["assistant_message_id"] != "msg-adv" {
		t.Fatalf("running meta.assistant_message_id: got %v, want msg-adv", runningMeta["assistant_message_id"])
	}

	resultLine := []byte(`{"type":"assistant","message":{"id":"msg-adv","role":"assistant","model":"claude-opus-4-7","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_e2e","content":{"type":"advisor_result","text":` +
		mustEncodeJSONString(advisorText) + `}}]}}`)
	completeEvents, err := parser.ParseLine("t-advisor", resultLine)
	if err != nil {
		t.Fatalf("result parse: %v", err)
	}
	if len(completeEvents) != 1 || completeEvents[0].Kind != provider.EventToolComplete {
		t.Fatalf("expected one EventToolComplete, got %+v", completeEvents)
	}
	if completeEvents[0].ItemID != "srvtoolu_e2e" {
		t.Fatalf("EventToolComplete ItemID: got %q, want srvtoolu_e2e", completeEvents[0].ItemID)
	}
	if completeEvents[0].Content != advisorText {
		t.Fatalf("EventToolComplete Content mismatch:\n got %q\nwant %q", completeEvents[0].Content, advisorText)
	}
	if err := router.Handle(completeEvents[0]); err != nil {
		t.Fatalf("route complete: %v", err)
	}

	completed, ok, err := st.GetThreadItem("t-advisor", "srvtoolu_e2e")
	if err != nil || !ok {
		t.Fatalf("post-complete lookup: ok=%v err=%v", ok, err)
	}
	if completed.Status != statusCompleted {
		t.Fatalf("post-complete status: got %q, want %q", completed.Status, statusCompleted)
	}
	if completed.ToolName != "advisor" {
		t.Fatalf("post-complete toolName: got %q, want advisor", completed.ToolName)
	}
	if completed.PayloadID != "tool-call-result:srvtoolu_e2e" {
		t.Fatalf("post-complete payloadID: got %q, want tool-call-result:srvtoolu_e2e", completed.PayloadID)
	}

	// Completion must NOT scrub the `advisor_model` field we stamped on
	// the launch — mergeItemMetaJSON unions the launch keys with the
	// completion keys, so the model affix survives.
	var completedMeta map[string]any
	if err := json.Unmarshal([]byte(completed.Meta), &completedMeta); err != nil {
		t.Fatalf("completed meta unmarshal: %v (meta=%q)", err, completed.Meta)
	}
	if completedMeta["advisor_model"] != "claude-opus-4-7" {
		t.Fatalf("completed meta.advisor_model: got %v, want claude-opus-4-7", completedMeta["advisor_model"])
	}
	if completedMeta["assistant_message_id"] != "msg-adv" {
		t.Fatalf("completed meta.assistant_message_id: got %v, want msg-adv (mergeItemMetaJSON must preserve every launch-side unknown key, not just advisor_model)", completedMeta["assistant_message_id"])
	}

	data, err := st.GetPayloadData(completed.ThreadID, completed.PayloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != advisorText {
		t.Fatalf("payload data: got %q, want %q", string(data), advisorText)
	}

	// The payload header carries the truncated preview AdvisorRow's
	// collapsed-row rendering reads. completionPayload writes at most
	// 240 chars; the advisor body fits inside that cap so we expect
	// the full text in the preview header.
	pm, err := st.GetPayloadMeta(completed.ThreadID, completed.PayloadID)
	if err != nil {
		t.Fatalf("get payload meta: %v", err)
	}
	if pm.Kind != "tool_call_result" {
		t.Fatalf("payload kind: got %q, want tool_call_result", pm.Kind)
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(pm.Meta), &header); err != nil {
		t.Fatalf("payload meta unmarshal: %v (meta=%q)", err, pm.Meta)
	}
	preview, _ := header["preview"].(string)
	if preview == "" {
		t.Fatalf("payload header missing preview field: %q", pm.Meta)
	}
	if !strings.Contains(preview, "Reviewer suggests two small refactors") {
		t.Fatalf("preview does not contain advisor text head: %q", preview)
	}
}

// mustEncodeJSONString writes a Go string as a JSON-quoted literal so
// the inline NDJSON template above stays readable while still escaping
// newlines/quotes through encoding/json. Tests-only.
func mustEncodeJSONString(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(out)
}
