package trackerapp

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// cleanupStore holds the first cleanup until release closes, and fails
// every cleanup with fail when it is set.
type cleanupStore struct {
	*store.Store
	arrived, release chan struct{}
	once             sync.Once
	fail             error
	calls            atomic.Int32
}

func (c *cleanupStore) FinishMirrorCleanup(pause store.ChunkPause) error {
	c.calls.Add(1)
	if c.arrived != nil {
		c.once.Do(func() {
			close(c.arrived)
			<-c.release
		})
	}
	if c.fail != nil {
		return c.fail
	}
	return c.Store.FinishMirrorCleanup(pause)
}

// logs captures what the service logs.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// The cleanup after a turn off runs with opMu released: other connection
// changes and ticks proceed, and the connection turned off reads as off.
func TestCleanupAfterTurnOffRunsOutsideTheConnectionLock(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	cs := &cleanupStore{Store: m.st, arrived: make(chan struct{}), release: make(chan struct{})}
	m.svc.store = cs
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	other := m.addLinear()
	off := make(chan error)
	go func() {
		_, err := m.svc.SetEnabled(m.conn.ID, false)
		off <- err
	}()
	<-cs.arrived
	if _, err := m.svc.Rename(other.ID, "Other"); err != nil {
		t.Fatal(err)
	}
	if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); !out.Ended {
		t.Fatalf("a delta during the cleanup %+v", out)
	}
	close(cs.release)
	if err := <-off; err != nil {
		t.Fatal(err)
	}
}

// A cleanup that fails after a turn off or a removal is logged and the
// change stands; FinishMirrorCleanup returns it.
func TestCleanupFailureIsLoggedAndTheChangeStands(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	disk := errors.New("disk I/O error")
	m.svc.store = &cleanupStore{Store: m.st, fail: disk}
	var l logs
	m.svc.logf = l.logf
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	c, err := m.svc.SetEnabled(m.conn.ID, false)
	if err != nil || c.Enabled {
		t.Fatalf("turn off: %+v, %v", c, err)
	}
	if got := l.text(); !strings.Contains(got, "turning off connection "+m.conn.ID) || !strings.Contains(got, disk.Error()) {
		t.Fatalf("log %q", got)
	}
	if err := m.svc.Remove(m.conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.GetTrackerConnection(m.conn.ID); err == nil {
		t.Fatal("the removal did not stand")
	}
	if got := l.text(); !strings.Contains(got, "removing connection "+m.conn.ID) {
		t.Fatalf("log %q", got)
	}
	if err := m.svc.FinishMirrorCleanup(); !errors.Is(err, disk) {
		t.Fatalf("FinishMirrorCleanup: %v", err)
	}
}

// panicStore fails the test on any store call.
type panicStore struct{ Store }

// New reads and writes nothing: a service with no connection used costs
// nothing at start.
func TestNewTouchesNothing(t *testing.T) {
	t.Parallel()
	s, err := New(Deps{
		Store: panicStore{}, Changed: func() {}, CredentialsDir: t.TempDir(), Origin: APIOrigin, Version: "1",
		Now: time.Now, Life: t.Context(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Stop()
}

// A drain that stopped part way is finished by FinishMirrorCleanup (New
// touches nothing: TestNewTouchesNothing).
func TestFinishMirrorCleanupDrainsAnInterruptedClear(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if _, err := m.st.DetachMirror(m.conn.ID); err != nil {
		t.Fatal(err)
	}
	// pending is how many chunks a store cleanup would drain, draining
	// none: the first pause stops it.
	pending := func() int {
		var pauses atomic.Int32
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			_ = m.st.FinishMirrorCleanup(func() { pauses.Add(1); runtime.Goexit() })
		}()
		<-stopped
		return int(pauses.Load())
	}
	if pending() == 0 {
		t.Fatal("the detach left nothing to drain")
	}
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if n := pending(); n != 0 {
		t.Fatalf("the cleanup left %d chunks", n)
	}
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Fatalf("a second cleanup: %v", err)
	}
}

// An issue a thread no longer links, with no board row, leaves the mirror
// at the next cleanup; one still linked stays.
func TestCleanupRemovesIssuesNothingKeeps(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.linkThread("thread-1", "10005")
	m.p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
		return tracker.Cards{Cards: []tracker.Card{card("10005", 1_799_000_000_000)}}, nil
	}
	must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10001", "10005"})); len(cards) != 2 {
		t.Fatalf("the cleanup removed a kept issue: %+v", cards)
	}
	if _, err := m.st.ClearThreadTicket("thread-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Fatal(err)
	}
	if cards := must[[]store.MirrorCard](t)(m.st.ListMirrorCards(m.root(), []string{"10001", "10005"})); len(cards) != 1 || cards[0].IssueID != "10001" {
		t.Fatalf("cards after the unlink and cleanup %+v", cards)
	}
}
