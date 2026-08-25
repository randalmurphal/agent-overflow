package triage

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// TestSendShapeStampedAtEveryRegistrar drives one send of each shape
// through the registration surface and pins two things at once: the
// shape each registrar stamps, and that the stamp AGREES with the id
// grammar the App layer allocates for that shape. The stamp is the
// authoritative flush classifier, so a registrar stamping the wrong
// shape would misplace queued user messages in the timeline.
func TestSendShapeStampedAtEveryRegistrar(t *testing.T) {
	router, _, _ := newTestRouter(t)

	flushItem := func(id string, turnIndex int) store.Item {
		return store.Item{
			ID: id, ThreadID: "t1", TurnIndex: turnIndex,
			Kind: "user_text", Role: "user", Status: "completed",
			Summary: "queued text",
		}
	}

	// One send per registrar, each carrying the id grammar its App-layer
	// allocator mints: `user:<turn>` (app_send.go), `user:<turn>:steer:<n>`
	// (app_steer.go), `user:<turn>:flush:<n>` (app_flush_queue.go's
	// nextFlushUserItemID — the one flush-id mint site).
	router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{})
	router.RegisterPendingSteerSendWithExpectation("t1", "user:0:steer:1", 0, PendingSendExpectation{ByClientID: true})
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:deferred", flushItem("user:1:flush:1", 1), 10, PendingSendExpectation{})
	router.RegisterPendingQuietFlushSendWithExpectation("t1", "queue:quiet", flushItem("user:1:flush:2", 1), 2, 20, PendingSendExpectation{})
	router.RegisterPendingFlushResendWithExpectation("t1", "user:1:flush:3", 3, PendingSendExpectation{ByClientID: true})

	want := map[string]sendShape{
		"user:0":         sendShapeDirect,
		"user:0:steer:1": sendShapeSteer,
		"user:1:flush:1": sendShapeFlush,
		"user:1:flush:2": sendShapeFlush,
		"user:1:flush:3": sendShapeFlush,
	}

	router.mu.Lock()
	queue := append([]pendingSend(nil), router.state("t1").pendingSends...)
	router.mu.Unlock()

	if len(queue) != len(want) {
		t.Fatalf("registered %d entries, want %d: %+v", len(queue), len(want), queue)
	}
	for _, entry := range queue {
		expected, known := want[entry.AOItemID]
		if !known {
			t.Fatalf("unexpected pending entry %q", entry.AOItemID)
		}
		if entry.Shape != expected {
			t.Errorf("%q stamped shape %s, want %s", entry.AOItemID, entry.Shape, expected)
		}
		// The grammar assertion, restated independently of
		// assertSendShapeMatchesID so a registrar that starts stamping
		// the wrong shape fails here even if the tripwire is edited.
		if got, grammar := entry.Shape == sendShapeFlush, strings.Contains(entry.AOItemID, ":flush:"); got != grammar {
			t.Errorf("%q: stamped-flush=%v but the id grammar says %v", entry.AOItemID, got, grammar)
		}
	}
}

// TestSendShapeMismatchPanicsAtRegistration pins the tripwire: a send
// registered through a registrar whose shape contradicts its id grammar
// must panic in a test binary. Registration is the only moment the two
// can disagree — both are AO-authored in the same call and immutable
// afterwards — so this is the whole enforcement surface.
func TestSendShapeMismatchPanicsAtRegistration(t *testing.T) {
	router, _, _ := newTestRouter(t)

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("registering a :flush: id through the direct registrar did not panic")
		}
		msg, ok := recovered.(string)
		if !ok {
			t.Fatalf("mismatch panic value = %T, want string", recovered)
		}
		for _, want := range []string{"send-shape mismatch at registration", "user:9:flush:1", "stamped=direct"} {
			if !strings.Contains(msg, want) {
				t.Errorf("mismatch message %q missing %q", msg, want)
			}
		}
	}()

	router.RegisterPendingSendWithExpectation("t1", "user:9:flush:1", 9, PendingSendExpectation{})
}

// TestSendShapeStampDecides pins that a reader classifies by the STAMP,
// not the id text: an entry stamped direct is excluded from the
// deferred-flush count even when its id happens to contain ":flush:".
// The entry is built directly (no registrar) because the registration
// assertion makes that state unreachable through the public surface —
// which is exactly why the readers may trust the stamp.
func TestSendShapeStampDecides(t *testing.T) {
	router, _, _ := newTestRouter(t)

	item := store.Item{ID: "user:4:flush:1", ThreadID: "t1", TurnIndex: 4, Kind: "user_text", Role: "user"}
	router.mu.Lock()
	router.state("t1").pendingSends = []pendingSend{{
		AOItemID: "user:4:flush:1", QueueItemID: "queue:q1", TurnIndex: 4,
		DeferredItem: &item, Shape: sendShapeDirect,
		InterruptedTurnIndex: -1, EchoPromotedBoundary: -1,
	}}
	router.mu.Unlock()

	if got := router.DeferredPendingFlushItemCount("t1"); got != 0 {
		t.Fatalf("DeferredPendingFlushItemCount = %d for a direct-stamped entry, want 0", got)
	}

	router.mu.Lock()
	router.state("t1").pendingSends[0].Shape = sendShapeFlush
	router.mu.Unlock()
	if got := router.DeferredPendingFlushItemCount("t1"); got != 1 {
		t.Fatalf("DeferredPendingFlushItemCount = %d, want 1", got)
	}
}
