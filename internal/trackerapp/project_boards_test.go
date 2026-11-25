package trackerapp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
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

// saveFromStored saves boards as a page that read the project's list just
// before.
func saveFromStored(m *mirrorFixture, boards []ProjectBoard) ([]BoardChoice, error) {
	listed, err := m.svc.ProjectBoards(m.project())
	if err != nil {
		return nil, err
	}
	held := make([]BoardRef, len(listed))
	for i, b := range listed {
		held[i] = BoardRef{Connection: b.Connection, Board: b.Board, OpensFirst: b.OpensFirst}
	}
	return m.svc.SetProjectBoards(m.project(), held, boards)
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
	if _, err := saveFromStored(m, listedBoards(m, testBoard)); err != nil {
		t.Fatal(err)
	}
	if _, err := saveFromStored(m, listedBoards(m, "")); err != nil {
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
			if _, err := saveFromStored(m, listedBoards(m, testBoard)); err != nil {
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

// A save does only what its own list needs: a reorder or a change of the
// board that opens first runs no cleanup, and neither does a save that
// unlists a board, whose stored copy the cleanup's own callers clear, or
// one that lists it again, which readies that board alone; none of them
// can fail on the cleanup.
func TestASaveRunsNoCleanup(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cleanups := &cleanupStore{Store: m.st, fail: errors.New("disk full")}
	m.svc.store = cleanups
	listed := listedBoards(m, "")
	reordered := append([]ProjectBoard{listed[len(listed)-1]}, listed[:len(listed)-1]...)
	reordered[0].OpensFirst = true
	opensSecond := slices.Clone(reordered)
	opensSecond[0].OpensFirst, opensSecond[1].OpensFirst = false, true
	for _, save := range []struct {
		name   string
		boards []ProjectBoard
	}{
		{"a reorder", reordered},
		{"an opens-first move", opensSecond},
		{"a save that unlists", listedBoards(m, testBoard)},
		{"a save that lists it again", listedBoards(m, "")},
	} {
		if _, err := saveFromStored(m, save.boards); err != nil {
			t.Fatalf("%s: %v", save.name, err)
		}
		if n := cleanups.calls.Load(); n != 0 {
			t.Fatalf("%s ran the mirror cleanup %d times, want none", save.name, n)
		}
	}
}

// readyFails is a store whose readying of the boards a save listed again
// fails.
type readyFails struct{ *store.Store }

func (readyFails) ReadyTrackerBoards(context.Context, []store.TrackerBoardRef) error {
	return errors.New("disk full")
}

// A save whose readying of a board listed again fails keeps the list and
// says that readying failed, and a save that readies nothing does not
// fail on it.
func TestASaveSaysTheReadyingFailed(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	sub := m.subscribe(t, testBoard)
	if loadedOrEnded(t, m, sub.Seq) {
		t.Fatal("the board ended before it was unlisted")
	}
	m.svc.store = readyFails{m.st}
	if _, err := saveFromStored(m, listedBoards(m, testBoard)); err != nil {
		t.Fatalf("a save that unlists: %v", err)
	}
	_, err := saveFromStored(m, listedBoards(m, ""))
	if err == nil || !strings.Contains(err.Error(), "readying the boards it listed again failed") {
		t.Fatalf("a save whose readying failed = %v", err)
	}
	if got, err := m.svc.ProjectBoards(m.project()); err != nil || len(got) != len(testBoards) {
		t.Fatalf("the list after a failed readying = %+v, %v", got, err)
	}
}

// A save answers the list as ProjectBoards reads it: each board named as
// the picker showed it when it was added, until this computer reads the
// board, with its connection; a read needs its project.
func TestProjectBoardsAreNamedAsTheyWereChosen(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	// The fixture lists the board unnamed; it is added again as chosen.
	if _, err := saveFromStored(m, nil); err != nil {
		t.Fatal(err)
	}
	boards := []ProjectBoard{{Connection: m.conn.ID, Board: string(testBoard), OpensFirst: true, Name: "WEB board", TrackerProject: "WEB"}}
	want := []BoardChoice{{
		Project: m.project(), Connection: m.conn.ID, ConnectionKind: m.conn.Kind, ConnectionLabel: m.conn.Label, ConnectionEnabled: true,
		VerifiedAccount: m.conn.VerifiedAccountID, Board: string(testBoard), Name: "WEB board", TrackerProject: "WEB", OpensFirst: true,
	}}
	if got, err := saveFromStored(m, boards); err != nil || !slices.Equal(got, want) {
		t.Fatalf("SetProjectBoards = %+v, %v; want %+v", got, err, want)
	}
	if got, err := m.svc.ProjectBoards(m.project()); err != nil || !slices.Equal(got, want) {
		t.Fatalf("ProjectBoards = %+v, %v; want %+v", got, err, want)
	}
	if got, err := m.svc.BoardChoices(); err != nil || !slices.Equal(got, want) {
		t.Fatalf("BoardChoices = %+v, %v; want %+v", got, err, want)
	}
	if got, err := m.svc.ProjectBoards(""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("ProjectBoards of no project = %+v, %v; want ErrProjectNotFound", got, err)
	}
}

// A save built from a list that is no longer the stored one is refused as
// a change, and the stored list stays.
func TestASaveFromAnOldListIsRefused(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	before, err := m.svc.ProjectBoards(m.project())
	if err != nil {
		t.Fatal(err)
	}
	held := make([]BoardRef, len(before))
	for i, b := range before {
		held[i] = BoardRef{Connection: b.Connection, Board: b.Board, OpensFirst: b.OpensFirst}
	}
	if _, err := m.svc.SetProjectBoards(m.project(), held, listedBoards(m, testBoard)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.SetProjectBoards(m.project(), held, listedBoards(m, "")); !errors.Is(err, ErrBoardsChanged) {
		t.Fatalf("a save from the list before another save = %v; want ErrBoardsChanged", err)
	}
	if got, err := m.svc.ProjectBoards(m.project()); err != nil || len(got) != len(testBoards)-1 {
		t.Fatalf("the list after the refused save = %+v, %v", got, err)
	}
}
