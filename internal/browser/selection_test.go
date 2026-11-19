package browser

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/browser/turnorder"
)

// gapClock replaces the Manager's selection gap timer: a wait for a missing
// predecessor expires only when the test fires it.
type gapClock struct {
	mu      sync.Mutex
	started int
	fire    chan time.Time
}

func installGapClock(m *Manager) *gapClock {
	clock := &gapClock{fire: make(chan time.Time)}
	m.selectionGapTimer = func() (<-chan time.Time, func() bool) {
		clock.mu.Lock()
		clock.started++
		clock.mu.Unlock()
		return clock.fire, func() bool { return true }
	}
	return clock
}

func (c *gapClock) timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// expire ends one waiting selection's gap wait.
func (c *gapClock) expire(t *testing.T) {
	t.Helper()
	select {
	case c.fire <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("no selection is waiting on the gap timer")
	}
}

// awaitTimers waits until n gap waits have started.
func (c *gapClock) awaitTimers(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.timers() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d gap waits started, want %d", c.timers(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// newSelectionManager opens tabs a, b, c and d for the user, attaches a pane
// mount, and answers the tab ids and the mount. d is active. Gap waits
// expire only when the test fires them.
func newSelectionManager(t *testing.T) (*Manager, Access, []string, string) {
	t.Helper()
	m, access, ids := newTabsManager(t, addr("a"), addr("b"), addr("c"), addr("d"))
	installGapClock(m)
	mount, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { assertSelectionsSettled(t, m) })
	return m, access, ids, mount.ID
}

// place is the seq-th numbered command of epoch 1 made in mount.
func place(mount string, seq uint64) Numbered {
	return placeIn(mount, 1, seq)
}

// placeIn is the seq-th numbered command of epoch made in mount.
func placeIn(mount string, epoch, seq uint64) Numbered {
	n, err := NewNumbered(MountID(mount), epoch, seq)
	if err != nil {
		panic(err)
	}
	return n
}

// numbered is ctx carrying the user's selection seq made in mount.
func numbered(ctx context.Context, mount string, seq uint64) context.Context {
	return WithSelection(ctx, place(mount, seq))
}

// orderStats is what mount's order holds; zero when it has none.
func orderStats(m *Manager, mount string) turnorder.Stats {
	m.mu.Lock()
	order := m.panes[mount].order
	m.mu.Unlock()
	if order == nil {
		return turnorder.Stats{}
	}
	return order.Stats()
}

func activeTab(m *Manager, access Access) string {
	return m.CompanionState(access.Caller()).ActivePageID
}

// waiting is the number of selections waiting for their turn in mount.
func waiting(m *Manager, mount string) int {
	return orderStats(m, mount).Waiting
}

