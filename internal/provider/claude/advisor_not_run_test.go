package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// Advisor calls that share an API message with a client tool_use are never
// run by the API (claude-wire.md §Orphaned server-side tool calls). These
// tests pin each block order observed in session transcripts.

func clientToolEnvelope(parent, msgID, toolUseID string) string {
	return `{"type":"assistant","parent_tool_use_id":` + jsonString(parent) +
		`,"message":{"id":"` + msgID + `","role":"assistant","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Bash","input":{"command":"true"}}],"usage":{"input_tokens":3}}}`
}

// advisorEvent is one advisor-related event in emission order.
type advisorEvent struct {
	kind   provider.EventKind
	itemID string
	meta   map[string]any
}

// advisorEvents feeds lines through one parser and returns the tool events
// for the given ids, in order.
func advisorEvents(t *testing.T, ids []string, lines ...string) []advisorEvent {
	t.Helper()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	parser := NewParser()
	var out []advisorEvent
	for _, line := range lines {
		events, err := parser.ParseLine(testThreadProto, []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		for _, evt := range events {
			if !want[evt.ItemID] || (evt.Kind != provider.EventToolStart && evt.Kind != provider.EventToolComplete) {
				continue
			}
			var meta map[string]any
			if len(evt.Meta) > 0 {
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("meta unmarshal: %v", err)
				}
			}
			out = append(out, advisorEvent{kind: evt.Kind, itemID: evt.ItemID, meta: meta})
		}
	}
	return out
}

func isNotRun(e advisorEvent) bool {
	return e.kind == provider.EventToolComplete && e.meta["item_status"] == "declined" && e.meta["is_error"] == false
}

// The common shape (session a4ee3c2c): the model calls Bash, then the
// advisor, in one message. The API stops for Bash and never runs the
// advisor; the next message calls it again and that call runs. Only the
// second call gets a row, and the late model stamp lands on it.
func TestAdvisorAfterClientToolUseGetsNoRow(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_dead", "srvtoolu_real", "toolu_bash"},
		advisorMessageStart("", "msg-1"),
		clientToolEnvelope("", "msg-1", "toolu_bash"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_dead", noIterations),
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":6,"output_tokens":30}}}`,
		advisorMessageStart("", "msg-2"),
		advisorCallEnvelope("", "msg-2", "srvtoolu_real", noIterations),
		advisorResultEnvelope("", "msg-2", "srvtoolu_real"),
		clientToolEnvelope("", "msg-2", "toolu_bash2"),
		advisorMessageDelta("", "claude-fable-5-1"),
	)
	for _, e := range got {
		if e.itemID == "srvtoolu_dead" {
			t.Fatalf("an advisor call after a client tool_use must emit nothing, got %+v", e)
		}
	}
	var start, complete, stamp int
	for _, e := range got {
		if e.itemID != "srvtoolu_real" {
			continue
		}
		switch {
		case e.kind == provider.EventToolStart && e.meta["advisor_model"] == "claude-fable-5-1":
			stamp++
		case e.kind == provider.EventToolStart:
			start++
		case e.kind == provider.EventToolComplete && e.meta["is_error"] == false && e.meta["item_status"] == nil:
			complete++
		}
	}
	if start != 1 || complete != 1 || stamp != 1 {
		t.Fatalf("real call: got start=%d complete=%d stamp=%d, want 1 each; events %+v", start, complete, stamp, got)
	}
}

// The reverse order: the advisor block arrives first, then a client
// tool_use of the same message. The call's row exists, and it settles as
// declined the moment the client tool_use arrives, before the tool's own
// start. A model on the closing message_delta never lands on it.
func TestAdvisorBeforeClientToolUseSettlesNotRun(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_dead", "toolu_bash"},
		advisorMessageStart("", "msg-1"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_dead", noIterations),
		clientToolEnvelope("", "msg-1", "toolu_bash"),
		advisorMessageDelta("", "claude-fable-5-1"),
		// A result for a settled call cannot reopen it.
		advisorResultEnvelope("", "msg-1", "srvtoolu_dead"),
	)
	if len(got) != 3 {
		t.Fatalf("got %d events, want start, not-run, tool start: %+v", len(got), got)
	}
	if got[0].kind != provider.EventToolStart || got[0].itemID != "srvtoolu_dead" {
		t.Fatalf("first event: got %+v, want the advisor start", got[0])
	}
	if !isNotRun(got[1]) || got[1].itemID != "srvtoolu_dead" {
		t.Fatalf("second event: got %+v, want the advisor settled as declined", got[1])
	}
	if got[2].kind != provider.EventToolStart || got[2].itemID != "toolu_bash" {
		t.Fatalf("third event: got %+v, want the Bash start", got[2])
	}
}

// A message with two client tool calls around the advisor (observed
// shape: tool_use, server_tool_use, tool_use) gets no advisor row.
func TestAdvisorBetweenClientToolUsesGetsNoRow(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_dead"},
		advisorMessageStart("", "msg-1"),
		clientToolEnvelope("", "msg-1", "toolu_a"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_dead", noIterations),
		clientToolEnvelope("", "msg-1", "toolu_b"),
	)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no advisor events", got)
	}
}

