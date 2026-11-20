package trackerapp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// loopStore counts the store calls the linked loop makes, can fail its
// cleanup, and wakes a test waiting on its counts.
type loopStore struct {
	*store.Store
	mu                      sync.Mutex
	lists, linked, cleanups int
	asked                   int
	cleanupErr, listErr     error
	changed                 chan struct{}
	connected               bool
}

func newLoopStore(st *store.Store) *loopStore {
	return &loopStore{Store: st, changed: make(chan struct{}), connected: true}
}

func (l *loopStore) count(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f()
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *loopStore) ListTrackerConnections() ([]store.TrackerConnection, error) {
	var err error
	l.count(func() { l.lists++; err = l.listErr })
	if err != nil {
		return nil, err
	}
	return l.Store.ListTrackerConnections()
}

func (l *loopStore) ListMirrorOffBoardLinkedIssues(root int64, after string) ([]string, bool, error) {
	l.count(func() { l.linked++ })
	return l.Store.ListMirrorOffBoardLinkedIssues(root, after)
}

func (l *loopStore) FinishMirrorCleanup(pause store.ChunkPause) error {
	var err error
	l.count(func() { l.cleanups++; err = l.cleanupErr })
	if err != nil {
		return err
	}
	return l.Store.FinishMirrorCleanup(pause)
}

// clientConnected is the service's ClientConnected, counting its asks.
func (l *loopStore) clientConnected() bool {
	var connected bool
	l.count(func() { l.asked++; connected = l.connected })
	return connected
}

func (l *loopStore) snapshot() (lists, linked, cleanups, asked int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lists, l.linked, l.cleanups, l.asked
}

// waitFor waits until cond holds of the counts.
func (l *loopStore) waitFor(t *testing.T, what string, cond func(lists, linked, cleanups, asked int) bool) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		l.mu.Lock()
		ok := cond(l.lists, l.linked, l.cleanups, l.asked)
		ch := l.changed
		l.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			lists, linked, cleanups, asked := l.snapshot()
			t.Fatalf("waited for %s: %d lists, %d linked reads, %d cleanups, %d asks", what, lists, linked, cleanups, asked)
		}
	}
}

// loopRunning is the running loop's exit channel; nil while none runs.
func (s *Service) loopRunning() <-chan struct{} {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.subs.loop.done
}

// newLoopFixture is a mirror fixture with no connection whose store and
// client callback count the loop's work.
func newLoopFixture(t *testing.T) (*fixture, *loopStore) {
	t.Helper()
	f := newFixtureWith(t, Cadence{Linked: 5 * time.Millisecond})
	ls := newLoopStore(f.st)
	f.svc.store = ls
	f.svc.clientConnected = ls.clientConnected
	p := &scripted{t: t}
	f.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) { return p, nil }
	return f, ls
}

// The loop costs one read of the connections and no goroutine while no
// Jira or Linear connection is on; it starts with the first one turned on
// or added, and has exited when the call turning the last one off, or
// removing it, returns.
func TestLinkedLoopFollowsTheConnections(t *testing.T) {
	t.Parallel()
	f, ls := newLoopFixture(t)
	if lists, linked, cleanups, asked := ls.snapshot(); lists+linked+cleanups+asked != 0 {
		t.Fatalf("New made %d store calls and %d asks for the loop", lists+linked+cleanups, asked)
	}
	if err := f.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	if lists, _, _, _ := ls.snapshot(); lists != 2 {
		t.Fatalf("arming the loop twice with no connection read the connections %d times; want 2", lists)
	}
	if f.svc.loopRunning() != nil {
		t.Fatal("the loop runs with no connection on")
	}

	conn := f.addJira()
	if f.svc.loopRunning() == nil {
		t.Fatal("the loop does not run after a Jira connection was added")
	}
	ls.waitFor(t, "a round", func(_, linked, cleanups, _ int) bool { return linked >= 1 && cleanups >= 2 })

	stopped := f.svc.loopRunning()
	if _, err := f.svc.SetEnabled(conn.ID, false); err != nil {
		t.Fatal(err)
	}
	assertExited(t, stopped, "turning the last connection off")

	if _, err := f.svc.SetEnabled(conn.ID, true); err != nil {
		t.Fatal(err)
	}
	stopped = f.svc.loopRunning()
	if stopped == nil {
		t.Fatal("the loop does not run after the connection was turned on again")
	}
	if err := f.svc.Remove(conn.ID); err != nil {
		t.Fatal(err)
	}
	assertExited(t, stopped, "removing the last connection")
}

