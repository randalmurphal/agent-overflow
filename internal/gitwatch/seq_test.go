package gitwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

// gatedRead is a status read the test finishes: each call announces
// itself on entered and returns the status the test sends on replies, or
// the error it sends on failures.
type gatedRead struct {
	entered  chan struct{}
	replies  chan gitops.GitStatus
	failures chan error
}

func newGatedRead() *gatedRead {
	return &gatedRead{entered: make(chan struct{}, 8), replies: make(chan gitops.GitStatus), failures: make(chan error)}
}

func (g *gatedRead) read(ctx context.Context) (gitops.GitStatus, error) {
	g.entered <- struct{}{}
	select {
	case status := <-g.replies:
		return status, nil
	case err := <-g.failures:
		return gitops.GitStatus{}, err
	case <-ctx.Done():
		return gitops.GitStatus{}, ctx.Err()
	}
}

func (g *gatedRead) fail(t *testing.T, err error) {
	t.Helper()
	select {
	case g.failures <- err:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a status read to take its failure")
	}
}

func (g *gatedRead) fn() StatusFn {
	return func(ctx context.Context, _ string) (gitops.GitStatus, error) { return g.read(ctx) }
}

func (g *gatedRead) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a status read to begin")
	}
}

func (g *gatedRead) finish(t *testing.T, status gitops.GitStatus) {
	t.Helper()
	select {
	case g.replies <- status:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a status read to take its answer")
	}
}

// newSeqWatch subscribes to a workspace whose watcher reads through
// refreshes and whose initial read is immediate, with no filesystem
// watches, so only the test starts a read.
func newSeqWatch(t *testing.T) (*Manager, *gatedRead, string, *Subscription) {
	t.Helper()
	refreshes := newGatedRead()
	mgr := NewManager(ManagerConfig{
		StatusFn: refreshes.fn(),
		FastStatusFn: func(context.Context, string) (gitops.GitStatus, error) {
			return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
		},
	})
	mgr.installFn = func([]gitops.WatchRoot, chan<- notify.EventInfo) error { return nil }
	t.Cleanup(mgr.Close)
	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(t.Context(), dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	if sub.Initial().Seq == 0 {
		t.Fatalf("initial status %+v carries no seq", sub.Initial())
	}
	return mgr, refreshes, dir, sub
}

type publishResult struct {
	status gitops.GitStatus
	err    error
}

// publishAsync runs Publish with a read the test finishes through the
// returned gatedRead.
func publishAsync(mgr *Manager, dir string) (*gatedRead, <-chan publishResult) {
	read := newGatedRead()
	done := make(chan publishResult, 1)
	go func() {
		status, err := mgr.Publish(dir, func() (gitops.GitStatus, error) { return read.read(context.Background()) })
		done <- publishResult{status, err}
	}()
	return read, done
}

func awaitPublish(t *testing.T, done <-chan publishResult) gitops.GitStatus {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("Publish: %v", result.err)
		}
		return result.status
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Publish")
		return gitops.GitStatus{}
	}
}

func expectNoQueuedUpdate(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case status := <-sub.Updates():
		t.Fatalf("unexpected update: %+v", status)
	default:
	}
}

// A Publish whose read began first and finished last loses to the
// watcher's refresh that began after it: its answer is the refresh's
// status, and its own read reaches no subscriber.
func TestPublishLosesToARefreshThatBeganLater(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)

	slow, done := publishAsync(mgr, dir)
	slow.awaitEntered(t)

	mgr.RequestRefresh(dir)
	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 2})
	later := recvWithin(t, sub, 3*time.Second)
	if later.FileCount != 2 {
		t.Fatalf("refresh broadcast %+v, want FileCount 2", later)
	}

	slow.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1})
	got := awaitPublish(t, done)
	if got != later {
		t.Fatalf("Publish answered %+v, want the later refresh's %+v", got, later)
	}
	// The next refresh starts only after the slow read was offered, so
	// nothing queued means it lost.
	refreshes.awaitEntered(t)
	expectNoQueuedUpdate(t, sub)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 2})
	expectNoUpdate(t, sub, 100*time.Millisecond)
}

// A refresh whose read began first and finished last loses to a Publish
// that began after it: the Publish is broadcast and the refresh is not.
func TestRefreshLosesToAPublishThatBeganLater(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)

	mgr.RequestRefresh(dir)
	refreshes.awaitEntered(t)

	published, err := mgr.Publish(dir, func() (gitops.GitStatus, error) {
		return gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1}, nil
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.FileCount != 1 || published.Seq <= sub.Initial().Seq {
		t.Fatalf("Publish answered %+v, want its own read after the initial seq %d", published, sub.Initial().Seq)
	}
	if got := recvWithin(t, sub, 3*time.Second); got != published {
		t.Fatalf("broadcast %+v, want the published %+v", got, published)
	}

	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 2})
	// The next refresh starts only after the slow one was offered, so
	// nothing queued means it lost.
	refreshes.awaitEntered(t)
	expectNoQueuedUpdate(t, sub)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1})
	expectNoUpdate(t, sub, 100*time.Millisecond)
}

