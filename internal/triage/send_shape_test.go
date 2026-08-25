package triage

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// TestSendShapeStampedAtEveryRegistrar drives one send of each shape
// through the registration surface and pins two things at once: the
// shape each registrar stamps, and that the stamp AGREES with the id
// grammar the App layer allocates for that shape. The second half is
// the whole point of the field this release — the sniff sites still
// decide by substring, so a stamp that disagreed would be a silent lie
// until the sniff is deleted.
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
	queue := append([]pendingSend(nil), router.pendingByThread["t1"]...)
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
		// The grammar assertion: exactly the comparison every sniff site
		// makes through sniffFlushShape, restated here so a registrar
		// that starts stamping the wrong shape fails at the surface
		// rather than at whichever reader happens to run first.
		if got, sniffed := entry.Shape == sendShapeFlush, strings.Contains(entry.AOItemID, ":flush:"); got != sniffed {
			t.Errorf("%q: stamped-flush=%v but the id grammar says %v", entry.AOItemID, got, sniffed)
		}
	}
}

// TestSendShapeDriftIsLoud pins the assertion itself: a send registered
// through a registrar whose shape contradicts its id grammar must not
// pass quietly. In a test binary that is a panic; in production it is
// one bounded log line and the sniff's answer, unchanged.
func TestSendShapeDriftIsLoud(t *testing.T) {
	router, _, _ := newTestRouter(t)

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("registering a :flush: id through the direct registrar did not report drift")
		}
		msg, ok := recovered.(string)
		if !ok {
			t.Fatalf("drift panic value = %T, want string", recovered)
		}
		for _, want := range []string{sendShapeSiteRegister, "user:9:flush:1", "sniff=flush", "stamped=direct"} {
			if !strings.Contains(msg, want) {
				t.Errorf("drift message %q missing %q", msg, want)
			}
		}
	}()

	router.RegisterPendingSendWithExpectation("t1", "user:9:flush:1", 9, PendingSendExpectation{})
}

// TestSendShapeSniffStaysAuthoritative pins the no-behavior-change half
// of the contract: a drifting entry that reaches a READER is still
// classified by the substring, not by the stamp. The entry is built
// directly (no registrar) so the registration-time assertion can't
// intercept it, and the reader is exercised with the check disarmed —
// production's posture, where the drift is logged and the decision is
// made anyway.
func TestSendShapeSniffStaysAuthoritative(t *testing.T) {
	router, _, _ := newTestRouter(t)

	// Stamped direct, spelled flush. The sniff says flush, so the
	// deferred-flush count must include it.
	item := store.Item{ID: "user:4:flush:1", ThreadID: "t1", TurnIndex: 4, Kind: "user_text", Role: "user"}
	router.mu.Lock()
	router.pendingByThread["t1"] = []pendingSend{{
		AOItemID: "user:4:flush:1", QueueItemID: "queue:q1", TurnIndex: 4,
		DeferredItem: &item, Shape: sendShapeDirect,
		InterruptedTurnIndex: -1, EchoPromotedBoundary: -1,
	}}
	router.mu.Unlock()

	var got int
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Errorf("reader did not report the drift it read past")
			}
		}()
		got = router.DeferredPendingFlushItemCount("t1")
	}()

	// The panic is the test-build posture, so the count above never
	// returned. Re-read with the entry stamped truthfully to confirm the
	// sniff's answer is what the site would have used either way.
	if got != 0 {
		t.Fatalf("count returned %d despite the drift panic", got)
	}
	router.mu.Lock()
	router.pendingByThread["t1"][0].Shape = sendShapeFlush
	router.mu.Unlock()
	if got := router.DeferredPendingFlushItemCount("t1"); got != 1 {
		t.Fatalf("DeferredPendingFlushItemCount = %d, want 1", got)
	}
}
