package identity

import (
	"fmt"
	"testing"
)

// TestProofReplayRemembersAcrossTheWholeAcceptanceWindow is the property
// the guard exists for, and the one an off-by-one would silently lose.
//
// A proof stamped T is acceptable for now anywhere in [T-freshness,
// T+freshness], so one proof is live across TWO freshness windows.
// Remembering for only one would leave a proof spendable twice inside its
// own lifetime — the guard would still pass a naive "is it remembered
// after one window" test while failing the case that matters.
func TestProofReplayRemembersAcrossTheWholeAcceptanceWindow(t *testing.T) {
	guard := newProofReplay()
	const start = 1_700_000_000_000

	if !guard.admit("jti-1", start) {
		t.Fatal("first presentation was refused")
	}
	// Walk to the far edge of the window in which that proof is still
	// acceptable, checking at every step that it stays spent.
	for at := int64(start); at <= start+2*deviceProofFreshness.Milliseconds(); at += 5_000 {
		if guard.admit("jti-1", at) {
			t.Fatalf("a spent proof was re-admitted %dms after it was spent, "+
				"inside the window where it still verifies", at-start)
		}
	}
}

// TestProofReplayCollectsWithoutScanning: entries do not accumulate past
// the window. Memory is bounded by the request RATE, not by uptime.
func TestProofReplayCollectsWithoutScanning(t *testing.T) {
	guard := newProofReplay()
	const start = 1_700_000_000_000
	window := proofReplayWindowMillis

	for i := range 100 {
		guard.admit(fmt.Sprintf("old-%d", i), start)
	}
	if guard.tracked() != 100 {
		t.Fatalf("tracked = %d, want 100", guard.tracked())
	}
	// Two rotations past those entries drops them entirely.
	guard.admit("marker", start+window+1)
	guard.admit("marker-2", start+2*window+2)
	if got := guard.tracked(); got > 2 {
		t.Fatalf("tracked = %d after two windows; the old generation was not dropped", got)
	}
	// And a long-collected identifier is spendable again, which is what
	// "bounded by the window" means and what a restart also produces.
	if !guard.admit("old-0", start+2*window+2) {
		t.Fatal("an identifier from two windows ago is still held")
	}
}

// TestProofReplayRotatesEarlyAtTheCap is the hard size cap of spec §14.
//
// It rotates rather than refusing. Refusing a fresh proof because the set
// is full would turn a burst into a sign-out for every real device on the
// machine, and reaching the cap inside one window requires producing
// thousands of proofs that ALREADY verified under a device's private key
// — so whoever can cause it holds the key the guard protects.
func TestProofReplayRotatesEarlyAtTheCap(t *testing.T) {
	guard := newProofReplay()
	const at = 1_700_000_000_000

	for i := range maxTrackedProofs * 3 {
		if !guard.admit(fmt.Sprintf("jti-%d", i), at) {
			t.Fatalf("a fresh identifier was refused at entry %d; the cap must "+
				"rotate rather than refuse", i)
		}
	}
	if got := guard.tracked(); got > 2*maxTrackedProofs {
		t.Fatalf("tracked = %d, want at most %d — the cap did not bound the set",
			got, 2*maxTrackedProofs)
	}
	// The most recent identifier is still held: rotation drops the OLD
	// generation, never the one being written.
	if guard.admit(fmt.Sprintf("jti-%d", maxTrackedProofs*3-1), at) {
		t.Fatal("the identifier just spent was re-admitted")
	}
}

// TestProofReplayIsSafeUnderConcurrentAdmits: two requests presenting one
// proof at the same instant must not both be admitted. Exactly one wins.
func TestProofReplayIsSafeUnderConcurrentAdmits(t *testing.T) {
	guard := newProofReplay()
	const at = 1_700_000_000_000
	const racers = 32

	results := make(chan bool, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			results <- guard.admit("contested", at)
		}()
	}
	close(start)

	admitted := 0
	for range racers {
		if <-results {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("%d of %d concurrent presentations of one proof were admitted, want 1",
			admitted, racers)
	}
}
