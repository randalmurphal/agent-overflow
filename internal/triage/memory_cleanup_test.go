package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestCapturedTurnsClearOnTurnComplete pins the cleanup behaviour added
// alongside the `capturedTurns` bounded-growth fix: the per-turn guard
// that stops checkpoint double-capture when a provider re-sends
// EventTurnStart must be dropped at turn-complete time instead of
// lingering until CleanupThread. Before the fix a long-running session
// would accumulate one entry per turn for the life of the thread.
func TestCapturedTurnsClearOnTurnComplete(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Prime the guard the same way a real checkpoint capture would:
	// mark (thread, turn 0) as captured, then trigger handleTurnComplete
	// and assert the map entry is gone.
	if router.markTurnCaptured("t1", 0) {
		t.Fatal("markTurnCaptured on an empty guard should return false (fresh mark)")
	}
	router.mu.Lock()
	primedLen := len(router.capturedTurns)
	router.mu.Unlock()
	if primedLen == 0 {
		t.Fatal("capturedTurns not primed after markTurnCaptured")
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	router.mu.Lock()
	remaining := len(router.capturedTurns)
	router.mu.Unlock()
	if remaining != 0 {
		t.Errorf("capturedTurns after turn-complete = %d, want 0 (cleanup regressed)", remaining)
	}
}

// TestUnknownSessionStatusLoggedStaysBounded pins the soft cap on the
// per-process throttle map. Without a cap a malicious or buggy provider
// could flood unique content strings and grow the map without bound;
// the cap drops older entries wholesale once the threshold is crossed.
func TestUnknownSessionStatusLoggedStaysBounded(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Sanity: cap is small enough that we can exercise it in test time.
	if unknownSessionStatusCap > 1024 {
		t.Skipf("cap too large for a test budget: %d", unknownSessionStatusCap)
	}

	// Flood with unique unknown content strings — each is a new key.
	// This tests the cap behaviour without requiring a full provider
	// event flow: logUnknownSessionStatusOnce is package-private so
	// the test can drive it directly.
	for i := 0; i <= unknownSessionStatusCap+5; i++ {
		router.logUnknownSessionStatusOnce(uniqueTestStatus(i))
	}

	router.mu.Lock()
	size := len(router.unknownSessionStatusLogged)
	router.mu.Unlock()
	if size > unknownSessionStatusCap+1 {
		t.Errorf("unknownSessionStatusLogged grew past cap: size=%d, cap=%d", size, unknownSessionStatusCap)
	}
}

// uniqueTestStatus generates a distinct content string per index so
// each logUnknownSessionStatusOnce call writes a new map key.
func uniqueTestStatus(i int) string {
	const prefix = "test-unknown-"
	// strconv is heavier than needed for a single int; hand-roll to keep
	// the test file import surface tight.
	return prefix + itoaAscii(i)
}

func itoaAscii(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
