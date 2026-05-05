package triage

import (
	"strconv"
	"testing"
)

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
