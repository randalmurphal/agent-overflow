package destination

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/browser/uicall"
)

// Each decision is taken once, by its completion or by the close, and a
// set closed refuses a later hold.
func TestDecisionsAreTakenOnce(t *testing.T) {
	s := NewDecisions[string, int]()
	if !s.Hold("a", 1) || !s.Hold("b", 2) {
		t.Fatal("an open set refused a decision")
	}
	if d, ok := s.Take("a"); !ok || d != 1 {
		t.Fatalf("take a = %d, %v", d, ok)
	}
	if _, ok := s.Take("a"); ok {
		t.Fatal("a decision was taken twice")
	}
	if got := s.Close(); !slices.Equal(got, []int{2}) {
		t.Fatalf("close took %v, want the open decision", got)
	}
	if _, ok := s.Take("b"); ok {
		t.Fatal("a decision the close took was taken again")
	}
	if s.Hold("c", 3) {
		t.Fatal("a closed set held a decision")
	}
	if got := s.Close(); len(got) != 0 {
		t.Fatalf("a second close took %v", got)
	}
}

// A worker's staged claim is released exactly once, unless a decision it
// completed handed it off: whether the completion took the decision, found
// it taken by the profile's close, or never ran because the UI loop went;
// a dispatch that timed out is tried again while the UI loop is there.
func TestSettleReleasesAClaimNoDecisionTook(t *testing.T) {
	// A loop runs a closure posted to it at once, or never.
	const runs, drops = true, false
	cases := []struct {
		name         string
		posts        []bool
		take         bool
		gone         bool
		wantReleases int
		wantComplete int
	}{
		{"completed and taken", []bool{runs}, true, false, 0, 1},
		{"completed after the close took it", []bool{runs}, false, false, 1, 1},
		{"the UI loop gone", []bool{drops}, true, true, 1, 0},
		{"timed out, then completed", []bool{drops, runs}, true, false, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			releases, completes, n := 0, 0, 0
			post := func(fn func()) {
				run := tc.posts[min(n, len(tc.posts)-1)]
				n++
				if run {
					fn()
				}
			}
			thread := uicall.NewThread(post, func() bool { return true }, time.Millisecond)
			Settle(thread, func() bool { return tc.gone },
				func() Staged { return Staged{Path: "/staged", Claim: NewClaim(func() { releases++ })} },
				func(Staged) bool { completes++; return tc.take })
			if releases != tc.wantReleases || completes != tc.wantComplete {
				t.Fatalf("released %d times and completed %d, want %d and %d", releases, completes, tc.wantReleases, tc.wantComplete)
			}
		})
	}
}

// A claim is released once however often Release is called, and not at
// all once handed off.
func TestClaimIsOneShot(t *testing.T) {
	n := 0
	c := NewClaim(func() { n++ })
	c.Release()
	c.Release()
	h := NewClaim(func() { n++ })
	h.HandOff()
	h.Release()
	if n != 1 {
		t.Fatalf("released %d times, want once", n)
	}
}