// A Publish whose read began first and finished last loses to a Publish
// that began after it.
func TestPublishLosesToAPublishThatBeganLater(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)

	slow, done := publishAsync(mgr, dir)
	slow.awaitEntered(t)
	fast, err := mgr.Publish(dir, func() (gitops.GitStatus, error) {
		return gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 2}, nil
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	slow.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1})
	if got := awaitPublish(t, done); got != fast {
		t.Fatalf("slow Publish answered %+v, want the later one's %+v", got, fast)
	}
	if got := recvWithin(t, sub, 3*time.Second); got != fast {
		t.Fatalf("broadcast %+v, want %+v", got, fast)
	}
	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 2})
	expectNoUpdate(t, sub, 100*time.Millisecond)
}

// A Publish that lands while the workspace's first watcher reads its
// initial status, and began after that read, is the status the watcher
// starts from.
func TestPublishDuringTheFirstSubscribeIsTheInitialStatus(t *testing.T) {
	t.Parallel()
	initialRead := newGatedRead()
	mgr := NewManager(ManagerConfig{
		StatusFn: func(context.Context, string) (gitops.GitStatus, error) {
			return gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1}, nil
		},
		FastStatusFn: initialRead.fn(),
	})
	mgr.installFn = func([]gitops.WatchRoot, chan<- notify.EventInfo) error { return nil }
	t.Cleanup(mgr.Close)
	dir := makeRepoDir(t)

	subscribed := make(chan *Subscription, 1)
	go func() {
		sub, err := mgr.Subscribe(context.Background(), dir)
		if err != nil {
			t.Errorf("subscribe: %v", err)
		}
		subscribed <- sub
	}()
	initialRead.awaitEntered(t)
	published, err := mgr.Publish(dir, func() (gitops.GitStatus, error) {
		return gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1}, nil
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	initialRead.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 9})
	var sub *Subscription
	select {
	case sub = <-subscribed:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Subscribe")
	}
	if sub == nil {
		t.FailNow()
	}
	defer sub.Close()
	if got := sub.Initial(); got != published {
		t.Fatalf("initial status %+v, want the later Publish's %+v", got, published)
	}
	mgr.mu.Lock()
	creating := len(mgr.creating)
	mgr.mu.Unlock()
	if creating != 0 {
		t.Fatalf("%d creating entries left after Subscribe returned", creating)
	}
}

func creatingCount(mgr *Manager) int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.creating)
}

// A first Subscribe that fails, at its watch roots or at its initial
// read, leaves no creating entry behind.
func TestFailedFirstSubscribeLeavesNoCreatingEntry(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for name, config := range map[string]ManagerConfig{
		"watch roots": {
			StatusFn:     newStubStatus(gitops.GitStatus{IsRepo: true}).fn(),
			WatchRootsFn: func(string) ([]gitops.WatchRoot, error) { return nil, boom },
		},
		"initial read": {
			StatusFn: func(context.Context, string) (gitops.GitStatus, error) { return gitops.GitStatus{}, boom },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mgr := NewManager(config)
			mgr.installFn = func([]gitops.WatchRoot, chan<- notify.EventInfo) error { return nil }
			t.Cleanup(mgr.Close)
			if sub, err := mgr.Subscribe(t.Context(), makeRepoDir(t)); !errors.Is(err, boom) {
				if sub != nil {
					sub.Close()
				}
				t.Fatalf("Subscribe error = %v, want %v", err, boom)
			}
			if n := creatingCount(mgr); n != 0 {
				t.Fatalf("%d creating entries left after the failed Subscribe", n)
			}
		})
	}
}

// Of two racing first Subscribes, one whose initial read fails does not
// take the creating entry from the other: a Publish after the failure
// is still the initial status of the one that succeeds.
func TestRacingSubscribeKeepsTheOfferAfterTheOtherFails(t *testing.T) {
	t.Parallel()
	initialReads := newGatedRead()
	mgr := NewManager(ManagerConfig{
		StatusFn:     newStubStatus(gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1}).fn(),
		FastStatusFn: initialReads.fn(),
	})
	mgr.installFn = func([]gitops.WatchRoot, chan<- notify.EventInfo) error { return nil }
	t.Cleanup(mgr.Close)
	dir := makeRepoDir(t)

	type subscribed struct {
		sub *Subscription
		err error
	}
	results := make(chan subscribed, 2)
	for range 2 {
		go func() {
			sub, err := mgr.Subscribe(context.Background(), dir)
			results <- subscribed{sub, err}
		}()
	}
	initialReads.awaitEntered(t)
	initialReads.awaitEntered(t)
	receive := func() subscribed {
		t.Helper()
		select {
		case result := <-results:
			return result
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for Subscribe")
			return subscribed{}
		}
	}

	boom := errors.New("boom")
	initialReads.fail(t, boom)
	if failed := receive(); !errors.Is(failed.err, boom) {
		t.Fatalf("first Subscribe to return = %+v, want the failed read", failed)
	}
	published, err := mgr.Publish(dir, func() (gitops.GitStatus, error) {
		return gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 1}, nil
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	initialReads.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 9})
	ok := receive()
	if ok.err != nil {
		t.Fatalf("second Subscribe: %v", ok.err)
	}
	defer ok.sub.Close()
	if got := ok.sub.Initial(); got != published {
		t.Fatalf("initial status %+v, want the Publish made after the other Subscribe failed %+v", got, published)
	}
	if n := creatingCount(mgr); n != 0 {
		t.Fatalf("%d creating entries left after both Subscribes returned", n)
	}
}

