package browser

import (
	"context"
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/browser/turnorder"
)

// A close names the tab the user was looking at, so a close that arrives
// after the user picked another tab closes the tab it names: Mod+W pressed
// in d's page reaches the window after the user clicked b, and b, active
// when the close's turn comes, survives.
func TestADelayedCloseClosesTheTabItNames(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	closing := deliver(func() error {
		return m.CloseCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[3]))
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, closing, "the close of d")
	if got, want := tabIDs(m, access), ids[:3]; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want d closed and b kept %v", got, want)
	}
	if got := activeTab(m, access); got != ids[1] {
		t.Fatalf("active %s, want b (%s)", got, ids[1])
	}
}

// Mod+W on a and then a's x, the x arriving first: the first close at its
// turn closes a and the second finds it gone and closes nothing, so one tab
// closes, not a and the tab its close left active.
func TestTwoClosesOfOneTabCloseOneTab(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	setActiveTab(m, access, ids[0])
	byX := deliver(func() error {
		return m.CloseCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[0]))
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, byX, "the close by the x")
	if got, want := tabIDs(m, access), ids[1:]; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want only a closed %v", got, want)
	}
	if got := m.CompanionState(access.Caller()).ClosedTabs; got != 1 {
		t.Fatalf("%d tabs can be reopened, want a's one close", got)
	}
}

// Two closes arriving reversed close in the user's order, so the closed-tab
// history holds b as the newest and reopen brings b back first.
func TestClosesArrivingReversedKeepTheUsersOrder(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	second := deliver(func() error {
		return m.CloseCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[1]))
	})
	awaitWaiting(t, m, mount, 1)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, second, "the close of b")
	if err := reopenTab(t, m, access); err != nil {
		t.Fatal(err)
	}
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("b"), addr("c"), addr("d")}) {
		t.Fatalf("tabs %v after one reopen, want b back first", got)
	}
}

// A close's turn takes the tab out and picks the next active tab in one
// hold: a step after the close of d, arriving first, steps from b, the
// most recently used tab the close left active, to c.
func TestACloseLeavesTheNextActiveTabForTheNextTurn(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	for seq, id := range []string{ids[1], ids[3]} {
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, uint64(seq+1)), access, PageID(id)); err != nil {
			t.Fatal(err)
		}
	}
	stepping := deliver(func() error { return m.StepCompanionPage(numbered(context.Background(), mount, 4), access, 1) })
	awaitWaiting(t, m, mount, 1)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 3), access, PageID(ids[3])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, stepping, "the step")
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("active %s after the close of d and a step, want c (%s)", got, ids[2])
	}
}

// A close after a number that never arrives acts once the count moves past
// it, on the tab it names; the missing number arriving late is refused.
func TestACloseAfterAMissingNumberActsOnItsTab(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	closing := deliver(func() error {
		return m.CloseCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[2]))
	})
	clock.awaitTimers(t, 1)
	clock.expire(t)
	mustDeliver(t, closing, "the close after a missing number")
	if _, open := findTab(m, access, ids[2]); open {
		t.Fatalf("c is still open: %v", tabIDs(m, access))
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[1])); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("the late selection answered %v", err)
	}
	if got := activeTab(m, access); got != ids[3] {
		t.Fatalf("the late selection applied: active %s, want d", got)
	}
}

// A close of an epoch the window has left, or arriving after the count
// moved past its number, is refused and closes nothing.
func TestALateOrOlderEpochsCloseIsRefused(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	if err := m.CloseCompanionPage(WithSelection(t.Context(), placeIn(mount, 2, 1)), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(WithSelection(t.Context(), placeIn(mount, 1, 1)), access, PageID(ids[1])); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("an older epoch's close answered %v", err)
	}
	selecting := deliver(func() error {
		return m.ActivateCompanionPage(WithSelection(context.Background(), placeIn(mount, 2, 3)), access, PageID(ids[1]))
	})
	clock.awaitTimers(t, 1)
	clock.expire(t)
	mustDeliver(t, selecting, "the selection of b")
	if err := m.CloseCompanionPage(WithSelection(t.Context(), placeIn(mount, 2, 2)), access, PageID(ids[2])); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("the late close answered %v", err)
	}
	if got, want := tabIDs(m, access), ids[1:]; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want only a closed %v", got, want)
	}
}

