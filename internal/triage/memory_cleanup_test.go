package triage

import (
	"strconv"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestCapturedTurnsLifecycle pins the new lifecycle for capturedTurns
// under the user-send-time capture model. capturedTurns is now used by
// TWO sites: captureBaselineForTurn (dedups baseline at turn 0 against
// re-fired EventTurnStart) and capturePriorTurnCheckpoint (dedups the
// prior-turn capture at next-user-send against re-init resends). Both
// marks survive turn-complete; only CleanupThread clears them. This is
// a deliberate trade — the map grows linearly with turns within a
// thread but the entries are tiny and CleanupThread bounds the
// session. The earlier "drop the (turnIndex-1) entry on every
// turn-complete" optimization was tied to the old turn-end capture
// model and would now silently allow re-fired EventTurnStart to
// re-capture, defeating the dedup guard.
func TestCapturedTurnsLifecycle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Prime the guard the same way a real checkpoint capture would.
	if router.markTurnCaptured("t1", 0) {
		t.Fatal("markTurnCaptured on an empty guard should return false (fresh mark)")
	}
	router.mu.Lock()
	primedLen := len(router.capturedTurns)
	router.mu.Unlock()
	if primedLen != 1 {
		t.Fatalf("capturedTurns after prime = %d, want 1", primedLen)
	}

	// Turn-complete must NOT clear the mark — the dedup needs to hold
	// through any re-fired EventTurnStart that targets the same turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	router.mu.Lock()
	afterComplete := len(router.capturedTurns)
	router.mu.Unlock()
	if afterComplete != 1 {
		t.Errorf("capturedTurns after turn-complete = %d, want 1 (mark must survive turn-end so re-fired EventTurnStart still dedups)", afterComplete)
	}

	// CleanupThread is the authoritative cleanup point.
	router.CleanupThread("t1")
	router.mu.Lock()
	afterCleanup := len(router.capturedTurns)
	router.mu.Unlock()
	if afterCleanup != 0 {
		t.Errorf("capturedTurns after CleanupThread = %d, want 0", afterCleanup)
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
	return "test-unknown-" + strconv.Itoa(i)
}