func awaitWaiting(t *testing.T, m *Manager, mount string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for waiting(m, mount) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d selections waiting in the mount, want %d", waiting(m, mount), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertSelectionsSettled fails when a mount still holds a waiter or a turn.
func assertSelectionsSettled(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	var mounts []string
	for id := range m.panes {
		mounts = append(mounts, id)
	}
	m.mu.Unlock()
	for _, id := range mounts {
		if stats := orderStats(m, id); stats.Waiting > 0 || stats.Taken {
			t.Errorf("mount %s left %d waiting, turn taken %v", id, stats.Waiting, stats.Taken)
		}
	}
}

// deliver runs one action on its own goroutine, as the transport does.
func deliver(fn func() error) <-chan error {
	return async(fn)
}

func mustDeliver(t *testing.T, done <-chan error, what string) {
	t.Helper()
	if err := await(t, done, what); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// The three sequences the review found, each ending on the user's last
// selection.
func TestTheUsersOrderSurvivesDelayedDelivery(t *testing.T) {
	t.Run("a detached mount's delayed selection", func(t *testing.T) {
		m, access, ids, oldMount := newSelectionManager(t)
		// The user clicked b in the old pane; the request is held while the
		// pane reconnects as a new mount, where the user clicks c.
		m.DetachPane(oldMount)
		next, err := m.AttachPane(access.Caller())
		if err != nil {
			t.Fatal(err)
		}
		if err := m.ActivateCompanionPage(numbered(t.Context(), next.ID, 1), access, ids[2]); err != nil {
			t.Fatal(err)
		}
		if err := m.ActivateCompanionPage(numbered(t.Context(), oldMount, 1), access, ids[1]); err != nil {
			t.Fatalf("the old pane's selection answered %v", err)
		}
		if got := activeTab(m, access); got != ids[2] {
			t.Fatalf("active %s, want c (%s)", got, ids[2])
		}
	})
	t.Run("65 steps behind a delayed activation", func(t *testing.T) {
		m, access, ids, mount := newSelectionManager(t)
		var steps []<-chan error
		for seq := uint64(2); seq <= 66; seq++ {
			steps = append(steps, deliver(func() error {
				return m.StepCompanionPage(numbered(context.Background(), mount, seq), access, 1)
			}))
		}
		awaitWaiting(t, m, mount, 65)
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			mustDeliver(t, step, "a step")
		}
		// Four tabs: a plus 65 steps is b.
		if got, want := activeTab(m, access), ids[65%4]; got != want {
			t.Fatalf("active %s, want %s", got, want)
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
		mount, err := m.AttachPane(access.Caller())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { assertSelectionsSettled(t, m) })
		if err := m.SuspendThread(t.Context(), access.ThreadID); err != nil {
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
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount.ID, 1), access, ids[1]); err != nil {
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

// selectionCase is one user action against the tabs of newPairManager.
type selectionCase struct {
	name string
	act  func(ctx context.Context, m *Manager, access Access, fixture pairFixture) error
}

type pairFixture struct {
	ids      []string
	download string
}

// newPairManager opens a, b, c, d and e for the user, closes e, downloads
// an image in an agent's tab after them, selects b, and attaches a pane.
func newPairManager(t *testing.T) (*Manager, Access, pairFixture, string) {
	t.Helper()
	m, access, ids := newTabsManager(t, addr("a"), addr("b"), addr("c"), addr("d"), addr("e"))
	installGapClock(m)
	t.Cleanup(func() { assertSelectionsSettled(t, m) })
	if err := m.CloseCompanionPage(t.Context(), access, ids[4]); err != nil {
		t.Fatal(err)
	}
	ids = ids[:4]
	startDownload(t, m, access, mustOpen(t, m, access, addr("files")), "chart.png", 10)
	downloads := threadDownloads(m, access)
	if len(downloads) != 1 {
		t.Fatalf("downloads %+v", downloads)
	}
	if err := m.ActivateCompanionPage(t.Context(), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	mount, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	return m, access, pairFixture{ids: ids, download: downloads[0].ID}, mount.ID
}

var selectionCases = []selectionCase{
	{"activate c", func(ctx context.Context, m *Manager, access Access, f pairFixture) error {
		return m.ActivateCompanionPage(ctx, access, f.ids[2])
	}},
	{"next tab", func(ctx context.Context, m *Manager, access Access, _ pairFixture) error {
		return m.StepCompanionPage(ctx, access, 1)
	}},
	{"previous tab", func(ctx context.Context, m *Manager, access Access, _ pairFixture) error {
		return m.StepCompanionPage(ctx, access, -1)
	}},
	{"new tab", func(ctx context.Context, m *Manager, access Access, _ pairFixture) error {
		_, err := m.NewCompanionPage(ctx, access, addr("n"))
		return err
	}},
	{"reopen", func(ctx context.Context, m *Manager, access Access, _ pairFixture) error {
		return m.ReopenClosedTab(ctx, access)
	}},
	{"duplicate a", func(ctx context.Context, m *Manager, access Access, f pairFixture) error {
		return m.DuplicateCompanionPage(ctx, access, f.ids[0])
	}},
	{"open download", func(ctx context.Context, m *Manager, access Access, f pairFixture) error {
		_, err := m.OpenThreadDownload(ctx, access, DownloadKey(f.download))
		return err
	}},
}

// selectionOutcome is what the user sees: the tabs' addresses in order and
// the active one's place.
func selectionOutcome(m *Manager, access Access) string {
	state := m.CompanionState(access.Caller())
	var urls []string
	active := -1
	for i, page := range state.Pages {
		address := page.URL
		if strings.HasPrefix(address, "file://") {
			// The fixture's one download, whose name depends on what the
			// Downloads folder already holds.
			address = "file:download"
		}
		urls = append(urls, address)
		if page.ID == state.ActivePageID {
			active = i
		}
	}
	return fmt.Sprintf("%v active %d", urls, active)
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

// pauseAdmissions holds every selection from mount right after its
// admission, before its commit, until the test resumes it.
func pauseAdmissions(m *Manager, mount string) (admitted chan Numbered, resume chan struct{}) {
	admitted, resume = make(chan Numbered, 1), make(chan struct{})
	m.selectionAdmitted = func(sel Numbered) {
		if sel.Mount() == MountID(mount) {
			admitted <- sel
			<-resume
		}
	}
	return admitted, resume
}

// detachAndChooseElsewhere detaches mount and selects d in its replacement,
// as a pane that reconnected would, and answers what the user then sees.
func detachAndChooseElsewhere(t *testing.T, m *Manager, access Access, f pairFixture, mount string) (string, int) {
	t.Helper()
	m.DetachPane(mount)
	next, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), next.ID, 1), access, f.ids[3]); err != nil {
		t.Fatal(err)
	}
	return selectionOutcome(m, access), m.CompanionState(access.Caller()).ClosedTabs
}

// A selection admitted before its pane detached commits nothing: the
// replacement pane's choice stands, no tab opens, and a reopen keeps its
// closed tab.
func TestADetachBetweenAdmissionAndCommit(t *testing.T) {
	for _, kind := range selectionCases {
		t.Run(kind.name, func(t *testing.T) {
			m, access, fixture, mount := newPairManager(t)
			m.mu.Lock()
			order := m.panes[mount].order
			m.mu.Unlock()
			admitted, resume := pauseAdmissions(m, mount)
			done := deliver(func() error {
				return kind.act(numbered(context.Background(), mount, 1), m, access, fixture)
			})
			select {
			case <-admitted:
			case <-time.After(10 * time.Second):
				t.Fatal("the selection was never admitted")
			}
			want, closed := detachAndChooseElsewhere(t, m, access, fixture, mount)
			close(resume)
			mustDeliver(t, done, kind.name)
			if got := selectionOutcome(m, access); got != want {
				t.Fatalf("after the old pane's %s: %s, want %s", kind.name, got, want)
			}
			if got := m.CompanionState(access.Caller()).ClosedTabs; got != closed {
				t.Fatalf("%d closed tabs, want %d", got, closed)
			}
			var stats turnorder.Stats
			if order != nil {
				stats = order.Stats()
			}
			if stats.Waiting > 0 || stats.Taken || stats.Outstanding > 0 {
				t.Fatalf("the detached mount kept %d waiting, taken %v, %d outstanding", stats.Waiting, stats.Taken, stats.Outstanding)
			}
		})
	}
}

// A reopen whose pane detaches between its read of the closed tab and its
// tab's admission leaves the closed tab where it was.
func TestADetachDuringAReopenKeepsTheClosedTab(t *testing.T) {
	m, access, fixture, mount := newPairManager(t)
	popped, resume := make(chan struct{}, 1), make(chan struct{})
	m.reopenRead = func() {
		popped <- struct{}{}
		<-resume
	}
	closed := m.CompanionState(access.Caller()).ClosedTabs
	done := deliver(func() error { return m.ReopenClosedTab(numbered(context.Background(), mount, 1), access) })
	receive(t, popped, "the reopen's pop")
	want, _ := detachAndChooseElsewhere(t, m, access, fixture, mount)
	close(resume)
	mustDeliver(t, done, "the reopen")
	if got := selectionOutcome(m, access); got != want {
		t.Fatalf("after the reopen: %s, want %s", got, want)
	}
	if got := m.CompanionState(access.Caller()).ClosedTabs; got != closed {
		t.Fatalf("%d closed tabs, want %d", got, closed)
	}
}

// A reopen whose pane detached before its pop leaves the closed tab on the
// stack the whole time: the hold that would pop it checks the pane first.
func TestADetachBeforeAReopensPopTakesNothing(t *testing.T) {
	m, access, fixture, mount := newPairManager(t)
	var popped atomic.Bool
	m.reopenRead = func() { popped.Store(true) }
	admitted, resume := pauseAdmissions(m, mount)
	done := deliver(func() error { return m.ReopenClosedTab(numbered(context.Background(), mount, 1), access) })
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("the reopen was never admitted")
	}
	detachAndChooseElsewhere(t, m, access, fixture, mount)
	close(resume)
	mustDeliver(t, done, "the reopen")
	if popped.Load() {
		t.Fatal("the reopen popped its closed tab after its pane detached")
	}
}

// A detach drops the mount's record of outstanding numbers with it.
func TestADetachDropsTheOutstandingRecord(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	third := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, ids[2])
	})
	awaitWaiting(t, m, mount, 1)
	clock.expire(t)
	mustDeliver(t, third, "number 3")
	m.mu.Lock()
	order := m.panes[mount].order
	m.mu.Unlock()
	skipped := order.Stats().Outstanding
	if skipped != 2 {
		t.Fatalf("%d numbers outstanding, want 2", skipped)
	}
	m.DetachPane(mount)
	skipped = order.Stats().Outstanding
	if skipped != 0 {
		t.Fatalf("the detached mount kept %d outstanding numbers", skipped)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("a skipped number of a detached pane selected %s", got)
	}
}

