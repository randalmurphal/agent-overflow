package claudetui

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

const testThread = "thread-tui-1"

// driveAgentRequest runs the reconstruction for one classAgent /v1/messages
// response through the REAL claude.Parser and returns the ProviderEvents. This
// is the parity harness: the TUI path must produce the same event shapes the
// headless path does, because it shares the same parser.
func driveAgentRequest(t *testing.T, reqBody string, sse []string) []provider.ProviderEvent {
	t.Helper()
	parser := claude.NewParser()
	var out []provider.ProviderEvent
	rec := newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		out = append(out, evs...)
	})

	class, req := classifyRequest([]byte(reqBody))
	if class != classAgent {
		t.Fatalf("classifyRequest = %v, want classAgent", class)
	}
	ar := rec.beginAgentRequest(req)
	for _, s := range sse {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()
	return out
}

func kindsOf(events []provider.ProviderEvent) []provider.EventKind {
	ks := make([]provider.EventKind, len(events))
	for i, e := range events {
		ks[i] = e.Kind
	}
	return ks
}

func findKind(events []provider.ProviderEvent, kind provider.EventKind) []provider.ProviderEvent {
	var out []provider.ProviderEvent
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

const agentReqBody = `{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"},{"name":"Read"}]}`

// TestReconstructTextAndToolTurn drives a turn with streamed text plus a Bash
// tool_use whose JSON input is streamed in fragments and stops at tool_use
// (model not done). Asserts: live text deltas, content-block boundaries, a tool
// start carrying the assembled input, and NO turn-complete (still mid-turn).
func TestReconstructTextAndToolTurn(t *testing.T) {
	sse := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-haiku","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls -la\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":15}}`,
		`{"type":"message_stop"}`,
	}
	events := driveAgentRequest(t, agentReqBody, sse)

	// Live streamed text.
	var text strings.Builder
	for _, e := range findKind(events, provider.EventTextDelta) {
		text.WriteString(e.Content)
	}
	if text.String() != "Hello world" {
		t.Fatalf("assembled text deltas = %q, want %q", text.String(), "Hello world")
	}

	// Content-block boundaries streamed for the text block.
	if len(findKind(events, provider.EventContentBlockStart)) == 0 {
		t.Error("expected at least one content_block_start event")
	}
	if len(findKind(events, provider.EventContentBlockStop)) == 0 {
		t.Error("expected at least one content_block_stop event")
	}

	// Tool start from the assembled assistant envelope, carrying the input
	// that was streamed in fragments and reassembled.
	starts := findKind(events, provider.EventToolStart)
	var bash *provider.ProviderEvent
	for i := range starts {
		if starts[i].ItemID == "toolu_1" {
			bash = &starts[i]
		}
	}
	if bash == nil {
		t.Fatalf("expected EventToolStart for toolu_1, got kinds %v", kindsOf(events))
	}
	meta := string(bash.Meta)
	if !strings.Contains(meta, "Bash") {
		t.Errorf("tool start meta missing tool name: %s", meta)
	}
	if !strings.Contains(meta, "ls -la") {
		t.Errorf("tool start meta missing reassembled input: %s", meta)
	}

	// stop_reason tool_use ⇒ model not done ⇒ no turn-complete this round.
	if tc := findKind(events, provider.EventTurnComplete); len(tc) != 0 {
		t.Errorf("expected no turn-complete on tool_use, got %d", len(tc))
	}
}

