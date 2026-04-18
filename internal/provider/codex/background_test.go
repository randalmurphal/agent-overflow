package codex

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// newTestClassifier is a tiny shortcut so each test reads closer to the
// scenario it's modeling instead of ceremony.
func newTestClassifier() *BackgroundClassifier {
	return NewBackgroundClassifier()
}

// runThroughClassifier threads a sequence of events through the classifier
// in order, returning the events (possibly mutated). Using pointers
// directly mirrors how session.go calls the classifier per event.
func runThroughClassifier(c *BackgroundClassifier, events []provider.ProviderEvent) []provider.ProviderEvent {
	for i := range events {
		c.Classify(&events[i])
	}
	return events
}

// metaIsBackground decodes evt.Meta and reports whether is_background is
// present and true. Returns false when Meta is nil, unparseable, or the
// key is absent — those are all equivalent to "the classifier did not
// mark it".
func metaIsBackground(t *testing.T, evt provider.ProviderEvent) bool {
	t.Helper()
	if len(evt.Meta) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(evt.Meta, &m); err != nil {
		t.Fatalf("unexpected unmarshal error on Meta %q: %v", string(evt.Meta), err)
	}
	raw, ok := m["is_background"]
	if !ok {
		return false
	}
	b, _ := raw.(bool)
	return b
}

// findEvent returns the first event matching kind and itemID. Tests fail
// fast if the event is missing — a missing EventToolComplete after a
// matched EventToolStart would indicate a classifier state-tracking bug
// well worth surfacing up rather than silently skipping.
func findEvent(t *testing.T, events []provider.ProviderEvent, kind provider.EventKind, itemID string) provider.ProviderEvent {
	t.Helper()
	for _, evt := range events {
		if evt.Kind == kind && evt.ItemID == itemID {
			return evt
		}
	}
	t.Fatalf("event not found: kind=%q itemID=%q (events=%+v)", kind, itemID, events)
	return provider.ProviderEvent{}
}

func TestBackground_SingleInlineCommand_NotBackground(t *testing.T) {
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if metaIsBackground(t, complete) {
		t.Errorf("inline command should not be marked background; meta=%s", string(complete.Meta))
	}
}

func TestBackground_MultipleQueuedInlineCommands_NotBackground(t *testing.T) {
	// Forge's "later turn activity" rule would incorrectly flag cmd-1 as
	// background here because cmd-2 and cmd-3 start while cmd-1 is still
	// open. Our rule ignores sibling tool starts — only assistant text
	// triggers background.
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventToolStart, ItemID: "cmd-2"},
		{Kind: provider.EventToolStart, ItemID: "cmd-3"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-2"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-3"},
	}
	out := runThroughClassifier(c, events)

	for _, id := range []string{"cmd-1", "cmd-2", "cmd-3"} {
		complete := findEvent(t, out, provider.EventToolComplete, id)
		if metaIsBackground(t, complete) {
			t.Errorf("queued sibling %s should not be marked background; meta=%s", id, string(complete.Meta))
		}
	}
}

func TestBackground_CommandPlusAssistantText_Background(t *testing.T) {
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "Running that now while I consider the next step.", Role: "assistant"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if !metaIsBackground(t, complete) {
		t.Errorf("command concurrent with assistant text should be background; meta=%s", string(complete.Meta))
	}
}

func TestBackground_TwoCommandsOneText_BothBackground(t *testing.T) {
	// Text fires once while both tools are open. Because the model is
	// emitting prose with two tools running, both are genuinely concurrent
	// and both should be flagged.
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventToolStart, ItemID: "cmd-2"},
		{Kind: provider.EventTextDelta, Content: "Both of those should take a bit.", Role: "assistant"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-2"},
	}
	out := runThroughClassifier(c, events)

	for _, id := range []string{"cmd-1", "cmd-2"} {
		complete := findEvent(t, out, provider.EventToolComplete, id)
		if !metaIsBackground(t, complete) {
			t.Errorf("%s should be marked background (text emitted while open); meta=%s", id, string(complete.Meta))
		}
	}
}

func TestBackground_SecondCommandStartsAfterText_OnlyFirstBackground(t *testing.T) {
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "Kicking the first one.", Role: "assistant"},
		{Kind: provider.EventToolStart, ItemID: "cmd-2"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-2"},
	}
	out := runThroughClassifier(c, events)

	complete1 := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if !metaIsBackground(t, complete1) {
		t.Errorf("cmd-1 was open during text, expected background; meta=%s", string(complete1.Meta))
	}
	complete2 := findEvent(t, out, provider.EventToolComplete, "cmd-2")
	if metaIsBackground(t, complete2) {
		t.Errorf("cmd-2 started after text, should not be background; meta=%s", string(complete2.Meta))
	}
}

func TestBackground_TextBeforeAnyTool_NoEffectOnLaterTools(t *testing.T) {
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventTextDelta, Content: "I'll run ls now.", Role: "assistant"},
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if metaIsBackground(t, complete) {
		t.Errorf("text before tool should not flag the later tool; meta=%s", string(complete.Meta))
	}
}