// The normal case: advisor, result, then client tool calls in the same
// message. The call completes normally and is never settled as not run.
func TestAdvisorResultBeforeClientToolUseRunsNormally(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_a"},
		advisorMessageStart("", "msg-1"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_a", noIterations),
		advisorResultEnvelope("", "msg-1", "srvtoolu_a"),
		clientToolEnvelope("", "msg-1", "toolu_a"),
		clientToolEnvelope("", "msg-1", "toolu_b"),
		advisorMessageDelta("", "claude-fable-5-1"),
	)
	if len(got) != 3 {
		t.Fatalf("got %+v, want start, complete, model stamp", got)
	}
	for _, e := range got {
		if isNotRun(e) {
			t.Fatalf("a call with its result must never settle as not run: %+v", got)
		}
	}
	if got[2].meta["advisor_model"] != "claude-fable-5-1" {
		t.Fatalf("model stamp: got %+v", got[2])
	}
}

// claudetui assembles a whole message into one envelope. An advisor call
// with a client tool_use after it and no result in between gets no row in
// either order; a call with its result runs normally.
func TestAdvisorAssembledEnvelope(t *testing.T) {
	envelope := func(blocks string) string {
		return `{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"claude-opus-5-5","content":[` + blocks + `],"usage":` + advisorUsage() + `}}`
	}
	advisor := `{"type":"server_tool_use","id":"srvtoolu_a","name":"advisor","input":{}}`
	result := `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_a","content":{"type":"advisor_redacted_result","encrypted_content":"opaque"}}`
	tool := `{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"true"}}`

	for name, blocks := range map[string]string{
		"advisor then tool": advisor + "," + tool,
		"tool then advisor": tool + "," + advisor,
	} {
		t.Run(name, func(t *testing.T) {
			if got := advisorEvents(t, []string{"srvtoolu_a"}, envelope(blocks)); len(got) != 0 {
				t.Fatalf("got %+v, want no advisor events", got)
			}
		})
	}
	t.Run("advisor, result, tool", func(t *testing.T) {
		got := advisorEvents(t, []string{"srvtoolu_a"}, envelope(advisor+","+result+","+tool))
		if len(got) != 2 || got[0].kind != provider.EventToolStart || got[1].kind != provider.EventToolComplete || isNotRun(got[1]) {
			t.Fatalf("got %+v, want start then a normal complete", got)
		}
	})
}

// The rule is per scope: a client tool_use in the main thread does not
// settle a subagent's pending advisor call, and a subagent's own client
// tool_use applies only to its own messages.
func TestAdvisorNotRunIsScopedPerAgent(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_sub", "srvtoolu_sub_dead"},
		advisorCallEnvelope("toolu_agent", "msg-sub-1", "srvtoolu_sub", noIterations),
		clientToolEnvelope("", "msg-main", "toolu_main"),
		advisorResultEnvelope("toolu_agent", "msg-sub-1", "srvtoolu_sub"),
		clientToolEnvelope("toolu_agent", "msg-sub-2", "toolu_sub_bash"),
		advisorCallEnvelope("toolu_agent", "msg-sub-2", "srvtoolu_sub_dead", noIterations),
	)
	if len(got) != 2 || got[0].itemID != "srvtoolu_sub" || got[1].itemID != "srvtoolu_sub" || isNotRun(got[1]) {
		t.Fatalf("got %+v, want the subagent call to start and complete normally and the dead one to emit nothing", got)
	}
}

// Across envelopes, a message without an id is never judged: its advisor
// call keeps its row.
func TestAdvisorWithoutMessageIDIsNotJudged(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_a"},
		clientToolEnvelope("", "", "toolu_a"),
		advisorCallEnvelope("", "", "srvtoolu_a", noIterations),
		clientToolEnvelope("", "", "toolu_b"),
	)
	if len(got) != 1 || got[0].kind != provider.EventToolStart {
		t.Fatalf("got %+v, want only the start", got)
	}
}

// A failed advisor call settles as an error carrying its code.
func TestAdvisorErrorResultCarriesCode(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	got := advisorEvents(t, []string{"srvtoolu_a"},
		advisorCallEnvelope("", "msg-1", "srvtoolu_a", noIterations),
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"advisor_tool_result","tool_use_id":"srvtoolu_a","content":{"type":"advisor_tool_result_error","error_code":"overloaded"}}]}}`,
	)
	if len(got) != 2 || got[1].kind != provider.EventToolComplete {
		t.Fatalf("got %+v, want start then complete", got)
	}
	if got[1].meta["is_error"] != true || got[1].meta["advisor_error_code"] != "overloaded" {
		t.Fatalf("complete meta: got %+v, want is_error and advisor_error_code overloaded", got[1].meta)
	}
}
