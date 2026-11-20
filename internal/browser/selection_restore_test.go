package browser

import (
	"context"
	"slices"
	"testing"
	"time"
)

// The next selection waits for an admitted one's selection, never for its
// restore: while a's restore is held, the user's next click selects c, and
// the restore then neither takes the selection back nor shows a.
func TestASelectionDoesNotWaitForARestore(t *testing.T) {
	m, engine, access, a, _, mount := newSuspendedPair(t)
	c := mustOpen(t, m, access, addr("c"))
	release := engine.hold(&engine.navGate)
	defer release()
	restoring := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 1), access, PageID(a.ID))
	})
	receive(t, engine.navigating, "a's restore")
	if err := await(t, deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 2), access, PageID(c.ID))
	}), "the click on c"); err != nil {
		t.Fatal(err)
	}
	engine.take()
	release()
	mustDeliver(t, restoring, "a's restore")
	if got := activeTab(m, access); got != c.ID {
		t.Fatalf("active %s after a's restore, want c (%s)", got, c.ID)
	}
	shownA := "show:" + paneHandle(m, access, a.ID)
	if calls := engine.take(); slices.Contains(calls, shownA) {
		t.Fatalf("a was shown after it lost the selection: %v", calls)
	}
}

// The next selection does not wait for a new tab's load: the opening's turn
// ends with the hold that adds and selects the tab.
func TestASelectionDoesNotWaitForANewTabsLoad(t *testing.T) {
	m, engine, access, a, _, mount := newSuspendedPair(t)
	release := engine.hold(&engine.navGate)
	defer release()
	opening := deliver(func() error {
		_, err := m.NewCompanionPage(numbered(context.Background(), mount, 1), access, addr("n"))
		return err
	})
	receive(t, engine.navigating, "the new tab's load")
	clicked := deliver(func() error {
		return m.StepCompanionPage(numbered(context.Background(), mount, 2), access, 1)
	})
	// The step selects at once; its own restore of a is held at the same
	// gate.
	deadline := time.Now().Add(10 * time.Second)
	for activeTab(m, access) != a.ID {
		if time.Now().After(deadline) {
			t.Fatal("the step waited for the new tab's load")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	mustDeliver(t, opening, "the new tab")
	mustDeliver(t, clicked, "the step")
}

// A passed activation of a suspended tab restores nothing.
func TestAPassedActivationRestoresNothing(t *testing.T) {
	m, _, access, a, b, mount := newSuspendedPair(t)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(b.ID)); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{numbered(t.Context(), mount, 1), numbered(t.Context(), "gone", 1)} {
		if err := m.ActivateCompanionPage(ctx, access, PageID(a.ID)); err != nil {
			t.Fatal(err)
		}
		if tab, _ := findTab(m, access, a.ID); !tab.Suspended || tab.Restoring {
			t.Fatalf("a passed activation restored a: %+v", tab)
		}
	}
	if got := activeTab(m, access); got != b.ID {
		t.Fatalf("active %s, want b", got)
	}
}

// The pane's restore of the tab it shows selects nothing: it restores the
// active tab, and a tab the user moved away from meanwhile stays as it is
// and unselected.
func TestThePanesRestoreSelectsNothing(t *testing.T) {
	m, _, access, a, b, _ := newSuspendedPair(t)
	active, other := a.ID, b.ID
	if activeTab(m, access) == b.ID {
		active, other = b.ID, a.ID
	}
	if err := m.RestoreActiveCompanionPage(t.Context(), access, PageID(other)); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(m, access, other); !tab.Suspended {
		t.Fatalf("a tab that is not active was restored: %+v", tab)
	}
	if got := activeTab(m, access); got != active {
		t.Fatalf("the restore selected %s, want %s still active", got, active)
	}
	if err := m.RestoreActiveCompanionPage(t.Context(), access, PageID(active)); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(m, access, active); tab.Suspended || tab.Restoring {
		t.Fatalf("the active tab was not restored: %+v", tab)
	}
}