// 200 next-tab presses held behind one delayed activation all move.
func TestManyStepsBehindOneDelayedActivation(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	var steps []<-chan error
	for seq := uint64(2); seq <= 201; seq++ {
		steps = append(steps, deliver(func() error {
			return m.StepCompanionPage(numbered(context.Background(), mount, seq), access, 1)
		}))
	}
	awaitWaiting(t, m, mount, 200)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		mustDeliver(t, step, "a step")
	}
	if got, want := activeTab(m, access), ids[200%4]; got != want {
		t.Fatalf("active %s, want %s", got, want)
	}
}

// A number not yet arrived holds the next one for one gap wait; the count
// then moves past it and a later number does not wait. The missing one
// lost its place: arriving late it is refused and selects nothing, and a
// second delivery of it is a repeat.
func TestAMissingNumberLosesItsPlaceAfterOneWait(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	third := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, ids[2])
	})
	awaitWaiting(t, m, mount, 1)
	clock.expire(t)
	mustDeliver(t, third, "number 3")
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("active %s, want c (%s)", got, ids[2])
	}
	timers := clock.timers()
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 4), access, ids[3]); err != nil {
		t.Fatal(err)
	}
	if clock.timers() != timers || activeTab(m, access) != ids[3] {
		t.Fatalf("number 4 waited (%d timers, was %d) or did not select d", clock.timers(), timers)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, ids[1]); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("the late number 2 answered %v", err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 2), access, ids[0]); err != nil {
		t.Fatalf("a repeat of the refused number 2 answered %v", err)
	}
	if got := activeTab(m, access); got != ids[3] {
		t.Fatalf("the late number 2 selected %s, want d (%s) still", got, ids[3])
	}
}

