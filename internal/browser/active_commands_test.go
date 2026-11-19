package browser

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/browser/turnorder"
)

// outcome is nil when a keyboard command answered want and no error.
func outcome(want ActiveOutcome) func(ActiveOutcome, error) error {
	return func(got ActiveOutcome, err error) error {
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("outcome %d, want %d", got, want)
		}
		return nil
	}
}

func mustOutcome(t *testing.T, want ActiveOutcome, got ActiveOutcome, err error) {
	t.Helper()
	if err := outcome(want)(got, err); err != nil {
		t.Fatal(err)
	}
}

// The keyboard's close names no tab and takes its turn in the user's order:
// the user selects suspended b, presses Mod+W, then selects live c. The
// selection of c arrives first, then b's, whose restore is held, then the
// close. While b's restore is still held, b is gone, c survives and is
// active, and a, the tab active before, is untouched.
func TestAKeyboardCloseClosesTheTabItsTurnLeavesActive(t *testing.T) {
	m, engine, access, a, b, mount := newSuspendedPair(t)
	c := mustOpen(t, m, access, addr("c"))
	m.setActivePage(access.ThreadID, a.ID)
	for len(engine.navigating) > 0 {
		<-engine.navigating
	}
	release := engine.hold(&engine.navGate)
	defer release()
	selectC := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, c.ID)
	})
	awaitWaiting(t, m, mount, 1)
	selectB := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 1), access, b.ID)
	})
	receive(t, engine.navigating, "b's restore")
	closing := deliver(func() error {
		return outcome(ActiveApplied)(m.CloseActiveCompanionPage(context.Background(), access, place(mount, 2)))
	})
	mustDeliver(t, closing, "the keyboard close")
	mustDeliver(t, selectC, "the selection of c")
	if got := tabIDs(m, access); !slices.Equal(got, []string{a.ID, c.ID}) {
		t.Fatalf("tabs %v with b's restore held, want a and c (%s, %s)", got, a.ID, c.ID)
	}
	if got := activeTab(m, access); got != c.ID {
		t.Fatalf("active %s with b's restore held, want c (%s)", got, c.ID)
	}
	release()
	mustDeliver(t, selectB, "the selection of b")
	if got := tabIDs(m, access); !slices.Equal(got, []string{a.ID, c.ID}) || activeTab(m, access) != c.ID {
		t.Fatalf("after b's restore: tabs %v, active %s; want a and c, c active", got, activeTab(m, access))
	}
	if tab, _ := findTab(m, access, a.ID); !tab.Suspended || tab.Restoring {
		t.Fatalf("a was touched: %+v", tab)
	}
	if got := m.CompanionState(access).ClosedTabs; got != 1 {
		t.Fatalf("%d tabs can be reopened, want b's close", got)
	}
}

// A keyboard close whose predecessor never arrived stands out of order when
// the count moves past that number, and closes nothing; the predecessor
// still applies when it arrives late, and the next close acts in order.
func TestAKeyboardCloseAfterAMissingNumberIsRefused(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	closing := deliver(func() error {
		return outcome(ActiveOutOfOrder)(m.CloseActiveCompanionPage(context.Background(), access, place(mount, 2)))
	})
	clock.awaitTimers(t, 1)
	clock.expire(t)
	mustDeliver(t, closing, "the close after a missing number")
	if got := tabIDs(m, access); len(got) != len(ids) {
		t.Fatalf("the refused close closed a tab: %v", got)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got := activeTab(m, access); got != ids[1] {
		t.Fatalf("the late selection did not apply: active %s, want b", got)
	}
	got, err := m.CloseActiveCompanionPage(t.Context(), access, place(mount, 3))
	mustOutcome(t, ActiveApplied, got, err)
	if _, open := findTab(m, access, ids[1]); open {
		t.Fatalf("the next close did not close b: %v", tabIDs(m, access))
	}
}

// A keyboard close or reload arriving after the count moved past its own
// number stands out of order and acts on nothing.
func TestALateKeyboardCommandIsRefused(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	loads := livePage(t, m, access, ids[0]).docEpoch.Load()
	selecting := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, ids[0])
	})
	clock.awaitTimers(t, 1)
	clock.expire(t)
	mustDeliver(t, selecting, "the selection of a")
	got, err := m.CloseActiveCompanionPage(t.Context(), access, place(mount, 1))
	mustOutcome(t, ActiveOutOfOrder, got, err)
	got, err = m.ReloadActiveCompanionPage(t.Context(), access, place(mount, 2))
	mustOutcome(t, ActiveOutOfOrder, got, err)
	if got := tabIDs(m, access); len(got) != len(ids) {
		t.Fatalf("a refused close closed a tab: %v", got)
	}
	if again := livePage(t, m, access, ids[0]).docEpoch.Load(); again != loads {
		t.Fatalf("a refused reload loaded a: %d loads, was %d", again, loads)
	}
}

