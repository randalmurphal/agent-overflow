package browser

import (
	"context"
	"fmt"
	"testing"
)

// Delayed deliveries, each ending on the user's last selection.
func TestTheUsersOrderSurvivesDelayedDelivery(t *testing.T) {
	t.Run("a detached mount's delayed selection", func(t *testing.T) {
		m, access, ids, oldMount := newSelectionManager(t)
		// The user clicked b in the old pane; the request is held while the
		// pane reconnects as a new mount, where the user clicks c.
		m.DetachPane(MountID(oldMount))
		next, err := m.AttachPane(access.Caller(), untiedPane)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ActivateCompanionPage(numbered(t.Context(), next.ID, 1), access, PageID(ids[2])); err != nil {
			t.Fatal(err)
		}
		if err := m.ActivateCompanionPage(numbered(t.Context(), oldMount, 1), access, PageID(ids[1])); err != nil {
			t.Fatalf("the old pane's selection answered %v", err)
		}
		if got := activeTab(m, access); got != ids[2] {
			t.Fatalf("active %s, want c (%s)", got, ids[2])
		}
	})
	t.Run("a reordered activation restores the tab it selects", func(t *testing.T) {
		m, engine := newLifecycleManager(t)
		installGapClock(m)
		access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
		var ids []string
		for _, name := range []string{"a", "b", "c", "d"} {
			ids = append(ids, mustOpen(t, m, access, addr(name)).ID)
		}
		mount, err := m.AttachPane(access.Caller(), untiedPane)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { assertSelectionsSettled(t, m) })
		if err := m.SuspendThread(t.Context(), access.ThreadID()); err != nil {
			t.Fatal(err)
		}
		setActiveTab(m, access, ids[3])
		engine.take()
		// The user clicked b (1) and then pressed next (2); the step
		// arrives first.
		step := deliver(func() error {
			return m.StepCompanionPage(numbered(context.Background(), mount.ID, 2), access, 1)
		})
		awaitWaiting(t, m, mount.ID, 1)
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount.ID, 1), access, PageID(ids[1])); err != nil {
			t.Fatal(err)
		}
		mustDeliver(t, step, "the step")
		if got := activeTab(m, access); got != ids[2] {
			t.Fatalf("active %s, want c (%s)", got, ids[2])
		}
		for _, id := range ids[1:3] {
			if tab, _ := findTab(m, access, id); tab.Suspended || tab.Restoring {
				t.Fatalf("a selected tab was not restored: %+v", tab)
			}
		}
	})
}

// Every pair of selecting actions ends where the user's order ends when the
// second reaches the Manager first.
func TestEveryPairOfSelectionsDeliveredReversed(t *testing.T) {
	for _, first := range selectionCases {
		for _, second := range selectionCases {
			t.Run(first.name+", "+second.name, func(t *testing.T) {
				inOrder, access, fixture, mount := newPairManager(t)
				if err := first.act(numbered(t.Context(), mount, 1), inOrder, access, fixture); err != nil {
					t.Fatal(err)
				}
				if err := second.act(numbered(t.Context(), mount, 2), inOrder, access, fixture); err != nil {
					t.Fatal(err)
				}
				want := selectionOutcome(inOrder, access)

				reversed, access, fixture, mount := newPairManager(t)
				late := deliver(func() error {
					return second.act(numbered(context.Background(), mount, 2), reversed, access, fixture)
				})
				awaitWaiting(t, reversed, mount, 1)
				if err := first.act(numbered(t.Context(), mount, 1), reversed, access, fixture); err != nil {
					t.Fatal(err)
				}
				mustDeliver(t, late, second.name)
				if got := selectionOutcome(reversed, access); got != want {
					t.Fatalf("reversed: %s\nin order: %s", got, want)
				}
			})
		}
	}
}

// Next-tab presses held behind one delayed activation all move: a plus n
// steps over four tabs is ids[n%4].
func TestManyStepsBehindOneDelayedActivation(t *testing.T) {
	for _, n := range []int{65, 200} {
		t.Run(fmt.Sprintf("%d steps", n), func(t *testing.T) {
			m, access, ids, mount := newSelectionManager(t)
			var steps []<-chan error
			for seq := uint64(2); seq <= uint64(n)+1; seq++ {
				steps = append(steps, deliver(func() error {
					return m.StepCompanionPage(numbered(context.Background(), mount, seq), access, 1)
				}))
			}
			awaitWaiting(t, m, mount, n)
			if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[0])); err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				mustDeliver(t, step, "a step")
			}
			if got, want := activeTab(m, access), ids[n%4]; got != want {
				t.Fatalf("active %s, want %s", got, want)
			}
		})
	}
}

