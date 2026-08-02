package codex

import (
	"encoding/json"
	"testing"
	"time"
)

// rootThreadHandshakeTimeout bounds the concurrent phase below. It only
// fires if a read path blocks forever — which is what folding the root
// thread id back under s.mu would do to the two readers that already hold
// it (registerChildOwnershipWithSource, collabProfileForThread). Without
// the bound that regression shows up as a package-wide test timeout with
// no attribution.
const rootThreadHandshakeTimeout = 15 * time.Second

// TestRootThreadIDSurvivesHandshakeWindow reproduces the window NewSession
// opens. readLoop has to start before the thread/start response can be
// read, so the constructor learns the Codex thread id (setRootThreadID)
// while the read loop is already dispatching notifications that consult it.
// While the id was a plain string field, that pairing was an unsynchronized
// read/write and `go test -race` failed roughly one run in ten with no
// stable reproducer.
//
// The reader side drives the three shapes of read that exist: through the
// real dispatch entry point, through the fail-closed routing predicate, and
// through a reader that is already holding s.mu.
func TestRootThreadIDSurvivesHandshakeWindow(t *testing.T) {
	const rootThread = "019fc2ff-9050-7971-ac4e-b902cc3b9f00"
	const settingsFrame = `{"threadId":"` + rootThread + `","threadSettings":{"model":"gpt-5.6-sol"}}`

	s := &Session{threadID: testThread}
	params := json.RawMessage(settingsFrame)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			// Root-thread params on purpose: a foreign thread id would take
			// the deferral path and leave an ownership-timeout timer running
			// past the end of the test.
			s.dispatchNotification("thread/settings/updated", params)
			s.isUnmappedForeignProviderThread("child-thread")
			s.collabProfileForThread("child-thread")
		}
	}()

	// Stands in for NewSession's post-handshake write. It must come after
	// the `go` statement: starting the goroutine is itself a happens-before
	// edge, and ordering the write ahead of it would hide the very race
	// this test exists to catch.
	s.setRootThreadID(rootThread)

	select {
	case <-done:
	case <-time.After(rootThreadHandshakeTimeout):
		t.Fatal("read-loop stand-in never finished; a root-thread-id reader is deadlocked")
	}

	if got := s.rootThreadID(); got != rootThread {
		t.Fatalf("rootThreadID() = %q, want %q", got, rootThread)
	}
	// One dispatch with the id settled proves the loop above was exercising
	// a live path rather than bailing out early on every iteration.
	s.dispatchNotification("thread/settings/updated", params)
	if got, known := s.ObservedThreadSettings(); !known || got.Model != "gpt-5.6-sol" {
		t.Fatalf("settings never reconciled through dispatch: known=%v got=%+v", known, got)
	}
}
