package browser

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	order := m.panes[MountID(mount)].order
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
		mounts = append(mounts, string(id))
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
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[4])); err != nil {
		t.Fatal(err)
	}
	ids = ids[:4]
	startDownload(t, m, access, mustOpen(t, m, access, addr("files")), "chart.png", 10)
	downloads := threadDownloads(m, access)
	if len(downloads) != 1 {
		t.Fatalf("downloads %+v", downloads)
	}
	if err := m.ActivateCompanionPage(t.Context(), access, PageID(ids[1])); err != nil {
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
		return m.ActivateCompanionPage(ctx, access, PageID(f.ids[2]))
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
		return m.DuplicateCompanionPage(ctx, access, PageID(f.ids[0]))
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
	m.DetachPane(MountID(mount))
	next, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateCompanionPage(numbered(t.Context(), next.ID, 1), access, PageID(f.ids[3])); err != nil {
		t.Fatal(err)
	}
	return selectionOutcome(m, access), m.CompanionState(access.Caller()).ClosedTabs
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
	if err := m.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if tab, _ := findTab(m, access, id); !tab.Suspended {
			t.Fatalf("tab %s was not suspended: %+v", id, tab)
		}
	}
	return m, engine, access, a, b, mount.ID
}
