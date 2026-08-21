package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// externalTurnTestSession builds the minimum Session a dispatchLine test
// needs, with the root Codex thread id bound so notifications carrying that
// id route as the session's own rather than being quarantined as a foreign
// child thread.
func externalTurnTestSession(t *testing.T, codexThreadID string) (*Session, *[]provider.ProviderEvent) {
	t.Helper()
	events := &[]provider.ProviderEvent{}
	s := &Session{
		threadID: "ao-thread-1",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			*events = append(*events, evt)
		},
	}
	s.setRootThreadID(codexThreadID)
	return s, events
}

func metaValue(t *testing.T, meta json.RawMessage, key string) (any, bool) {
	t.Helper()
	if len(meta) == 0 {
		return nil, false
	}
	var decoded map[string]any
	if err := json.Unmarshal(meta, &decoded); err != nil {
		t.Fatalf("decode meta %s: %v", meta, err)
	}
	value, ok := decoded[key]
	return value, ok
}

// TestExternalTurnIsAdoptedAndMarked is item 4's core claim: a turn/started
// this session never asked for is still adopted as the active turn (so
// Interrupt and Steer address something) but every row it produces is
// stamped with a typed origin so it cannot be persisted as if the person in
// front of Agent Overflow typed it.
func TestExternalTurnIsAdoptedAndMarked(t *testing.T) {
	s, events := externalTurnTestSession(t, "codex-thread-1")

	logged := captureLog(t, func() {
		s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"codex-thread-1","turn":{"id":"turn-ext-1"}}}`))
		s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"codex-thread-1","turnId":"turn-ext-1","item":{"id":"item-1","type":"userMessage","content":[{"type":"text","text":"run the tests"}]}}}`))
	})

	if len(*events) != 2 {
		t.Fatalf("expected turn start + user text, got %+v", *events)
	}
	start, userText := (*events)[0], (*events)[1]
	if start.Kind != provider.EventTurnStart {
		t.Fatalf("first event kind: got %q, want %q", start.Kind, provider.EventTurnStart)
	}
	if got, ok := metaValue(t, start.Meta, "origin"); !ok || got != ExternalTurnOriginQueue {
		t.Errorf("turn start origin: got %v (present=%v), want %q", got, ok, ExternalTurnOriginQueue)
	}
	if userText.Kind != provider.EventUserText {
		t.Fatalf("second event kind: got %q, want %q", userText.Kind, provider.EventUserText)
	}
	if got, ok := metaValue(t, userText.Meta, "origin"); !ok || got != ExternalTurnOriginQueue {
		t.Errorf("user text origin: got %v (present=%v), want %q", got, ok, ExternalTurnOriginQueue)
	}
	// The pre-existing meta key must survive the stamp — triage reads it to
	// correlate the row.
	if got, ok := metaValue(t, userText.Meta, "provider_item_id"); !ok || got != "item-1" {
		t.Errorf("provider_item_id: got %v (present=%v), want item-1", got, ok)
	}

	// Adopted as active, or Interrupt/Steer would address nothing.
	s.mu.Lock()
	active := s.activeTurnID
	s.mu.Unlock()
	if active != "turn-ext-1" {
		t.Errorf("activeTurnID: got %q, want turn-ext-1", active)
	}

	if !strings.Contains(logged, "turn-ext-1") || !strings.Contains(logged, ExternalTurnOriginQueue) {
		t.Errorf("expected one info line naming the adopted turn and its origin, got %q", logged)
	}
	// Per turn, not per event.
	if got := strings.Count(logged, "adopted externally queued turn"); got != 1 {
		t.Errorf("adoption log lines: got %d, want 1", got)
	}
}

// TestLocallyStartedTurnIsNotMarked is the other half: a claim registered by
// Send survives the race where turn/started arrives BEFORE the turn/start
// response, which is the only reason the claim is a counter instead of a
// turn id.
func TestLocallyStartedTurnIsNotMarked(t *testing.T) {
	s, events := externalTurnTestSession(t, "codex-thread-1")

	s.beginLocalTurnStart() // Send, just before writing turn/start.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"codex-thread-1","turn":{"id":"turn-local-1"}}}`))
	s.bindLocalTurnStart("turn-local-1") // the response, landing second.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"codex-thread-1","turnId":"turn-local-1","item":{"id":"item-1","type":"userMessage","content":[{"type":"text","text":"hello"}]}}}`))

	if len(*events) != 2 {
		t.Fatalf("expected turn start + user text, got %+v", *events)
	}
	for _, evt := range *events {
		if got, ok := metaValue(t, evt.Meta, "origin"); ok {
			t.Errorf("%s carries origin %v; an AO-initiated turn must not be marked external", evt.Kind, got)
		}
	}
	if s.turnIsExternal("turn-local-1") {
		t.Error("turn-local-1 classified external despite an outstanding local claim")
	}
}

