package browser

import (
	"context"
	"testing"
)

// newSelectionManager opens tabs a, b, c and d for the user, attaches a pane
// mount, and answers the tab ids and the mount. The last tab opened, d, is
// active.
func newSelectionManager(t *testing.T) (*Manager, Access, []string, string) {
	t.Helper()
	m, access, ids := newTabsManager(t, addr("a"), addr("b"), addr("c"), addr("d"))
	mount, err := m.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	return m, access, ids, mount.ID
}

// numbered is ctx carrying the user's selection seq made in mount.
func numbered(ctx context.Context, mount string, seq uint64) context.Context {
	return WithSelection(ctx, Selection{Mount: mount, Seq: seq})
}

func activeTab(m *Manager, access Access) string {
	return m.CompanionState(access).ActivePageID
}

// Two selections the user made in a row end on the second, whichever
// reaches the Manager first; the overtaken one answers success.
func TestTheUsersLastSelectionWinsInEitherDeliveryOrder(t *testing.T) {
	t.Run("activate, activate", func(t *testing.T) {
		m, access, ids, mount := newSelectionManager(t)
		// The user clicked a (1) and then c (2); c arrives first.
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, ids[2]); err != nil {
			t.Fatal(err)
		}
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
			t.Fatalf("the overtaken activation answered %v", err)
		}
		if got := activeTab(m, access); got != ids[2] {
			t.Fatalf("active %s, want c (%s)", got, ids[2])
		}
	})
	t.Run("activate, step", func(t *testing.T) {
		for _, stepFirst := range []bool{false, true} {
			m, access, ids, mount := newSelectionManager(t)
			// The user clicked b (1) and then pressed next tab (2).
			activate := func() {
				if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[1]); err != nil {
					t.Fatal(err)
				}
			}
			step := func() {
				if err := m.StepCompanionPage(numbered(t.Context(), mount, 2), access, 1); err != nil {
					t.Fatal(err)
				}
			}
			if stepFirst {
				step()
				activate()
			} else {
				activate()
				step()
			}
			if got := activeTab(m, access); got != ids[2] {
				t.Fatalf("step first %v: active %s, want c (%s), the tab after the clicked one", stepFirst, got, ids[2])
			}
		}
	})
	t.Run("step, activate", func(t *testing.T) {
		m, access, ids, mount := newSelectionManager(t)
		// The user pressed next tab (1) and then clicked b (2); b arrives
		// first, and the older step moves nothing.
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, ids[1]); err != nil {
			t.Fatal(err)
		}
		if err := m.StepCompanionPage(numbered(t.Context(), mount, 1), access, 1); err != nil {
			t.Fatal(err)
		}
		if got := activeTab(m, access); got != ids[1] {
			t.Fatalf("active %s, want b (%s)", got, ids[1])
		}
	})
	t.Run("step, step", func(t *testing.T) {
		m, access, ids, mount := newSelectionManager(t)
		// Two next-tab presses from d move two tabs, to b, in either order.
		for _, seq := range []uint64{2, 1} {
			if err := m.StepCompanionPage(numbered(t.Context(), mount, seq), access, 1); err != nil {
				t.Fatal(err)
			}
		}
		if got := activeTab(m, access); got != ids[1] {
			t.Fatalf("active %s, want b (%s), two tabs on from d", got, ids[1])
		}
	})
	t.Run("new tab, activate", func(t *testing.T) {
		m, access, ids, mount := newSelectionManager(t)
		// The user opened a tab (1) and then clicked a (2); the click arrives
		// first, and the new tab opens behind it.
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, ids[0]); err != nil {
			t.Fatal(err)
		}
		page, err := m.NewCompanionPage(numbered(t.Context(), mount, 1), access, addr("e"))
		if err != nil || page.ID == "" {
			t.Fatalf("new tab: %+v, %v", page, err)
		}
		if got := activeTab(m, access); got != ids[0] {
			t.Fatalf("active %s, want a (%s)", got, ids[0])
		}
	})
}