// A Publish whose content equals the published status broadcasts nothing,
// and the published status takes its seq: the answer and every later
// snapshot carry it.
func TestPublishOfTheSameContentTakesItsSeqWithoutABroadcast(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)
	initial := sub.Initial()

	got, err := mgr.Publish(dir, func() (gitops.GitStatus, error) {
		return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !got.Equal(initial) || got.Seq <= initial.Seq {
		t.Fatalf("Publish answered %+v, want the initial content at a seq above %d", got, initial.Seq)
	}
	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main"})
	expectNoUpdate(t, sub, 100*time.Millisecond)

	next, err := mgr.Subscribe(t.Context(), dir)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	defer next.Close()
	if snapshot := next.Initial(); snapshot.Seq <= got.Seq {
		t.Fatalf("snapshot %+v, want a seq above the Publish's %d (the follow-up refresh read later)", snapshot, got.Seq)
	}
}

// With nobody watching, Publish answers with its read stamped with a seq
// taken before the read, and later reads get higher ones.
func TestPublishWithoutAWatcherStampsTheRead(t *testing.T) {
	t.Parallel()
	mgr := NewManager(ManagerConfig{StatusFn: newStubStatus(gitops.GitStatus{}).fn()})
	t.Cleanup(mgr.Close)
	dir := makeRepoDir(t)

	var during uint64
	first, err := mgr.Publish(dir, func() (gitops.GitStatus, error) {
		during = mgr.seq.n.Load()
		return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if first.Branch != "main" || first.Seq == 0 || first.Seq != during {
		t.Fatalf("Publish answered %+v, want the read stamped with the seq taken before it (%d)", first, during)
	}
	second, err := mgr.Publish(dir, func() (gitops.GitStatus, error) { return gitops.GitStatus{IsRepo: true}, nil })
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("second seq %d, want above %d", second.Seq, first.Seq)
	}
}

// A failed read publishes nothing and returns its error; an empty cwd is
// refused before any read.
func TestPublishRefusesEmptyCwdAndReturnsReadErrors(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)

	boom := errors.New("boom")
	if _, err := mgr.Publish(dir, func() (gitops.GitStatus, error) { return gitops.GitStatus{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("Publish error = %v, want %v", err, boom)
	}
	expectNoUpdate(t, sub, 100*time.Millisecond)
	select {
	case <-refreshes.entered:
		t.Fatal("a failed Publish requested a refresh")
	default:
	}

	read := false
	if _, err := mgr.Publish("", func() (gitops.GitStatus, error) {
		read = true
		return gitops.GitStatus{}, nil
	}); err == nil || read {
		t.Fatalf("Publish(\"\") error = %v, read = %v; want a refusal before reading", err, read)
	}
}

// A Publish that changed working-tree content counts as a change for the
// refresh it requests: with the event stream quiet, the watches missed it
// and are reinstalled, although the refresh itself finds nothing new.
func TestPublishedChangeArmsMissDetection(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		w.livenessQuiet = 0
	})
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	changed := gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 5}
	h.stub.setStatus(changed)
	changed.Seq = h.w.seq.next()
	if got := h.w.offer(changed); got != changed {
		t.Fatalf("offer published %+v, want %+v", got, changed)
	}
	if got := recvWithin(t, sub, 3*time.Second); got != changed {
		t.Fatalf("broadcast %+v, want %+v", got, changed)
	}
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() >= 2 },
		"forced reinstall after a published change the watches never reported")
}

// A Publish whose only change is the PR fields says nothing about the
// watches, as for a requested refresh.
func TestPublishedPRChangeDoesNotReinstall(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		w.livenessQuiet = 0
	})
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	withPR := gitops.GitStatus{Branch: "main", OpenPRURL: "https://example.com/pr/1", OpenPRNumber: 1}
	h.stub.setStatus(withPR)
	withPR.Seq = h.w.seq.next()
	h.w.offer(withPR)
	recvWithin(t, sub, 3*time.Second)
	waitForCompletedCallCount(t, h.stub, 1, 3*time.Second)
	expectNoUpdate(t, sub, 200*time.Millisecond)
	if got := h.installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (a PR-only change must not reinstall)", got)
	}
}