func TestBackground_TextAfterToolCompletes_NoRetroEffect(t *testing.T) {
	// Text fires after cmd-1 already completed. The classifier should
	// have removed cmd-1 from openToolCalls at complete time, so the
	// later text cannot retroactively flag it.
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "That worked.", Role: "assistant"},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if metaIsBackground(t, complete) {
		t.Errorf("post-completion text should not retroactively flag; meta=%s", string(complete.Meta))
	}
}

func TestBackground_TurnBoundaryClearsState(t *testing.T) {
	// Turn N opens cmd-1, text arrives, cmd-1 never completes (interrupted
	// turn — the session-logic path). Turn completes. Turn N+1 starts
	// cmd-2 and completes it. cmd-2 must NOT be flagged, because the text
	// that flagged cmd-1 was in an unrelated earlier turn.
	c := newTestClassifier()
	turn1 := []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, TurnID: "turn-1"},
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "Running.", Role: "assistant"},
		// cmd-1 has no complete — simulates interruption.
		{Kind: provider.EventTurnComplete, TurnID: "turn-1"},
	}
	turn2 := []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, TurnID: "turn-2"},
		{Kind: provider.EventToolStart, ItemID: "cmd-2"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-2"},
		{Kind: provider.EventTurnComplete, TurnID: "turn-2"},
	}

	runThroughClassifier(c, turn1)
	out := runThroughClassifier(c, turn2)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-2")
	if metaIsBackground(t, complete) {
		t.Errorf("turn-2 command must not inherit turn-1 background state; meta=%s", string(complete.Meta))
	}
}

func TestBackground_NonAssistantTextIgnored(t *testing.T) {
	// Tool-output-formatted text, user echo, and reasoning all come
	// through as EventTextDelta with a non-assistant role or no role at
	// all in some code paths. Those must not trigger background marking —
	// only assistant-authored prose interleaved with a running tool
	// qualifies.
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "user echo", Role: "user"},
		{Kind: provider.EventTextDelta, Content: "no role", Role: ""},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if metaIsBackground(t, complete) {
		t.Errorf("non-assistant text should not flag background; meta=%s", string(complete.Meta))
	}
}

func TestBackground_PreservesExistingMetaKeys(t *testing.T) {
	// The classifier merges is_background=true into evt.Meta. Existing
	// keys (source, item_status, nested params) from the protocol layer
	// must survive the merge so downstream can still read them.
	c := newTestClassifier()
	meta, _ := json.Marshal(map[string]any{
		"source":      "agent",
		"item_status": "completed",
		"item":        map[string]any{"id": "cmd-1", "type": "command_execution"},
	})
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "working...", Role: "assistant"},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1", Meta: meta},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	var merged map[string]any
	if err := json.Unmarshal(complete.Meta, &merged); err != nil {
		t.Fatalf("unmarshal merged meta: %v", err)
	}
	if merged["is_background"] != true {
		t.Errorf("is_background missing after merge: %+v", merged)
	}
	if merged["source"] != "agent" {
		t.Errorf("source lost after merge: %+v", merged)
	}
	if merged["item_status"] != "completed" {
		t.Errorf("item_status lost after merge: %+v", merged)
	}
	if _, ok := merged["item"]; !ok {
		t.Errorf("item raw payload lost after merge: %+v", merged)
	}
}

func TestBackground_ItemUpdatedDoesNotResetMarking(t *testing.T) {
	// item/updated emits EventToolStart with the same ItemID. If we
	// treated the update as a fresh registration we'd wipe the prior
	// background flag. Assert we don't.
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1", Timestamp: time.Now()},
		{Kind: provider.EventTextDelta, Content: "slow...", Role: "assistant"},
		// Second EventToolStart for the same ID (simulates item/updated).
		{Kind: provider.EventToolStart, ItemID: "cmd-1", Timestamp: time.Now(), Replace: true},
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)

	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if !metaIsBackground(t, complete) {
		t.Errorf("item/updated must not wipe background flag; meta=%s", string(complete.Meta))
	}
}

func TestBackground_ResetClearsState(t *testing.T) {
	c := newTestClassifier()
	_ = runThroughClassifier(c, []provider.ProviderEvent{
		{Kind: provider.EventToolStart, ItemID: "cmd-1"},
		{Kind: provider.EventTextDelta, Content: "text", Role: "assistant"},
	})
	c.Reset()

	// After Reset, a new EventToolComplete for cmd-1 should find no
	// entry and silently return (no panic, no mutation).
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)
	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if metaIsBackground(t, complete) {
		t.Errorf("after Reset, classifier must not carry state; meta=%s", string(complete.Meta))
	}
}

func TestBackground_NilEventIsSafe(t *testing.T) {
	c := newTestClassifier()
	// Should not panic.
	c.Classify(nil)
}

func TestBackground_CompleteWithoutStartIsSilent(t *testing.T) {
	// A stray EventToolComplete without a preceding EventToolStart (e.g.
	// a reconnect scenario where the start was missed) must not panic
	// and must not mark the event as background.
	c := newTestClassifier()
	events := []provider.ProviderEvent{
		{Kind: provider.EventToolComplete, ItemID: "cmd-1"},
	}
	out := runThroughClassifier(c, events)
	complete := findEvent(t, out, provider.EventToolComplete, "cmd-1")
	if metaIsBackground(t, complete) {
		t.Errorf("unseen tool must not be marked background; meta=%s", string(complete.Meta))
	}
}