// The user opens a tab, selects one, or steps (1), then selects b (2); 2
// arrives first and applies after the gap wait, and 1, arriving later, is
// refused: it would undo a choice the user made after it. Nothing opens or
// moves.
func TestALateNumberIsRefused(t *testing.T) {
	for _, late := range []struct {
		name string
		act  func(ctx context.Context, m *Manager, access Access, ids []string) error
	}{
		{"new tab", func(ctx context.Context, m *Manager, access Access, _ []string) error {
			_, err := m.NewCompanionPage(ctx, access, addr("n"))
			return err
		}},
		{"activation", func(ctx context.Context, m *Manager, access Access, ids []string) error {
			return m.ActivateCompanionPage(ctx, access, ids[0])
		}},
		{"step", func(ctx context.Context, m *Manager, access Access, _ []string) error {
			return m.StepCompanionPage(ctx, access, 1)
		}},
	} {
		t.Run(late.name, func(t *testing.T) {
			m, access, ids, mount := newSelectionManager(t)
			clock := installGapClock(m)
			second := deliver(func() error {
				return m.ActivateCompanionPage(numbered(context.Background(), mount, 2), access, ids[1])
			})
			awaitWaiting(t, m, mount, 1)
			clock.expire(t)
			mustDeliver(t, second, "number 2")
			if err := late.act(numbered(t.Context(), mount, 1), m, access, ids); !errors.Is(err, turnorder.ErrLost) {
				t.Fatalf("the late %s answered %v", late.name, err)
			}
			if got := activeTab(m, access); got != ids[1] || len(tabIDs(m, access)) != len(ids) {
				t.Fatalf("active %s of %v, want b of the four tabs", got, tabIDs(m, access))
			}
		})
	}
}

