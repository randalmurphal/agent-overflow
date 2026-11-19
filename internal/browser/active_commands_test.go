package browser

import (
	"context"
	"slices"
	"testing"
)

// The keyboard's close names no tab and takes its turn in the user's order:
// the user selects suspended b, presses Mod+W, then selects a, and the three
// arrive last first. The close closes b while b's restore is still held,
// without waiting for it, and the later selection of a is not closed.
func TestAKeyboardCloseClosesTheTabItsTurnLeavesActive(t *testing.T) {
	m, engine, access, a, b, mount := newSuspendedPair(t)
	c := mustOpen(t, m, access, addr("c"))
	m.setActivePage(access.ThreadID, c.ID)
	release := engine.hold(&engine.navGate)
	defer release()
	selectA := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, a.ID)
	})
	awaitWaiting(t, m, mount, 1)
	closing := deliver(func() error {
		return m.CloseActiveCompanionPage(numbered(context.Background(), mount, 2), access)
	})
	awaitWaiting(t, m, mount, 2)
	selectB := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 1), access, b.ID)
	})
	receive(t, engine.navigating, "b's restore")
	// b's restore is held: the close does not wait for it.
	mustDeliver(t, closing, "the keyboard close")
	if _, open := findTab(m, access, b.ID); open {
		t.Fatalf("b is still open after the close: %v", tabIDs(m, access))
	}
	release()
	mustDeliver(t, selectB, "the selection of b")
	mustDeliver(t, selectA, "the selection of a")
	if got := tabIDs(m, access); !slices.Equal(got, []string{a.ID, c.ID}) {
		t.Fatalf("tabs %v, want a and c (%s, %s)", got, a.ID, c.ID)
	}
	if got := activeTab(m, access); got != a.ID {
		t.Fatalf("active %s, want a (%s)", got, a.ID)
	}
	if got := m.CompanionState(access).ClosedTabs; got != 1 {
		t.Fatalf("%d tabs can be reopened, want b's close", got)
	}
}

// Two presses of Mod+W arriving reversed close two tabs: the second's turn
// sees the first's close and closes the tab that close left active, b, the
// most recently used, not the first in the strip.
func TestKeyboardClosesInARowEachCloseOneTab(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	for seq, id := range []string{ids[1], ids[3]} {
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, uint64(seq+1)), access, id); err != nil {
			t.Fatal(err)
		}
	}
	second := deliver(func() error {
		return m.CloseActiveCompanionPage(numbered(context.Background(), mount, 4), access)
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.CloseActiveCompanionPage(numbered(t.Context(), mount, 3), access); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, second, "the second close")
	if got, want := tabIDs(m, access), []string{ids[0], ids[2]}; !slices.Equal(got, want) {
		t.Fatalf("tabs %v after closing d then b, want a and c %v", got, want)
	}
}

// The keyboard close's commit leaves the tab set and the active tab as the
// next turn must see them before the close is published: the closed tab is
// gone and the active tab is the one the repair picks, b, the most recently
// used.
func TestAKeyboardClosesCommitPicksTheNextActiveTab(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	for seq, id := range []string{ids[1], ids[3]} {
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, uint64(seq+1)), access, id); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	finish := m.closeActiveLocked(access, &selectionTurn{})
	active := m.sessionLocked(access.ThreadID).ActivePageID
	var left []string
	for _, tab := range m.companionTabsLocked(access.ThreadID) {
		left = append(left, tab.id)
	}
	m.mu.Unlock()
	if err := finish(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(left, ids[3]) || active != ids[1] {
		t.Fatalf("after the commit: tabs %v, active %s; want d (%s) gone and b (%s) active", left, active, ids[3], ids[1])
	}
}

