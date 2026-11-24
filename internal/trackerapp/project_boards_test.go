package trackerapp

import (
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/tracker"
)

// listedBoards is the mirror fixture's project list with board left out.
func listedBoards(m *mirrorFixture, without tracker.BoardID) []ProjectBoard {
	var out []ProjectBoard
	for _, b := range testBoards {
		if b != without {
			out = append(out, ProjectBoard{Connection: m.conn.ID, Board: string(b)})
		}
	}
	return out
}

// loadedOrEnded waits for the first event of scope past seq that says the
// board was read (ReadAgain) or ended, and reports which.
func loadedOrEnded(t *testing.T, m *mirrorFixture, seq uint64) (ended bool) {
	t.Helper()
	var got BoardEvent
	m.events.waitFor(t, "the board's first read or its end", func(boards []BoardEvent, _ []IssueEvent, _ []error) bool {
		for _, e := range boards {
			if e.Scope == m.scope() && e.Seq > seq && (e.ReadAgain || e.Ended) {
				got = e
				return true
			}
		}
		return false
	})
	return got.Ended
}

// A board removed from the last project that lists it and added back at
// once opens and reads: the save that added it back left it a lifetime.
func TestABoardListedAgainOpensAndReads(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	first := m.subscribe(t, testBoard)
	if loadedOrEnded(t, m, first.Seq) {
		t.Fatal("the board ended before it was ever unlisted")
	}
	m.svc.ReleaseSubscription(first.ID)
	if _, err := m.svc.SetProjectBoards(m.project(), listedBoards(m, testBoard)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SetProjectBoards(m.project(), listedBoards(m, "")); err != nil {
		t.Fatal(err)
	}
	again := m.subscribe(t, testBoard)
	if loadedOrEnded(t, m, again.Seq) {
		t.Fatal("the board listed again ended at its first tick")
	}
}

// A save that leaves a board unlisted ends its open views before it
// answers, a hidden one, whose pump does not tick, included: each
// subscriber is told Ended, and the handle and its tie are gone.
func TestASaveThatUnlistsABoardEndsItsViews(t *testing.T) {
	t.Parallel()
	for name, hide := range map[string]bool{"shown": false, "hidden": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			ties := &tieLog{}
			sub, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, ties.tie)
			if err != nil {
				t.Fatal(err)
			}
			if loadedOrEnded(t, m, sub.Seq) {
				t.Fatal("the board ended before it was unlisted")
			}
			if hide {
				if err := m.svc.SetSubscriptionVisible(sub.ID, false); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.svc.SetProjectBoards(m.project(), listedBoards(m, testBoard)); err != nil {
				t.Fatal(err)
			}
			boards, _, _ := m.events.snapshot()
			if !slices.ContainsFunc(boards, func(e BoardEvent) bool { return e.Scope == m.scope() && e.Ended }) {
				t.Fatal("the save answered before the unlisted board's subscribers were told it ended")
			}
			if _, err := m.svc.SubscriptionBoard(sub.ID); !errors.Is(err, ErrUnknownSubscription) {
				t.Fatalf("the unlisted board's handle: %v", err)
			}
			if n := ties.untiedOf(sub.ID); n != 1 {
				t.Fatalf("the unlisted board's handle was untied %d times, want 1", n)
			}
		})
	}
}