// A selection the Manager cannot place in a mount's order applies in arrival
// order: an unnumbered one (an older client), one with no mount, one from an
// unknown mount or a mount of another thread, and an agent's.
func TestUnorderedSelectionsApplyInArrivalOrder(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	other := Access{ThreadID: "other thread", Workspace: t.TempDir()}
	x := mustOpen(t, m, other, addr("x"))
	otherMount, err := m.AttachPane(other)
	if err != nil {
		t.Fatal(err)
	}
	// The other thread's mount has an order of its own, which a selection
	// on this thread naming it must not be held to.
	if err := m.ActivateCompanionPage(numbered(t.Context(), otherMount.ID, 100), other, x.ID); err != nil {
		t.Fatal(err)
	}
	seq := uint64(5)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, seq), access, ids[2]); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"unnumbered", t.Context()},
		{"number zero", numbered(t.Context(), mount, 0)},
		{"no mount", numbered(t.Context(), "", 1)},
		{"unknown mount", numbered(t.Context(), "gone", 1)},
		{"another thread's mount", numbered(t.Context(), otherMount.ID, 1)},
	} {
		if err := m.ActivateCompanionPage(tc.ctx, access, ids[0]); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := activeTab(m, access); got != ids[0] {
			t.Fatalf("%s: active %s, want a (%s)", tc.name, got, ids[0])
		}
		seq++
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, seq), access, ids[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.SelectPage(t.Context(), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got := activeTab(m, access); got != ids[1] {
		t.Fatalf("an agent's selection was not applied: active %s", got)
	}
}

// A mount's order dies with it: a new mount's first selection applies
// whatever number the old one reached.
func TestANewMountStartsItsOwnOrder(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 9), access, ids[2]); err != nil {
		t.Fatal(err)
	}
	m.DetachPane(mount)
	next, err := m.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), next.ID, 1), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := activeTab(m, access); got != ids[0] {
		t.Fatalf("active %s, want a (%s)", got, ids[0])
	}
}

// newSuspendedPair opens tabs a and b, suspends both, and attaches a pane
// mount.
func newSuspendedPair(t *testing.T) (*Manager, *lifecycleEngine, Access, PageInfo, PageInfo, string) {
	t.Helper()
	m, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	a := mustOpen(t, m, access, addr("a"))
	b := mustOpen(t, m, access, addr("b"))
	mount, err := m.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if tab, _ := findTab(m, access, id); !tab.Suspended {
			t.Fatalf("tab %s was not suspended: %+v", id, tab)
		}
	}
	return m, engine, access, a, b, mount.ID
}

// An overtaken activation of a suspended tab starts no restore: the tab
// stays suspended and nothing of it is presented.
func TestAnOvertakenActivationRestoresNothing(t *testing.T) {
	m, _, access, a, b, mount := newSuspendedPair(t)
	// The user clicked a (1) and then b (2); b arrives first.
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, b.ID); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(m, access, b.ID); tab.Suspended || tab.Restoring {
		t.Fatalf("the applied activation did not restore b: %+v", tab)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, a.ID); err != nil {
		t.Fatalf("the overtaken activation answered %v", err)
	}
	if tab, _ := findTab(m, access, a.ID); !tab.Suspended || tab.Restoring {
		t.Fatalf("the overtaken activation restored a: %+v", tab)
	}
	if got := activeTab(m, access); got != b.ID {
		t.Fatalf("active %s, want b (%s)", got, b.ID)
	}
}

// A newer selection made while an older one's restore is in flight stays the
// selection: the restore finishes its tab without taking the selection back
// or showing it, and a late duplicate of the older activation starts nothing.
func TestASelectionDuringARestoreStaysSelected(t *testing.T) {
	m, engine, access, a, _, mount := newSuspendedPair(t)
	// c is live: selecting it needs no restore.
	c := mustOpen(t, m, access, addr("c"))
	release := engine.hold(&engine.navGate)
	defer release()
	restoring := async(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 1), access, a.ID)
	})
	receive(t, engine.navigating, "a's restore")
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, c.ID); err != nil {
		t.Fatal(err)
	}
	duplicate := async(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 1), access, a.ID)
	})
	if err := await(t, duplicate, "the late duplicate"); err != nil {
		t.Fatal(err)
	}
	engine.take()
	release()
	if err := await(t, restoring, "a's restore"); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(m, access, a.ID); tab.Suspended || tab.Restoring {
		t.Fatalf("a's restore did not finish: %+v", tab)
	}
	if got := activeTab(m, access); got != c.ID {
		t.Fatalf("active %s after a's restore, want c (%s)", got, c.ID)
	}
	shownA := "show:" + paneHandle(m, access, a.ID)
	for _, call := range engine.take() {
		if call == shownA {
			t.Fatalf("a was shown after it lost the selection: %v", call)
		}
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
	if err := m.RestoreActiveCompanionPage(t.Context(), access, other); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(m, access, other); !tab.Suspended {
		t.Fatalf("a tab that is not active was restored: %+v", tab)
	}
	if got := activeTab(m, access); got != active {
		t.Fatalf("the restore selected %s, want %s still active", got, active)
	}
	if err := m.RestoreActiveCompanionPage(t.Context(), access, active); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(m, access, active); tab.Suspended || tab.Restoring {
		t.Fatalf("the active tab was not restored: %+v", tab)
	}
}
