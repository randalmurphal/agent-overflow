package claudetui

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

// overloadedErrorSSE is a mid-stream Anthropic `error` frame carrying
// overloaded_error — the shape the API delivers after HTTP 200 when it sheds
// load, and the one Claude Code's withRetry catches (by string-matching
// "overloaded_error") to re-POST. Verbatim from the 2026-06-14 incident
// (thread 4d82b192) provider-events log.
func overloadedErrorSSE() []string {
	return []string{
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	}
}

// apiRetryMetaOf decodes the attempt/error fields the shared parser normalizes
// onto an EventAPIRetry (buildClaudeAPIRetryMeta's {attempt, max_retries, error}).
func apiRetryMetaOf(t *testing.T, ev provider.ProviderEvent) (attempt int, errMsg string) {
	t.Helper()
	var m struct {
		Attempt int    `json:"attempt"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(ev.Meta, &m); err != nil {
		t.Fatalf("unmarshal api_retry meta %s: %v", ev.Meta, err)
	}
	return m.Attempt, m.Error
}

// TestReconstructOverloadedErrorEmitsApiRetry pins the reliability fix for the
// 2026-06-14 gateway-overload incident: a mid-stream overloaded_error must
// reconstruct into the same system.api_retry the headless path emits, so the TUI
// pane renders the retry (and triage's hide-under-4 gate suppresses the first
// three) instead of dropping the frame and stalling.
//
// Without the fix the error frame falls through parse_stream.go's default case
// and is dropped, so zero EventAPIRetry events are emitted — this test is RED on
// the first assertion. With the fix each failed attempt emits api_retry with the
// 1-indexed attempt that just failed (mirroring withRetry.ts), carrying the real
// error copy ("Overloaded"), and a successful completion resets the count so a
// later, independent overload starts back at attempt 1.
func TestReconstructOverloadedErrorEmitsApiRetry(t *testing.T) {
	rp := newReconParser(t)
	body := newTurnReqBody("remove the file")

	// Attempt 1 fails: idle send opens the turn (init + echo), then the overload
	// frame. withRetry will re-POST, so the loop must stay open.
	rp.drive("uuid-1", body, overloadedErrorSSE())
	retries := findKind(rp.out, provider.EventAPIRetry)
	if len(retries) != 1 {
		t.Fatalf("after attempt 1: api_retry count=%d want 1 (kinds %v)", len(retries), kindsOf(rp.out))
	}
	if attempt, errMsg := apiRetryMetaOf(t, retries[0]); attempt != 1 || errMsg != "Overloaded" {
		t.Fatalf("after attempt 1: api_retry attempt=%d error=%q want attempt=1 error=\"Overloaded\"", attempt, errMsg)
	}

	// Attempts 2 and 3: withRetry re-POSTs the same request (no echo, no init —
	// the loop is still open). Each failed attempt increments the count.
	rp.drive("", body, overloadedErrorSSE())
	rp.drive("", body, overloadedErrorSSE())
	retries = findKind(rp.out, provider.EventAPIRetry)
	if len(retries) != 3 {
		t.Fatalf("after attempt 3: api_retry count=%d want 3 (kinds %v)", len(retries), kindsOf(rp.out))
	}
	if attempt, _ := apiRetryMetaOf(t, retries[2]); attempt != 3 {
		t.Fatalf("after attempt 3: api_retry attempt=%d want 3", attempt)
	}

	// Attempt 4 succeeds (end_turn) → settles the turn AND resets the attempt
	// count for the next logical request.
	rp.drive("", body, endTurnSSE())

	// A later, independent overload starts a fresh retry sequence at attempt 1,
	// proving the reset (otherwise it would read attempt 4 and cross the
	// hide-under-4 threshold the real TUI keeps silent).
	rp.drive("uuid-2", newTurnReqBody("next task"), overloadedErrorSSE())
	retries = findKind(rp.out, provider.EventAPIRetry)
	if len(retries) != 4 {
		t.Fatalf("after later overload: api_retry count=%d want 4 (kinds %v)", len(retries), kindsOf(rp.out))
	}
	if attempt, _ := apiRetryMetaOf(t, retries[3]); attempt != 1 {
		t.Fatalf("after later overload: api_retry attempt=%d want 1 — a successful completion must reset the count", attempt)
	}
}

// nonOverloadErrorSSE is a mid-stream error frame that is NOT overloaded_error.
// withRetry does not re-POST a status-less non-overload mid-stream error, so the
// reconstruction must NOT model it as a retry.
func nonOverloadErrorSSE() []string {
	return []string{
		`{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`,
	}
}

// contentThenOverloadSSE streams real assistant content (text + a tool_use start)
// and THEN an overload frame mid-stream — the shape that proves a failed attempt
// discards its partial output instead of surfacing an orphaned tool_use.
func contentThenOverloadSSE(toolUseID string) []string {
	return []string{
		`{"type":"message_start","message":{"id":"msg_p","model":"claude-haiku","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":%q,"name":"Bash","input":{}}}`, toolUseID),
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	}
}

// TestReconstructToolUseStopResetsRetryCount pins that the attempt count resets on
// ANY terminal stop_reason, not only a soft round close. A request that overloads,
// then a retry that succeeds with stop=tool_use (model not done — the turn stays
// OPEN, no result), must reset the count so the next failure reads attempt 1. A
// regression that narrowed the reset back to IsSoftRoundCloseStopReason would read
// attempt 2 here (the tool_use success did not settle the turn).
func TestReconstructToolUseStopResetsRetryCount(t *testing.T) {
	rp := newReconParser(t)
	body := newTurnReqBody("do work")

	rp.drive("uuid-1", body, overloadedErrorSSE())                      // attempt 1 fails
	rp.drive("", continuationReqBody("toolu_x"), toolUseSSE("toolu_x")) // succeeds, stop=tool_use, turn stays open
	rp.drive("", continuationReqBody("toolu_x"), overloadedErrorSSE())  // overloads again

	retries := findKind(rp.out, provider.EventAPIRetry)
	if len(retries) != 2 {
		t.Fatalf("api_retry count=%d want 2 (kinds %v)", len(retries), kindsOf(rp.out))
	}
	if attempt, _ := apiRetryMetaOf(t, retries[1]); attempt != 1 {
		t.Fatalf("after tool_use success then overload: attempt=%d want 1 — a tool_use stop (non-soft-close) must reset the count", attempt)
	}
}

// TestReconstructNonRetryableErrorNotSurfacedAsRetry pins that a non-overload
// mid-stream error frame is NOT modeled as a retry (which would be a perpetual
// hidden api_retry that never settles). It falls through to the passthrough so any
// trailing terminal envelope still reaches the parser; surfacing it as a terminal
// EventError is a separate, spike-gated follow-on.
func TestReconstructNonRetryableErrorNotSurfacedAsRetry(t *testing.T) {
	rp := newReconParser(t)
	rp.drive("uuid-1", newTurnReqBody("x"), nonOverloadErrorSSE())
	if got := len(findKind(rp.out, provider.EventAPIRetry)); got != 0 {
		t.Fatalf("non-overload error produced %d api_retry, want 0 (kinds %v)", got, kindsOf(rp.out))
	}
}

// TestReconstructFailedAttemptDiscardsPartialOutput pins that an overloaded
// attempt which streamed real content (text + a tool_use start) emits NO
// EventToolStart — the assembled assistant envelope (the sole tool-start source)
// is skipped, so the failed attempt's partial tool_use never becomes an orphaned
// tool_call row. The successful retry re-streams the real content.
func TestReconstructFailedAttemptDiscardsPartialOutput(t *testing.T) {
	rp := newReconParser(t)
	rp.drive("uuid-1", newTurnReqBody("x"), contentThenOverloadSSE("toolu_partial"))

	if got := len(findKind(rp.out, provider.EventAPIRetry)); got != 1 {
		t.Fatalf("api_retry count=%d want 1 (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(findKind(rp.out, provider.EventToolStart)); got != 0 {
		t.Fatalf("failed attempt emitted %d tool starts, want 0 — the partial tool_use must be discarded, not orphaned", got)
	}
}

// TestReconstructSubagentErrorFrameNotSurfacedAsRetry pins that an overload frame
// on a SUBAGENT request (parent != "") is not surfaced as a turn-level api_retry —
// onSSE gates the retry detection on the main loop. The subagent's assistant
// envelope is still emitted (passthrough preserved).
func TestReconstructSubagentErrorFrameNotSurfacedAsRetry(t *testing.T) {
	rp := newReconParser(t)
	ar := rp.rec.beginSubagentRequest("toolu_parent")
	for _, s := range overloadedErrorSSE() {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()

	if got := len(findKind(rp.out, provider.EventAPIRetry)); got != 0 {
		t.Fatalf("subagent overload produced %d api_retry, want 0 — subagent retries are not turn-level (kinds %v)", got, kindsOf(rp.out))
	}
}

// TestReconstructInterruptThenErroredNoPhantomRetry pins the interrupt-vs-errored
// ordering fix: if an overload frame arrives and the user then hits Esc
// (interruptTurn closes and resets the turn) BEFORE the aborted request's end()
// runs, end() must emit NO phantom api_retry and must NOT leak a stale attempt
// count into the next turn. Without the interrupted short-circuit, end()'s errored
// branch increments the count and emits an api_retry after the interrupt result.
func TestReconstructInterruptThenErroredNoPhantomRetry(t *testing.T) {
	rp := newReconParser(t)

	// Open a turn, take an overload frame, then interrupt before end().
	rp.rec.pushUserEcho("x", "uuid-1")
	_, req := classifyRequest([]byte(newTurnReqBody("x")))
	ar := rp.rec.beginAgentRequest(req)
	ar.onSSE(json.RawMessage(overloadedErrorSSE()[0])) // ar.errored = true
	rp.rec.interruptTurn()                             // user Esc: emits interrupt result, resets count
	ar.end()                                           // errored + interrupted → must emit nothing

	if got := len(findKind(rp.out, provider.EventAPIRetry)); got != 0 {
		t.Fatalf("interrupt-then-errored produced %d phantom api_retry, want 0 (kinds %v)", got, kindsOf(rp.out))
	}

	// The stale count must not leak: a fresh turn that overloads reads attempt 1.
	rp.drive("uuid-2", newTurnReqBody("again"), overloadedErrorSSE())
	retries := findKind(rp.out, provider.EventAPIRetry)
	if len(retries) != 1 {
		t.Fatalf("after fresh overload: api_retry count=%d want 1 (kinds %v)", len(retries), kindsOf(rp.out))
	}
	if attempt, _ := apiRetryMetaOf(t, retries[0]); attempt != 1 {
		t.Fatalf("after interrupt then fresh overload: attempt=%d want 1 — interrupt must reset the count, no leak", attempt)
	}
}