// A passed close (a repeat of its number, or from a mount that is gone or
// was detached) closes nothing; the new mount's first close acts.
func TestAPassedCloseClosesNothing(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[3])); err != nil {
		t.Fatal(err)
	}
	for _, at := range []Numbered{place(mount, 1), place("gone", 1)} {
		if err := m.CloseCompanionPage(WithSelection(t.Context(), at), access, PageID(ids[2])); err != nil {
			t.Fatal(err)
		}
	}
	m.DetachPane(MountID(mount))
	next, err := m.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 2), access, PageID(ids[2])); err != nil {
		t.Fatal(err)
	}
	if got, want := tabIDs(m, access), ids[:3]; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want only d closed %v", got, want)
	}
	if err := m.CloseCompanionPage(numbered(t.Context(), next.ID, 1), access, PageID(ids[2])); err != nil {
		t.Fatal(err)
	}
	if got, want := tabIDs(m, access), ids[:2]; !slices.Equal(got, want) {
		t.Fatalf("tabs %v, want c closed by the new mount %v", got, want)
	}
}

// Close others and close to the right take their turn as one close: they
// close the tabs they find at it, in one hold.
func TestCloseOthersTakesItsTurn(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	others := deliver(func() error {
		return m.CloseCompanionTabs(numbered(context.Background(), mount, 2), access, PageID(ids[1]), CloseOtherTabs)
	})
	awaitWaiting(t, m, mount, 1)
	if _, err := m.NewCompanionPage(numbered(t.Context(), mount, 1), access, addr("e")); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, others, "close others")
	if got := tabIDs(m, access); !slices.Equal(got, []string{ids[1]}) {
		t.Fatalf("tabs %v, want b alone: the tab opened before close others closed too", got)
	}
}

// A tab still being created closes at a close naming it at once: its
// opening's turn ended when the tab was added.
func TestACloseDoesNotWaitForATabsCreation(t *testing.T) {
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
		return m.CloseCompanionPage(numbered(context.Background(), mount, 2), access, PageID(created))
	}), "the close")
	if _, open := findTab(m, access, created); open {
		t.Fatalf("the creating tab is still open: %v", tabIDs(m, access))
	}
	release()
	mustDeliver(t, opening, "the new tab")
	if _, open := findTab(m, access, created); open {
		t.Fatalf("the closed tab came back: %v", tabIDs(m, access))
	}
}

// A reload whose tab closes while it runs (Mod+R, then Mod+W) reloads
// nothing and answers no failure, and no other tab is touched.
func TestAReloadOfATabClosedMeanwhileDoesNothing(t *testing.T) {
	m, engine, access, a, b, mount := newSuspendedPair(t)
	setActiveTab(m, access, a.ID)
	release := engine.hold(&engine.navGate)
	defer release()
	reloading := deliver(func() error { return m.ReloadCompanionPage(context.Background(), access, PageID(a.ID)) })
	receive(t, engine.navigating, "a's restore")
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(a.ID)); err != nil {
		t.Fatal(err)
	}
	release()
	mustDeliver(t, reloading, "the reload")
	if tab, _ := findTab(m, access, b.ID); !tab.Suspended || tab.Restoring {
		t.Fatalf("the reload moved to b: %+v", tab)
	}
	if got := tabIDs(m, access); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("tabs %v, want b alone", got)
	}
}

// mustAttachEmpty attaches a pane to a thread and closes its only tab, so
// the mount stays with no tab.
func mustAttachEmpty(t *testing.T, m *Manager, access Access) string {
	t.Helper()
	page := mustOpen(t, m, access, addr("e"))
	mount, err := m.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	return mount.ID
}
