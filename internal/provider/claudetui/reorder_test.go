package claudetui

import (
	"bytes"
	"encoding/json"
	"testing"
)

// envelopes used across the reorder unit tests.
const (
	envAssistantBash    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo hi"}}]}}`
	envCompletionBash   = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi"}]}}`
	envResultDone       = `{"type":"result","subtype":"success","stop_reason":"end_turn"}`
	envUserPlainMessage = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`
)

// TestFeedReorderHoldsCompletionUntilStart is the core fix: a hook tool_result
// that arrives BEFORE its assistant start (the wire+hook inversion) is held, then
// released right after the assistant that starts its tool_use_id is fed.
func TestFeedReorderHoldsCompletionUntilStart(t *testing.T) {
	fr := newFeedReorder()

	// Inverted arrival: completion first ⇒ held, nothing fed yet.
	if got := fr.admit(json.RawMessage(envCompletionBash)); got != nil {
		t.Fatalf("completion before its start must be held, got %d envelopes", len(got))
	}

	// The start arrives ⇒ assistant fed, then the held completion replayed after.
	got := fr.admit(json.RawMessage(envAssistantBash))
	if len(got) != 2 {
		t.Fatalf("admit(assistant) = %d envelopes, want 2 (assistant then released completion)", len(got))
	}
	if envelopeType(got[0]) != "assistant" {
		t.Errorf("first released = %q, want assistant", envelopeType(got[0]))
	}
	if id, ok := userToolResultID(got[1]); !ok || id != "toolu_1" {
		t.Errorf("second released should be the toolu_1 completion, got ok=%v id=%q", ok, id)
	}
}

// TestFeedReorderPassesThroughOrderedCompletion proves the non-inverted (slow
// tool) path is untouched: when the start has already been fed, its completion
// passes straight through with no buffering.
func TestFeedReorderPassesThroughOrderedCompletion(t *testing.T) {
	fr := newFeedReorder()

	start := fr.admit(json.RawMessage(envAssistantBash))
	if len(start) != 1 || envelopeType(start[0]) != "assistant" {
		t.Fatalf("admit(assistant) with no pending = %d envelopes, want just the assistant", len(start))
	}

	got := fr.admit(json.RawMessage(envCompletionBash))
	if len(got) != 1 {
		t.Fatalf("ordered completion should pass straight through, got %d envelopes", len(got))
	}
	if id, ok := userToolResultID(got[0]); !ok || id != "toolu_1" {
		t.Errorf("passthrough completion = ok=%v id=%q, want toolu_1", ok, id)
	}
}

// TestFeedReorderReleasesMultipleToolUses proves a single assistant envelope
// carrying several tool_use blocks releases every completion that raced ahead.
func TestFeedReorderReleasesMultipleToolUses(t *testing.T) {
	fr := newFeedReorder()
	assistant := `{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_a","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_b","name":"Read","input":{}}]}}`
	compA := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"a"}]}}`
	compB := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b","content":"b"}]}}`

	if got := fr.admit(json.RawMessage(compA)); got != nil {
		t.Fatalf("compA should be held, got %d", len(got))
	}
	if got := fr.admit(json.RawMessage(compB)); got != nil {
		t.Fatalf("compB should be held, got %d", len(got))
	}

	got := fr.admit(json.RawMessage(assistant))
	if len(got) != 3 {
		t.Fatalf("admit(assistant) = %d envelopes, want 3 (assistant + both completions)", len(got))
	}
	released := map[string]bool{}
	for _, env := range got[1:] {
		if id, ok := userToolResultID(env); ok {
			released[id] = true
		}
	}
	if !released["toolu_a"] || !released["toolu_b"] {
		t.Errorf("both held completions should release, got %v", released)
	}
}

// TestFeedReorderResultFlushesAndResets proves the turn-close path: a straggler
// whose start never arrived is flushed (not held forever), and per-turn state is
// reset so a later turn reusing nothing leaks — proven by the same tool_use_id
// being held again after the reset.
func TestFeedReorderResultFlushesAndResets(t *testing.T) {
	fr := newFeedReorder()

	// A completion whose start never comes this turn.
	if got := fr.admit(json.RawMessage(envCompletionBash)); got != nil {
		t.Fatalf("orphan completion should be held, got %d", len(got))
	}

	// Turn close flushes the straggler ahead of the result, then resets.
	got := fr.admit(json.RawMessage(envResultDone))
	if len(got) != 2 {
		t.Fatalf("admit(result) = %d envelopes, want 2 (flushed straggler + result)", len(got))
	}
	if envelopeType(got[len(got)-1]) != "result" {
		t.Errorf("result must be last, got %q", envelopeType(got[len(got)-1]))
	}

	// started was reset: feed the start again, then the completion must be held
	// again (a stale started flag would let it pass straight through).
	fr.admit(json.RawMessage(envResultDone)) // no-op flush, confirms idempotent reset
	if got := fr.admit(json.RawMessage(envCompletionBash)); got != nil {
		t.Fatalf("after reset, completion before a fresh start must be held again, got %d", len(got))
	}
}

// TestFeedReorderIgnoresNonToolResultUser proves a plain user message (no
// tool_result block) is never buffered — only hook completions are.
func TestFeedReorderIgnoresNonToolResultUser(t *testing.T) {
	fr := newFeedReorder()
	got := fr.admit(json.RawMessage(envUserPlainMessage))
	if len(got) != 1 {
		t.Fatalf("a non-tool_result user envelope must pass through, got %d", len(got))
	}
	if envelopeType(got[0]) != "user" {
		t.Errorf("passthrough = %q, want user", envelopeType(got[0]))
	}
}

// TestStreamEventLineHasReorderFastPathPrefix locks the coupling between the
// feedLoop hot-path prefix and the marshaled streamEventLine shape: if a struct
// field reorder ever moved "type" off the front, the fast path would silently
// stop matching and every delta would take the slow classification path.
func TestStreamEventLineHasReorderFastPathPrefix(t *testing.T) {
	line := streamEventLine(json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`), "")
	if !bytes.HasPrefix(line, streamEventPrefix) {
		t.Fatalf("streamEventLine output %s lost the %q fast-path prefix", line, streamEventPrefix)
	}
}

// TestEnvelopeExtractors covers the small JSON peekers the reorderer relies on.
func TestEnvelopeExtractors(t *testing.T) {
	if got := envelopeType(json.RawMessage(envAssistantBash)); got != "assistant" {
		t.Errorf("envelopeType(assistant) = %q", got)
	}
	if got := envelopeType(json.RawMessage(`not json`)); got != "" {
		t.Errorf("envelopeType(garbage) = %q, want empty", got)
	}
	ids := assistantToolUseIDs(json.RawMessage(envAssistantBash))
	if len(ids) != 1 || ids[0] != "toolu_1" {
		t.Errorf("assistantToolUseIDs = %v, want [toolu_1]", ids)
	}
	if id, ok := userToolResultID(json.RawMessage(envCompletionBash)); !ok || id != "toolu_1" {
		t.Errorf("userToolResultID = ok=%v id=%q, want toolu_1", ok, id)
	}
	if _, ok := userToolResultID(json.RawMessage(envUserPlainMessage)); ok {
		t.Error("userToolResultID on a plain user message should be ok=false")
	}
}