func assertExited(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatalf("the loop runs after %s returned", what)
	}
}

// A loop running because a read of the connections failed, with none on,
// stops at the next read that succeeds: a later StartLinkedSync, or its
// own round.
func TestALoopWithNoConnectionStopsAtTheNextRead(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"start", "round"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f, ls := newLoopFixture(t)
			// No round reads the connections until the test lets one.
			ls.count(func() { ls.connected, ls.listErr = false, errors.New("test: the connections could not be read") })
			if err := f.svc.StartLinkedSync(); err == nil {
				t.Fatal("a failed read of the connections reported no failure")
			}
			running := f.svc.loopRunning()
			if running == nil {
				t.Fatal("no loop runs after the failed read")
			}
			ls.count(func() { ls.listErr = nil })
			if by == "start" {
				if err := f.svc.StartLinkedSync(); err != nil {
					t.Fatal(err)
				}
				assertExited(t, running, "StartLinkedSync with no connection on")
				return
			}
			ls.count(func() { ls.connected = true })
			waitClosed(t, "the loop with no connection on", running)
			if f.svc.loopRunning() != nil {
				t.Fatal("a loop runs with no connection on")
			}
		})
	}
}

// The loop waits for the first reads to settle before its first round.
func TestLinkedLoopWaitsForTheSettle(t *testing.T) {
	t.Parallel()
	f, ls := newLoopFixture(t)
	release := f.events.holdSettle()
	f.addJira()
	if err := f.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	f.events.waitFor(t, "the settle wait", func([]BoardEvent, []IssueEvent, []error) bool { return f.events.settles.Load() == 1 })
	if _, linked, cleanups, asked := ls.snapshot(); linked+cleanups+asked != 0 {
		t.Fatalf("before the settle the loop made %d linked reads, %d cleanups and %d asks", linked, cleanups, asked)
	}
	release()
	ls.waitFor(t, "the first round", func(_, linked, cleanups, _ int) bool { return linked >= 1 && cleanups >= 1 })
}

// With no client connected the loop skips its rounds, after one cleanup;
// a client connecting resumes them.
func TestLinkedLoopSkipsWithNoClient(t *testing.T) {
	t.Parallel()
	f, ls := newLoopFixture(t)
	ls.connected = false
	f.addJira()
	if err := f.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	ls.waitFor(t, "five skipped rounds", func(_, _, _, asked int) bool { return asked >= 5 })
	if _, linked, cleanups, _ := ls.snapshot(); linked != 0 || cleanups != 1 {
		t.Fatalf("with no client the loop made %d linked reads and %d cleanups; want 0 and 1", linked, cleanups)
	}
	ls.count(func() { ls.connected = true })
	ls.waitFor(t, "a round", func(_, linked, cleanups, _ int) bool { return linked >= 1 && cleanups >= 2 })
}

// A cleanup that fails is reported, and the loop goes on.
func TestLinkedLoopSurvivesACleanupFailure(t *testing.T) {
	t.Parallel()
	f, ls := newLoopFixture(t)
	failed := errors.New("test: the cleanup failed")
	ls.cleanupErr = failed
	f.addJira()
	if err := f.svc.StartLinkedSync(); err != nil {
		t.Fatal(err)
	}
	f.events.waitFor(t, "three failures", func(_ []BoardEvent, _ []IssueEvent, errs []error) bool { return len(errs) >= 3 })
	_, _, errs := f.events.snapshot()
	for _, err := range errs {
		if !errors.Is(err, failed) {
			t.Fatalf("the loop reported %v", err)
		}
	}
	ls.waitFor(t, "rounds after the failures", func(_, linked, _, _ int) bool { return linked >= 3 })
}