// A pane detach and reattach between commands: the old mount's close is
// passed and closes nothing, and the new mount's first close closes the
// active tab.
func TestAKeyboardCloseAcrossAReattach(t *testing.T) {
	m, access, ids, oldMount := newSelectionManager(t)
	m.DetachPane(oldMount)
	next, err := m.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.CloseActiveCompanionPage(t.Context(), access, place(oldMount, 1))
	mustOutcome(t, ActivePassed, got, err)
	if got := tabIDs(m, access); len(got) != len(ids) {
		t.Fatalf("the detached mount's close closed a tab: %v", got)
	}
	got, err = m.CloseActiveCompanionPage(t.Context(), access, place(next.ID, 1))
	mustOutcome(t, ActiveApplied, got, err)
	if _, open := findTab(m, access, ids[3]); open {
		t.Fatalf("the new mount's close did not close d: %v", tabIDs(m, access))
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
		return outcome(ActiveApplied)(m.CloseActiveCompanionPage(context.Background(), access, place(mount, 4)))
	})
	awaitWaiting(t, m, mount, 1)
	got, err := m.CloseActiveCompanionPage(t.Context(), access, place(mount, 3))
	mustOutcome(t, ActiveApplied, got, err)
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
	got, finish := m.closeActiveLocked(access, turnorder.Unordered())
	active := m.sessionLocked(access.ThreadID).ActivePageID
	var left []string
	for _, tab := range m.companionTabsLocked(access.ThreadID) {
		left = append(left, tab.id)
	}
	m.mu.Unlock()
	if got != ActiveApplied {
		t.Fatalf("outcome %d, want applied", got)
	}
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
		return outcome(ActiveApplied)(m.CloseActiveCompanionPage(context.Background(), access, place(mount, 2)))
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
// closes nothing, and one on a thread with no tab finds none.
func TestAPassedKeyboardCloseClosesNothing(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	got, err := m.CloseActiveCompanionPage(t.Context(), access, place(mount, 1))
	mustOutcome(t, ActiveApplied, got, err)
	for _, at := range []Numbered{place(mount, 1), place("gone", 1)} {
		got, err := m.CloseActiveCompanionPage(t.Context(), access, at)
		mustOutcome(t, ActivePassed, got, err)
	}
	if got := tabIDs(m, access); len(got) != len(ids)-1 {
		t.Fatalf("tabs %v, want one closed of %v", got, ids)
	}
	empty := Access{ThreadID: "empty", Workspace: t.TempDir()}
	emptyMount := mustAttachEmpty(t, m, empty)
	got, err = m.CloseActiveCompanionPage(t.Context(), empty, place(emptyMount, 1))
	mustOutcome(t, ActiveNoTab, got, err)
}

// mustAttachEmpty attaches a pane to a thread and closes its only tab, so
// the mount stays with no tab.
func mustAttachEmpty(t *testing.T, m *Manager, access Access) string {
	t.Helper()
	page := mustOpen(t, m, access, addr("e"))
	mount, err := m.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	return mount.ID
}

// The keyboard's reload names no tab: the user steps from d to a and
// presses Mod+R, the reload arrives first, and a is reloaded, not d.
func TestAKeyboardReloadReloadsTheTabItsTurnLeavesActive(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	loads := func(id string) uint64 { return livePage(t, m, access, id).docEpoch.Load() }
	beforeA, beforeD := loads(ids[0]), loads(ids[3])
	reloading := deliver(func() error {
		return outcome(ActiveApplied)(m.ReloadActiveCompanionPage(context.Background(), access, place(mount, 2)))
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
	for _, at := range []Numbered{place(mount, 2), place("gone", 1)} {
		got, err := m.ReloadActiveCompanionPage(t.Context(), access, at)
		mustOutcome(t, ActivePassed, got, err)
	}
	if a := loads(ids[0]) - beforeA; a != 1 {
		t.Fatalf("a passed reload loaded a again: %d loads", a)
	}
}

// A keyboard reload whose tab closes while it runs (Mod+R, then Mod+W)
// finds no tab and reloads no other one.
func TestAKeyboardReloadOfATabClosedMeanwhileDoesNothing(t *testing.T) {
	m, engine, access, a, b, mount := newSuspendedPair(t)
	m.setActivePage(access.ThreadID, a.ID)
	release := engine.hold(&engine.navGate)
	defer release()
	reloading := deliver(func() error {
		return outcome(ActiveNoTab)(m.ReloadActiveCompanionPage(context.Background(), access, place(mount, 1)))
	})
	receive(t, engine.navigating, "a's restore")
	if err := m.CloseCompanionPage(t.Context(), access, a.ID); err != nil {
		t.Fatal(err)
	}
	release()
	mustDeliver(t, reloading, "the keyboard reload")
	if tab, _ := findTab(m, access, b.ID); !tab.Suspended || tab.Restoring {
		t.Fatalf("the reload moved to b: %+v", tab)
	}
	if got := tabIDs(m, access); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("tabs %v, want b alone", got)
	}
}