// Two mounts keep their own orders: one waiting does not hold the other.
func TestTwoMountsKeepTheirOwnOrders(t *testing.T) {
	m, access, ids, one := newSelectionManager(t)
	other, err := m.AttachPane(access.Caller(), untiedPane)
	if err != nil {
		t.Fatal(err)
	}
	two := other.ID
	oneSecond := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), one, 2), access, PageID(ids[1]))
	})
	twoSecond := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), two, 2), access, PageID(ids[3]))
	})
	awaitWaiting(t, m, one, 1)
	awaitWaiting(t, m, two, 1)
	if err := m.ActivateCompanionPage(numbered(t.Context(), two, 1), access, PageID(ids[2])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, twoSecond, "the second mount's 2")
	if got := activeTab(m, access); got != ids[3] || waiting(m, one) != 1 {
		t.Fatalf("active %s, want d; %d waiting in the first mount", got, waiting(m, one))
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), one, 1), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, oneSecond, "the first mount's 2")
	if got := activeTab(m, access); got != ids[1] {
		t.Fatalf("active %s, want b", got)
	}
}

// A selection outside a mount's order applies in arrival order and never
// waits for a numbered one waiting its turn.
func TestUnorderedSelectionsDoNotWait(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	second := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 2), access, PageID(ids[3]))
	})
	awaitWaiting(t, m, mount, 1)
	visible := true
	for _, tc := range []struct {
		name string
		page string
		act  func() error
	}{
		{"unnumbered", ids[0], func() error { return m.ActivateCompanionPage(t.Context(), access, PageID(ids[0])) }},
		{"agent", ids[2], func() error { _, err := m.SelectPage(t.Context(), access, PageID(ids[2])); return err }},
		{"header show", ids[0], func() error {
			_, err := m.Visibility(t.Context(), access, &visible, PageID(ids[0]))
			return err
		}},
	} {
		if err := await(t, deliver(tc.act), tc.name); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := activeTab(m, access); got != tc.page {
			t.Fatalf("%s: active %s, want %s", tc.name, got, tc.page)
		}
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, second, "number 2")
	if got := activeTab(m, access); got != ids[3] {
		t.Fatalf("active %s, want d", got)
	}
}

// A selection the Manager will not admit is a quiet success that selects,
// opens and restores nothing: a number from a mount it does not have or of
// another thread, a repeat, and an opening passed by its mount's count.
func TestPassedSelectionsDoNothing(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	other := m.Bind(Caller{ThreadID: "other thread", Workspace: t.TempDir()})
	mustOpen(t, m, other, addr("x"))
	otherMount, err := m.AttachPane(other.Caller(), untiedPane)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[3])); err != nil {
		t.Fatal(err)
	}
	before := selectionOutcome(m, access)
	closed := m.CompanionState(access.Caller()).ClosedTabs
	for name, act := range map[string]func() error{
		"unknown mount": func() error {
			return m.ActivateCompanionPage(numbered(t.Context(), "gone", 1), access, PageID(ids[0]))
		},
		"another thread's mount": func() error {
			return m.ActivateCompanionPage(numbered(t.Context(), otherMount.ID, 1), access, PageID(ids[0]))
		},
		"a repeat": func() error {
			return m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[0]))
		},
		"a passed new tab": func() error {
			_, err := m.NewCompanionPage(numbered(t.Context(), mount, 1), access, addr("n"))
			return err
		},
		"a passed reopen":    func() error { return m.ReopenClosedTab(numbered(t.Context(), mount, 1), access) },
		"a passed duplicate": func() error { return m.DuplicateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[0])) },
		"a passed step":      func() error { return m.StepCompanionPage(numbered(t.Context(), mount, 1), access, 1) },
	} {
		if err := act(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := selectionOutcome(m, access); got != before {
			t.Fatalf("%s changed the tabs: %s, was %s", name, got, before)
		}
		if got := m.CompanionState(access.Caller()).ClosedTabs; got != closed {
			t.Fatalf("%s: %d closed tabs, was %d", name, got, closed)
		}
	}
}

// A repeat of a number already waiting answers at once and selects nothing.
func TestARepeatOfAWaitingNumberDoesNothing(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	third := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, PageID(ids[2]))
	})
	awaitWaiting(t, m, mount, 1)
	if err := await(t, deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, PageID(ids[0]))
	}), "the repeat"); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids[:2] {
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, uint64(i+1)), access, PageID(id)); err != nil {
			t.Fatal(err)
		}
	}
	mustDeliver(t, third, "number 3")
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("active %s, want c", got)
	}
}