// TestAbandonedLocalTurnStartDoesNotAbsorbALaterTurn — a turn/start that
// failed outright releases its claim, so the NEXT turn (which may well be an
// injected one) is still classified honestly.
func TestAbandonedLocalTurnStartDoesNotAbsorbALaterTurn(t *testing.T) {
	s, _ := externalTurnTestSession(t, "codex-thread-1")

	s.beginLocalTurnStart()
	s.abandonLocalTurnStart() // turn/start returned an error.

	if s.adoptTurnStart("turn-ext-1") != turnAdoptionExternal {
		t.Error("turn after a failed turn/start read as local; the claim was not released")
	}

	// An empty turn id on the response releases the claim too — there is
	// nothing to bind it to and an unbound claim would silently absorb an
	// unrelated later turn.
	s.beginLocalTurnStart()
	s.bindLocalTurnStart("")
	if s.adoptTurnStart("turn-ext-2") != turnAdoptionExternal {
		t.Error("turn after an id-less turn/start response read as local")
	}
}

// TestTurnOriginIsForgottenOnCompletion keeps the map at one live entry for a
// healthy session, and the cap keeps a sick one bounded.
func TestTurnOriginIsForgottenOnCompletion(t *testing.T) {
	s, _ := externalTurnTestSession(t, "codex-thread-1")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"codex-thread-1","turn":{"id":"turn-ext-1"}}}`))
	if !s.turnIsExternal("turn-ext-1") {
		t.Fatal("turn-ext-1 not recorded as external")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"codex-thread-1","turn":{"id":"turn-ext-1","status":"completed"}}}`))

	s.mu.Lock()
	tracked := len(s.turnOrigins)
	s.mu.Unlock()
	if tracked != 0 {
		t.Errorf("turnOrigins after completion: %d entries, want 0", tracked)
	}

	// Turns whose completion never arrives must not grow the map without
	// bound; past the cap new turns read as local, the safe direction.
	var lastVerdict turnAdoption
	var lastTurnID string
	for i := range maxTrackedTurnOrigins + 10 {
		lastTurnID = string(rune('a'+i%26)) + string(rune('0'+i/26))
		lastVerdict = s.adoptTurnStart(lastTurnID)
	}
	s.mu.Lock()
	tracked = len(s.turnOrigins)
	s.mu.Unlock()
	if tracked > maxTrackedTurnOrigins {
		t.Errorf("turnOrigins grew to %d, cap is %d", tracked, maxTrackedTurnOrigins)
	}
	// The two halves of the cap must agree. At the cap nothing is recorded,
	// so turnIsExternal answers "local" for the turn — and adoptTurnStart has
	// to answer the same, or the turn/started would be stamped external while
	// every later event of that same turn was not.
	if lastVerdict != turnAdoptionLocal {
		t.Errorf("adoptTurnStart at the cap = %v, want turnAdoptionLocal to match turnIsExternal", lastVerdict)
	}
	if s.turnIsExternal(lastTurnID) {
		t.Errorf("turnIsExternal(%q) = true at the cap; nothing was recorded", lastTurnID)
	}
}

// TestThreadQueueChangedRaisesANotice covers the notification half of item 4.
// The wire payload is `{threadId}` and nothing else at rust-v0.149.0, so the
// notice deliberately reports no count.
func TestThreadQueueChangedRaisesANotice(t *testing.T) {
	s, events := externalTurnTestSession(t, "codex-thread-1")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/queue/changed","params":{"threadId":"codex-thread-1"}}`))

	if len(*events) != 1 {
		t.Fatalf("expected 1 notification, got %+v", *events)
	}
	evt := (*events)[0]
	if evt.Kind != provider.EventNotification {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventNotification)
	}
	if evt.Content != externalQueueNoticeText {
		t.Errorf("content: got %q, want %q", evt.Content, externalQueueNoticeText)
	}
	if got, ok := metaValue(t, evt.Meta, "kind"); !ok || got != "external_queue" {
		t.Errorf("meta.kind: got %v (present=%v), want external_queue", got, ok)
	}
	if got, ok := metaValue(t, evt.Meta, "origin"); !ok || got != ExternalTurnOriginQueue {
		t.Errorf("meta.origin: got %v (present=%v), want %q", got, ok, ExternalTurnOriginQueue)
	}
	// A handled method never reaches the protocol-drift log.
	if notificationMethodConsumed("thread/queue/changed") != true {
		t.Error("thread/queue/changed must count as consumed or it gets opted out at initialize")
	}
}