// A tab still being created closes at the keyboard's close at once: its
// opening's turn ended when the tab was added.
func TestAKeyboardCloseDoesNotWaitForATabsCreation(t *testing.T) {
	m, engine, access, _, _, mount := newSuspendedPair(t)
	release := engine.hold(&engine.newPageGate)
	defer release()
	opening := deliver(func() error {
		_, err := m.NewCompanionPage(numbered(context.Background(), mount, 1), access, addr("n"))
		return err
	})
	receive(t, engine.creating, "the new tab's creation")
	created := activeTab(m, access)
	if tab, _ := findTab(m, access, created); !tab.Creating {
		t.Fatalf("the active tab is not the one being created: %+v", tab)
	}
	mustDeliver(t, deliver(func() error {
		return m.CloseActiveCompanionPage(numbered(context.Background(), mount, 2), access)
	}), "the keyboard close")
	if _, open := findTab(m, access, created); open {
		t.Fatalf("the creating tab is still open: %v", tabIDs(m, access))
	}
	release()
	mustDeliver(t, opening, "the new tab")
	if _, open := findTab(m, access, created); open {
		t.Fatalf("the closed tab came back: %v", tabIDs(m, access))
	}
}

// A passed keyboard close, a repeat or one from a mount that is gone,
// closes nothing, and one on a thread with no tab is no error.
func TestAPassedKeyboardCloseClosesNothing(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	if err := m.CloseActiveCompanionPage(numbered(t.Context(), mount, 1), access); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{numbered(t.Context(), mount, 1), numbered(t.Context(), "gone", 1)} {
		if err := m.CloseActiveCompanionPage(ctx, access); err != nil {
			t.Fatal(err)
		}
	}
	if got := tabIDs(m, access); len(got) != len(ids)-1 {
		t.Fatalf("tabs %v, want one closed of %v", got, ids)
	}
	empty := Access{ThreadID: "empty", Workspace: t.TempDir()}
	if err := m.CloseActiveCompanionPage(t.Context(), empty); err != nil {
		t.Fatalf("a close with no tab answered %v", err)
	}
}

// The keyboard's reload names no tab: the user steps from d to a and
// presses Mod+R, the reload arrives first, and a is reloaded, not d.
func TestAKeyboardReloadReloadsTheTabItsTurnLeavesActive(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	loads := func(id string) uint64 { return livePage(t, m, access, id).docEpoch.Load() }
	beforeA, beforeD := loads(ids[0]), loads(ids[3])
	reloading := deliver(func() error {
		return m.ReloadActiveCompanionPage(numbered(context.Background(), mount, 2), access)
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.StepCompanionPage(numbered(t.Context(), mount, 1), access, 1); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, reloading, "the keyboard reload")
	if got := activeTab(m, access); got != ids[0] {
		t.Fatalf("active %s, want a (%s)", got, ids[0])
	}
	if a, d := loads(ids[0])-beforeA, loads(ids[3])-beforeD; a != 1 || d != 0 {
		t.Fatalf("a loaded %d times and d %d, want a reloaded once and d not", a, d)
	}
	// A passed one reloads nothing.
	for _, ctx := range []context.Context{numbered(t.Context(), mount, 2), numbered(t.Context(), "gone", 1)} {
		if err := m.ReloadActiveCompanionPage(ctx, access); err != nil {
			t.Fatal(err)
		}
	}
	if a := loads(ids[0]) - beforeA; a != 1 {
		t.Fatalf("a passed reload loaded a again: %d loads", a)
	}
}

// A keyboard reload whose tab closes while it runs (Mod+R, then Mod+W)
// answers success and reloads no other tab.
func TestAKeyboardReloadOfATabClosedMeanwhileDoesNothing(t *testing.T) {
	m, engine, access, a, b, mount := newSuspendedPair(t)
	m.setActivePage(access.ThreadID, a.ID)
	release := engine.hold(&engine.navGate)
	defer release()
	reloading := deliver(func() error {
		return m.ReloadActiveCompanionPage(numbered(context.Background(), mount, 1), access)
	})
	receive(t, engine.navigating, "a's restore")
	if err := m.CloseCompanionPage(t.Context(), access, a.ID); err != nil {
		t.Fatal(err)
	}
	release()
	if err := await(t, reloading, "the keyboard reload"); err != nil {
		t.Fatalf("the reload of a closed tab answered %v", err)
	}
	if tab, _ := findTab(m, access, b.ID); !tab.Suspended || tab.Restoring {
		t.Fatalf("the reload moved to b: %+v", tab)
	}
	if got := tabIDs(m, access); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("tabs %v, want b alone", got)
	}
}
