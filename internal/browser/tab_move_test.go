package browser

import (
	"context"
	"slices"
	"testing"
)

// Close to the right of b, then a dragged after b, the move arriving
// first: the move waits for the close's turn, so the close reads the strip
// the user saw, closes c and d, and a survives behind b.
func TestAMoveWaitsForTheCloseBeforeIt(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	moving := deliver(func() error {
		return m.MoveCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[0]), TabPlace{Beside: PageID(ids[1]), After: true})
	})
	awaitWaiting(t, m, mount, 1)
	if got := tabIDs(m, access); !slices.Equal(got, ids) {
		t.Fatalf("tabs %v before the close's turn, want the move waiting %v", got, ids)
	}
	if err := m.CloseCompanionTabs(numbered(t.Context(), mount, 1), access, PageID(ids[1]), CloseTabsRight); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, moving, "the move of a")
	if got, want := tabIDs(m, access), []string{ids[1], ids[0]}; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want c and d closed and a moved after b %v", got, want)
	}
}

// a closed, then Reopen, then d dropped before c in the strip b c d the
// user saw, the move arriving first: the move lands beside c at its turn,
// after the reopen put a back in front, so a b d c, not the a d b c an
// index into the strip the user saw would give.
func TestAMoveLandsBesideItsNeighborAfterAReopenBeforeIt(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	moving := deliver(func() error {
		return m.MoveCompanionPage(numbered(context.Background(), mount, 3), access, PageID(ids[3]), TabPlace{Beside: PageID(ids[2])})
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.ReopenClosedTab(numbered(t.Context(), mount, 2), access); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, moving, "the move of d")
	if got, want := tabURLs(m, access), []string{addr("a"), addr("b"), addr("d"), addr("c")}; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want d before c with a back in front %v", got, want)
	}
}

// b dropped after c, and c closed by a command before it in the pane's
// order: the place the user chose is gone, so b stays where it is.
func TestAMoveBesideATabClosedBeforeItLeavesTheTab(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	moving := deliver(func() error {
		return m.MoveCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[1]), TabPlace{Beside: PageID(ids[2]), After: true})
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[2])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, moving, "the move of b")
	if got, want := tabIDs(m, access), []string{ids[0], ids[1], ids[3]}; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want b left where it was %v", got, want)
	}
}

// A move beside itself is refused in its turn, so the pane's next command
// does not wait for it.
func TestAMoveBesideItselfIsRefusedInItsTurn(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	if err := m.MoveCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[3]), TabPlace{Beside: PageID(ids[3])}); err == nil {
		t.Fatal("a move beside itself was accepted")
	}
	if err := m.MoveCompanionPage(numbered(t.Context(), mount, 2), access, PageID(ids[3]), TabPlace{Beside: PageID(ids[0])}); err != nil {
		t.Fatal(err)
	}
	if got, want := tabIDs(m, access), []string{ids[3], ids[0], ids[1], ids[2]}; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want d in front %v", got, want)
	}
}

// A passed move (a repeat of its number, or from a mount that is gone)
// moves nothing.
func TestAPassedMoveMovesNothing(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	if err := m.MoveCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[3]), TabPlace{Beside: PageID(ids[0])}); err != nil {
		t.Fatal(err)
	}
	moved := []string{ids[3], ids[0], ids[1], ids[2]}
	for _, at := range []Numbered{place(mount, 1), place("gone", 1)} {
		if err := m.MoveCompanionPage(WithSelection(t.Context(), at), access, PageID(ids[3]), TabPlace{Beside: PageID(ids[2]), After: true}); err != nil {
			t.Fatal(err)
		}
		if got := tabIDs(m, access); !slices.Equal(got, moved) {
			t.Fatalf("tabs %v after a passed move, want %v", got, moved)
		}
	}
}