// TestReconstructDoneTurnEmitsResult drives a text-only turn that ends with
// end_turn. The stream's message_delta yields a soft round-close, and the
// synthesized result envelope folds in cumulative usage. Both turn-completes
// fire, exactly as the headless path produces them.
func TestReconstructDoneTurnEmitsResult(t *testing.T) {
	sse := []string{
		`{"type":"message_start","message":{"id":"msg_2","model":"claude-haiku","role":"assistant","usage":{"input_tokens":20,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
	events := driveAgentRequest(t, agentReqBody, sse)

	completes := findKind(events, provider.EventTurnComplete)
	if len(completes) != 2 {
		t.Fatalf("expected 2 turn-complete events (soft + result), got %d: kinds %v", len(completes), kindsOf(events))
	}

	var sawSoft, sawWire bool
	for _, e := range completes {
		switch m := e.TurnComplete.(type) {
		case *provider.SoftRoundCloseMeta:
			sawSoft = true
			if m.StopReason != "end_turn" {
				t.Errorf("soft close stop_reason = %q, want end_turn", m.StopReason)
			}
		case *provider.WireTurnCompleteMeta:
			sawWire = true
			if m.StopReason != "end_turn" {
				t.Errorf("wire complete stop_reason = %q, want end_turn", m.StopReason)
			}
			if m.Usage == nil {
				t.Error("wire complete should carry accumulated usage")
			} else if m.Usage.OutputTokens != 5 || m.Usage.InputTokens != 20 {
				t.Errorf("wire usage = in:%d out:%d, want in:20 out:5", m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}
	}
	if !sawSoft {
		t.Error("expected a SoftRoundCloseMeta turn-complete from the stream")
	}
	if !sawWire {
		t.Error("expected a WireTurnCompleteMeta turn-complete from the synthesized result")
	}
}

// TestReconstructMultiRequestTurnSumsUsage proves the cross-request turn-usage
// accumulator: a turn made of two wire requests (tool round-trip) bills the sum
// of both requests' output, closing only when the second ends with end_turn.
func TestReconstructMultiRequestTurnSumsUsage(t *testing.T) {
	parser := claude.NewParser()
	var out []provider.ProviderEvent
	rec := newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		out = append(out, evs...)
	})

	_, req := classifyRequest([]byte(agentReqBody))

	// Request 1: emits a tool_use, stop tool_use (no result yet).
	ar1 := rec.beginAgentRequest(req)
	for _, s := range []string{
		`{"type":"message_start","message":{"id":"m1","model":"claude-haiku","usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":100,"output_tokens":7}}`,
	} {
		ar1.onSSE(json.RawMessage(s))
	}
	ar1.end()
	if len(findKind(out, provider.EventTurnComplete)) != 0 {
		t.Fatal("turn must not complete after the tool_use request")
	}

	// Request 2: the continuation, ends end_turn.
	ar2 := rec.beginAgentRequest(req)
	for _, s := range []string{
		`{"type":"message_start","message":{"id":"m2","model":"claude-haiku","usage":{"input_tokens":160,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":160,"output_tokens":9}}`,
	} {
		ar2.onSSE(json.RawMessage(s))
	}
	ar2.end()

	wire := wireComplete(t, findKind(out, provider.EventTurnComplete))
	if wire.Usage == nil {
		t.Fatal("wire complete missing usage")
	}
	// Output billed across both requests: 7 + 9 = 16; input summed: 100 + 160.
	if wire.Usage.OutputTokens != 16 {
		t.Errorf("summed output = %d, want 16", wire.Usage.OutputTokens)
	}
	if wire.Usage.InputTokens != 260 {
		t.Errorf("summed input = %d, want 260", wire.Usage.InputTokens)
	}
}

// TestReconstructInterrupt proves the no-control-ack interrupt path: the
// synthesized error_during_execution result classifies as an abort, so triage
// clears the working indicator as cancelled (not a hard failure).
func TestReconstructInterrupt(t *testing.T) {
	parser := claude.NewParser()
	var out []provider.ProviderEvent
	rec := newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		out = append(out, evs...)
	})
	rec.interruptTurn()

	wire := wireComplete(t, findKind(out, provider.EventTurnComplete))
	if !wire.Aborted {
		t.Error("interrupt should set Aborted=true")
	}
	if wire.StopReason != "interrupted" {
		t.Errorf("interrupt stop_reason = %q, want interrupted", wire.StopReason)
	}
	if wire.ErrorMessage != "" {
		t.Errorf("interrupt must not carry a hard error message, got %q", wire.ErrorMessage)
	}
}

// TestReconstructInterruptDoesNotLeakUsage guards the cross-turn billing leak:
// when an interrupt closes a turn mid-stream, the late end() of the aborted
// request must NOT bill its partial usage into the NEXT turn. Drives the
// leak-prone ordering (interruptTurn BEFORE the in-flight request's end()).
func TestReconstructInterruptDoesNotLeakUsage(t *testing.T) {
	parser := claude.NewParser()
	var out []provider.ProviderEvent
	rec := newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		out = append(out, evs...)
	})
	_, req := classifyRequest([]byte(agentReqBody))

	// Turn 1: a request that streams partial usage, then the user interrupts
	// mid-stream (no message_delta / stop_reason ever arrives). The Esc cancels
	// the upstream request, so interruptTurn lands BEFORE the stream's end().
	ar1 := rec.beginAgentRequest(req)
	ar1.onSSE(json.RawMessage(`{"type":"message_start","message":{"id":"m1","model":"claude-haiku","usage":{"input_tokens":50,"output_tokens":1}}}`))
	ar1.onSSE(json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	ar1.onSSE(json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hal"}}`))
	rec.interruptTurn() // user aborts
	ar1.end()           // late stream close for the aborted request

	// Turn 2: a fresh, clean turn.
	ar2 := rec.beginAgentRequest(req)
	for _, s := range []string{
		`{"type":"message_start","message":{"id":"m2","model":"claude-haiku","usage":{"input_tokens":200,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":200,"output_tokens":9}}`,
	} {
		ar2.onSSE(json.RawMessage(s))
	}
	ar2.end()

	// The second turn's wire usage must reflect ONLY its own request — the
	// aborted turn-1 request's 50/1 must not bleed in.
	var second *provider.WireTurnCompleteMeta
	for _, e := range findKind(out, provider.EventTurnComplete) {
		if m, ok := e.TurnComplete.(*provider.WireTurnCompleteMeta); ok && !m.Aborted {
			second = m
		}
	}
	if second == nil {
		t.Fatalf("no non-aborted wire turn-complete; kinds %v", kindsOf(out))
	}
	if second.Usage == nil {
		t.Fatal("second turn missing usage")
	}
	if second.Usage.InputTokens != 200 || second.Usage.OutputTokens != 9 {
		t.Errorf("second turn usage = in:%d out:%d, want in:200 out:9 (aborted request leaked in)",
			second.Usage.InputTokens, second.Usage.OutputTokens)
	}
}

func wireComplete(t *testing.T, completes []provider.ProviderEvent) *provider.WireTurnCompleteMeta {
	t.Helper()
	for _, e := range completes {
		if m, ok := e.TurnComplete.(*provider.WireTurnCompleteMeta); ok {
			return m
		}
	}
	t.Fatalf("no WireTurnCompleteMeta turn-complete among %d completes", len(completes))
	return nil
}