// TestStrictReviewRequiredRaisesAWarning covers the other newly handled 0.149
// notification: the `auto` tier's reviewer switching to the slow path is why
// a session that looks stalled is actually working.
func TestStrictReviewRequiredRaisesAWarning(t *testing.T) {
	s, events := externalTurnTestSession(t, "codex-thread-1")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"autoApprovalReview/strictReviewRequired","params":{"threadId":"codex-thread-1","turnId":"turn-1","startedAtMs":1700000000000}}`))

	if len(*events) != 1 {
		t.Fatalf("expected 1 notification, got %+v", *events)
	}
	evt := (*events)[0]
	if evt.Kind != provider.EventNotification {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventNotification)
	}
	if got, ok := metaValue(t, evt.Meta, "kind"); !ok || got != "warning" {
		t.Errorf("meta.kind: got %v (present=%v), want warning", got, ok)
	}
	if !strings.Contains(strings.ToLower(evt.Content), "safety checks") {
		t.Errorf("content %q does not explain the slowdown", evt.Content)
	}
	// The payload carries no reason field, so the copy must not claim one.
	if strings.Contains(strings.ToLower(evt.Content), "because") {
		t.Errorf("content %q names a cause the wire does not carry", evt.Content)
	}
}

// TestTimedOutTurnStartClaimDoesNotAbsorbALaterExternalTurn is finding 15. A
// `turn/start` that times out after its write leaves its claim outstanding on
// purpose — the turn may exist. What must not happen is that claim surviving
// forever: the next `turn/started` with nobody else to account for it is an
// EXTERNAL turn, and a surplus claim tells the user the injected prompt was
// their own.
func TestTimedOutTurnStartClaimDoesNotAbsorbALaterExternalTurn(t *testing.T) {
	t.Run("a later response naming a classified turn releases it", func(t *testing.T) {
		s := &Session{threadID: "ao-thread-1"}

		// The write went out, the ack never came back.
		s.beginLocalTurnStart()
		s.noteAmbiguousLocalTurnStart()

		// The retry's turn/started beats its response onto the read loop —
		// the race the claim counter exists for.
		s.beginLocalTurnStart()
		if got := s.adoptTurnStart("turn-retry"); got != turnAdoptionLocal {
			t.Fatalf("adoptTurnStart(retry) = %v, want local", got)
		}
		s.bindLocalTurnStart("turn-retry")

		if got := s.adoptTurnStart("turn-foreign"); got != turnAdoptionExternal {
			t.Errorf("adoptTurnStart(foreign) = %v, want external — the timed-out request's surplus claim absorbed an injected turn", got)
		}
	})

	t.Run("a turn boundary releases it", func(t *testing.T) {
		s := &Session{threadID: "ao-thread-1"}
		s.beginLocalTurnStart()
		s.noteAmbiguousLocalTurnStart()

		// Upstream runs one turn at a time per thread, so a turn the
		// timed-out request created would have had to start before this one
		// could finish.
		s.clearTurnStart("turn-other")

		if got := s.adoptTurnStart("turn-foreign"); got != turnAdoptionExternal {
			t.Errorf("adoptTurnStart(foreign) = %v, want external after a turn boundary retired the ambiguity", got)
		}
	})

	t.Run("the claim still covers its own turn until then", func(t *testing.T) {
		s := &Session{threadID: "ao-thread-1"}
		s.beginLocalTurnStart()
		s.noteAmbiguousLocalTurnStart()

		// The timed-out request DID create a turn and it starts late. This is
		// the case the outstanding claim exists for; releasing it eagerly
		// would mark AO's own turn as injected.
		if got := s.adoptTurnStart("turn-late"); got != turnAdoptionLocal {
			t.Errorf("adoptTurnStart(late) = %v, want local", got)
		}
	})

	t.Run("a definite failure needs no ambiguity at all", func(t *testing.T) {
		s := &Session{threadID: "ao-thread-1"}
		s.beginLocalTurnStart()
		s.abandonLocalTurnStart()
		if got := s.adoptTurnStart("turn-foreign"); got != turnAdoptionExternal {
			t.Errorf("adoptTurnStart(foreign) = %v, want external", got)
		}
	})
}