// The gap wait gives up a missing number only while no turn is taken: a
// predecessor still selecting is waited for.
func TestAGapWaitDoesNotPassATakenTurn(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	first, err := m.awaitTurn(t.Context(), access, place(mount, 1))
	if err != nil || first.Standing() == turnorder.Passed {
		t.Fatalf("turn 1: %+v, %v", first, err)
	}
	third := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, ids[2])
	})
	awaitWaiting(t, m, mount, 1)
	clock.awaitTimers(t, 1)
	clock.expire(t)
	// The expired wait was handled with turn 1 still taken: 3 waits again.
	clock.awaitTimers(t, 2)
	first.Release()
	// 2 is now the missing one.
	clock.expire(t)
	mustDeliver(t, third, "number 3")
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("active %s, want c", got)
	}
}

// A detach releases the mount's waiters: they answer success and select
// nothing.
func TestADetachReleasesWaitingSelections(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	before := activeTab(m, access)
	second := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 2), access, ids[0])
	})
	third := deliver(func() error {
		_, err := m.NewCompanionPage(numbered(context.Background(), mount, 3), access, addr("n"))
		return err
	})
	awaitWaiting(t, m, mount, 2)
	m.DetachPane(mount)
	mustDeliver(t, second, "number 2")
	mustDeliver(t, third, "number 3")
	if got := activeTab(m, access); got != before {
		t.Fatalf("active %s, want %s unchanged", got, before)
	}
	if got := len(tabIDs(m, access)); got != 4 {
		t.Fatalf("%d tabs, want 4: a released opening opened a tab", got)
	}
}

// Two mounts keep their own orders: one waiting does not hold the other.
func TestTwoMountsKeepTheirOwnOrders(t *testing.T) {
	m, access, ids, one := newSelectionManager(t)
	other, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	two := other.ID
	oneSecond := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), one, 2), access, ids[1])
	})
	twoSecond := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), two, 2), access, ids[3])
	})
	awaitWaiting(t, m, one, 1)
	awaitWaiting(t, m, two, 1)
	if err := m.ActivateCompanionPage(numbered(t.Context(), two, 1), access, ids[2]); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, twoSecond, "the second mount's 2")
	if got := activeTab(m, access); got != ids[3] || waiting(m, one) != 1 {
		t.Fatalf("active %s, want d; %d waiting in the first mount", got, waiting(m, one))
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), one, 1), access, ids[0]); err != nil {
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
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 2), access, ids[3])
	})
	awaitWaiting(t, m, mount, 1)
	visible := true
	for _, tc := range []struct {
		name string
		page string
		act  func() error
	}{
		{"unnumbered", ids[0], func() error { return m.ActivateCompanionPage(t.Context(), access, ids[0]) }},
		{"agent", ids[2], func() error { _, err := m.SelectPage(t.Context(), access, ids[2]); return err }},
		{"header show", ids[0], func() error {
			_, err := m.Visibility(t.Context(), access, &visible, ids[0])
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
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[1]); err != nil {
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
	otherMount, err := m.AttachPane(other.Caller())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, ids[3]); err != nil {
		t.Fatal(err)
	}
	before := selectionOutcome(m, access)
	closed := m.CompanionState(access.Caller()).ClosedTabs
	for name, act := range map[string]func() error{
		"unknown mount": func() error {
			return m.ActivateCompanionPage(numbered(t.Context(), "gone", 1), access, ids[0])
		},
		"another thread's mount": func() error {
			return m.ActivateCompanionPage(numbered(t.Context(), otherMount.ID, 1), access, ids[0])
		},
		"a repeat": func() error {
			return m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0])
		},
		"a passed new tab": func() error {
			_, err := m.NewCompanionPage(numbered(t.Context(), mount, 1), access, addr("n"))
			return err
		},
		"a passed reopen":    func() error { return m.ReopenClosedTab(numbered(t.Context(), mount, 1), access) },
		"a passed duplicate": func() error { return m.DuplicateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]) },
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
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, ids[2])
	})
	awaitWaiting(t, m, mount, 1)
	if err := await(t, deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 3), access, ids[0])
	}), "the repeat"); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids[:2] {
		if err := m.ActivateCompanionPage(numbered(t.Context(), mount, uint64(i+1)), access, id); err != nil {
			t.Fatal(err)
		}
	}
	mustDeliver(t, third, "number 3")
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("active %s, want c", got)
	}
}

