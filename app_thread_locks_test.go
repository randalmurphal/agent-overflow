package main

import (
	"testing"
	"time"

	"agent-overflow/internal/keyedlock"
)

// The registry itself is tested in `internal/keyedlock`. What is needed here is
// its one observable: a concurrent caller parked on a thread's action lock has
// performed no side effect yet, so its reference count is the only proof it is
// actually blocked rather than merely started.
func waitForThreadLockRefs(t *testing.T, locks *keyedlock.Registry, threadID string, want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if locks.Refs(threadID) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lock refs for %s = %d, want %d", threadID, locks.Refs(threadID), want)
}