// Past the bound on waiting selections a mount refuses the next one.
func TestWaitingSelectionsAreBounded(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	var held []<-chan error
	for seq := uint64(2); seq < 2+turnorder.MaxWaiters; seq++ {
		held = append(held, deliver(func() error {
			return m.StepCompanionPage(numbered(context.Background(), mount, seq), access, 1)
		}))
	}
	awaitWaiting(t, m, mount, turnorder.MaxWaiters)
	excess := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 2+turnorder.MaxWaiters), access, ids[0])
	})
	select {
	case err := <-excess:
		if err == nil {
			t.Fatal("a selection past the bound answered success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a selection past the bound waited")
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, step := range held {
		mustDeliver(t, step, "a step")
	}
	// The refused number was never admitted: sent again before the count
	// passes it, it takes its turn.
	refused := uint64(2 + turnorder.MaxWaiters)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, refused), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got := activeTab(m, access); got != ids[1] {
		t.Fatalf("the retried number did not act: active %s", got)
	}
}

// A gap of any width is skipped in bounded work: the record keeps the top
// of it and the rest is refused when it arrives.
func TestAWideGapIsSkippedInBoundedWork(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	far := uint64(1) << 62
	waiter := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, far), access, ids[2])
	})
	awaitWaiting(t, m, mount, 1)
	clock.expire(t)
	mustDeliver(t, waiter, "the far number")
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("a number below the record answered %v", err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, far-1), access, ids[1]); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("the held far-1 answered %v", err)
	}
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("a refused number selected: active %s", got)
	}
}

// A number refused at the waiter bound and sent again after the count
// passed it is an outstanding number: it is refused on arrival.
func TestARefusedNumberRetriedLateIsRefused(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	clock := installGapClock(m)
	var held []<-chan error
	for seq := uint64(2); seq < 2+turnorder.MaxWaiters; seq++ {
		held = append(held, deliver(func() error {
			return m.StepCompanionPage(numbered(context.Background(), mount, seq), access, 1)
		}))
	}
	awaitWaiting(t, m, mount, turnorder.MaxWaiters)
	refused := uint64(2 + turnorder.MaxWaiters)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, refused), access, ids[1]); err == nil {
		t.Fatal("a selection past the bound was admitted")
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, step := range held {
		mustDeliver(t, step, "a step")
	}
	after := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, refused+1), access, ids[2])
	})
	awaitWaiting(t, m, mount, 1)
	clock.expire(t)
	mustDeliver(t, after, "the number after the refused one")
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, refused), access, ids[1]); !errors.Is(err, turnorder.ErrLost) {
		t.Fatalf("the late retry answered %v", err)
	}
	if got := activeTab(m, access); got != ids[2] {
		t.Fatalf("the late retry selected: active %s", got)
	}
}

// A waiting selection whose request ends leaves no waiter behind.
func TestACanceledWaitLeavesNoWaiter(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	second := deliver(func() error {
		return m.ActivateCompanionPage(numbered(ctx, mount, 2), access, ids[0])
	})
	awaitWaiting(t, m, mount, 1)
	cancel()
	if err := await(t, second, "the canceled wait"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the canceled wait answered %v", err)
	}
	if waiting(m, mount) != 0 {
		t.Fatal("the canceled wait is still waiting")
	}
}

// newSuspendedPair opens tabs a and b, suspends both, and attaches a pane
// mount.
func newSuspendedPair(t *testing.T) (*Manager, *lifecycleEngine, Access, PageInfo, PageInfo, string) {
	t.Helper()
	m, engine := newLifecycleManager(t)
	installGapClock(m)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	a := mustOpen(t, m, access, addr("a"))
	b := mustOpen(t, m, access, addr("b"))
	mount, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { assertSelectionsSettled(t, m) })
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

// The next selection waits for an admitted one's selection, never for its
// restore: while a's restore is held, the user's next click selects c, and
// the restore then neither takes the selection back nor shows a.
func TestASelectionDoesNotWaitForARestore(t *testing.T) {
	m, engine, access, a, _, mount := newSuspendedPair(t)
	c := mustOpen(t, m, access, addr("c"))
	release := engine.hold(&engine.navGate)
	defer release()
	restoring := deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 1), access, a.ID)
	})
	receive(t, engine.navigating, "a's restore")
	if err := await(t, deliver(func() error {
		return m.ActivateCompanionPage(numbered(context.Background(), mount, 2), access, c.ID)
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
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, b.ID); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{numbered(t.Context(), mount, 1), numbered(t.Context(), "gone", 1)} {
		if err := m.ActivateCompanionPage(ctx, access, a.ID); err != nil {
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
