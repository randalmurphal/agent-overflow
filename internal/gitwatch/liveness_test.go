package gitwatch

// Tests for the silent-death recovery layer: the requestRefresh
// miss-detection and the liveness probe ticker. Both exist because an
// fs-watch install can "succeed" and then never deliver (dead FSEvents
// stream after a macOS dark-wake, incident 2026-08-01), and every other
// rebuild trigger rides on fs events — a fully deaf watcher could never
// heal itself.

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

// livenessHarness is a slim fixture for driving the run loop with
// injected events, a swappable status, and counted installs. Unlike
// rebuildHarness it exposes the watcher pre-start so tests can shrink
// the liveness timings.
type livenessHarness struct {
	ws       string
	stub     *stubStatus
	installs *countingInstall
	w        *workspaceWatcher

	fastCalls atomic.Int32
}

// newLivenessHarness builds (but does not start) the watcher. The fast
// status fn shares the stub's state and counts its own invocations so
// tests can pin which fn the probe used. configure runs before start.
func newLivenessHarness(t *testing.T, configure func(w *workspaceWatcher)) *livenessHarness {
	t.Helper()
	h := &livenessHarness{
		ws:       makeRepoDir(t),
		stub:     newStubStatus(gitops.GitStatus{Branch: "main"}),
		installs: &countingInstall{},
	}
	roots := []gitops.WatchRoot{subtreeRoot(h.ws)}
	rootsFn := func() ([]gitops.WatchRoot, error) {
		return append([]gitops.WatchRoot(nil), roots...), nil
	}
	inner := h.stub.fn()
	fastFn := func(ctx context.Context, cwd string) (gitops.GitStatus, error) {
		h.fastCalls.Add(1)
		return inner(ctx, cwd)
	}
	h.w = newWorkspaceWatcher(h.ws, h.stub.fn(), fastFn, roots, rootsFn, new(sequence))
	if configure != nil {
		configure(h.w)
	}
	h.w.install(h.installs.fn())
	h.w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(h.w.stop)
	return h
}

func (h *livenessHarness) fastCallCount() int { return int(h.fastCalls.Load()) }

func TestStatusDiffersIgnoringPR(t *testing.T) {
	t.Parallel()
	base := gitops.GitStatus{IsRepo: true, Branch: "main", Insertions: 3, Forge: "github"}

	prOnly := base
	prOnly.OpenPRURL = "https://example.com/pr/7"
	prOnly.OpenPRNumber = 7
	prOnly.OpenPRLookupError = "boom"
	prOnly.OpenPRLookupPending = true
	prOnly.OpenPRRecheckAt = time.Now()
	if statusDiffersIgnoringPR(base, prOnly) {
		t.Fatalf("PR-only delta must not count as a non-PR change")
	}

	churn := base
	churn.Insertions = 9
	if !statusDiffersIgnoringPR(base, churn) {
		t.Fatalf("working-tree delta must count as a non-PR change")
	}
}

// A requestRefresh that observes working-tree changes while the event
// stream has been quiet proves the watches missed them: the watcher
// must broadcast AND force a reinstall.
func TestRequestRefreshMissDetectionReinstalls(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		w.livenessQuiet = 0 // every refresh-observed change counts as a miss
	})
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	h.stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 5})
	h.w.requestRefresh()

	status := recvWithin(t, sub, 3*time.Second)
	if status.Insertions != 5 {
		t.Fatalf("broadcast = %+v, want the missed status", status)
	}
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() >= 2 },
		"forced reinstall after miss detection")
}

// A change made while miss detection has the watches out raises no event:
// the watcher refreshes again once they are back.
func TestRequestRefreshSeesAChangeMadeDuringTheReinstall(t *testing.T) {
	t.Parallel()
	ws := makeRepoDir(t)
	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	roots := []gitops.WatchRoot{subtreeRoot(ws)}
	rootsFn := func() ([]gitops.WatchRoot, error) { return roots, nil }
	w := newWorkspaceWatcher(ws, stub.fn(), nil, roots, rootsFn, new(sequence))
	w.livenessQuiet = 0 // every refresh-observed change counts as a miss
	var installs atomic.Int32
	w.install(func([]gitops.WatchRoot, chan<- notify.EventInfo) error {
		if installs.Add(1) == 2 {
			stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 9})
		}
		return nil
	})
	sub := w.addSubscriber(gitops.GitStatus{Branch: "main"})
	w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(w.stop)

	stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 5})
	w.requestRefresh()

	// A subscriber holds only the latest status, so the first may be
	// superseded before it is read.
	for {
		status := recvWithin(t, sub, 3*time.Second)
		if status.Insertions == 9 {
			return
		}
		if status.Insertions != 5 {
			t.Fatalf("broadcast = %+v, want the missed status, then the one changed during the reinstall", status)
		}
	}
}

// Miss detection that could not take the watches out (the roots could not
// be recomputed) has no gap to cover, so it reads the status once.
func TestRequestRefreshReadsOnceWhenTheWatchesStay(t *testing.T) {
	t.Parallel()
	ws := makeRepoDir(t)
	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	roots := []gitops.WatchRoot{subtreeRoot(ws)}
	rootsFn := func() ([]gitops.WatchRoot, error) { return nil, errors.New("roots unavailable") }
	w := newWorkspaceWatcher(ws, stub.fn(), nil, roots, rootsFn, new(sequence))
	w.livenessQuiet = 0 // every refresh-observed change counts as a miss
	w.install(func([]gitops.WatchRoot, chan<- notify.EventInfo) error { return nil })
	sub := w.addSubscriber(gitops.GitStatus{Branch: "main"})
	w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(w.stop)

	stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 5})
	w.requestRefresh()
	if status := recvWithin(t, sub, 3*time.Second); status.Insertions != 5 {
		t.Fatalf("broadcast = %+v, want the missed status", status)
	}
	expectNoUpdate(t, sub, 200*time.Millisecond)
	if calls := stub.callCount(); calls != 1 {
		t.Fatalf("status read %d times, want 1", calls)
	}
}

// A directory created between computing a new watcher's roots and
// installing them is in neither: the run loop recomputes the roots before
// any event, reinstalls, and refreshes for what changed meanwhile.
func TestNewWatcherRevalidatesItsRootsBeforeAnyEvent(t *testing.T) {
	t.Parallel()
	ws := makeRepoDir(t)
	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	installed := []gitops.WatchRoot{subtreeRoot(ws)}
	current := []gitops.WatchRoot{subtreeRoot(ws), subtreeRoot(filepath.Join(ws, "created"))}
	rootsFn := func() ([]gitops.WatchRoot, error) { return current, nil }
	w := newWorkspaceWatcher(ws, stub.fn(), nil, installed, rootsFn, new(sequence))
	var installs atomic.Int32
	w.install(func([]gitops.WatchRoot, chan<- notify.EventInfo) error {
		if installs.Add(1) == 2 {
			stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 3})
		}
		return nil
	})
	sub := w.addSubscriber(gitops.GitStatus{Branch: "main"})
	w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(w.stop)

	if status := recvWithin(t, sub, 3*time.Second); status.Insertions != 3 {
		t.Fatalf("broadcast = %+v, want the status changed during the reinstall", status)
	}
	if got := w.currentWatchRoots(); len(got) != 2 {
		t.Fatalf("watch roots = %v, want the recomputed set", got)
	}
}

// A refresh whose only delta is the PR fields (the attach hook's cache
// warm-up doing its job) says nothing about watchpoint health: it must
// broadcast without reinstalling.
func TestRequestRefreshPRWarmupDoesNotReinstall(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		w.livenessQuiet = 0
	})
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	h.stub.setStatus(gitops.GitStatus{Branch: "main", OpenPRURL: "https://example.com/pr/1", OpenPRNumber: 1})
	h.w.requestRefresh()

	status := recvWithin(t, sub, 3*time.Second)
	if status.OpenPRNumber != 1 {
		t.Fatalf("broadcast = %+v, want the PR fields", status)
	}
	expectNoUpdate(t, sub, 300*time.Millisecond) // settle: no second broadcast either
	if got := h.installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (PR warm-up must not reinstall)", got)
	}
}

// While fs events are flowing, an attach-triggered refresh that sees a
// change is explained by the debounce race, not dead watches: the quiet
// window must suppress the reinstall.
func TestRequestRefreshSuppressedWhileEventsFlowing(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, nil) // production quiet window (3s)
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	// A real event lands (starting the quiet clock) and its debounced
	// refresh broadcasts the change.
	h.stub.setStatus(gitops.GitStatus{Branch: "main", Insertions: 2})
	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, "f.txt"))
	recvWithin(t, sub, 3*time.Second)

	// Immediately after, an attach observes a further change — well
	// inside the quiet window, so no reinstall may fire. (The startup
	// rebuild recompute is equality-gated and never installs.)
	h.stub.setStatus(gitops.GitStatus{Branch: "main", Insertions: 4})
	h.w.requestRefresh()
	recvWithin(t, sub, 3*time.Second)
	expectNoUpdate(t, sub, 300*time.Millisecond)
	if got := h.installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (change during event flow must not reinstall)", got)
	}
}

// The liveness ticker probes a silent watcher with the fast status fn
// and, on drift, reinstalls the watches and broadcasts the fresh truth.
func TestLivenessProbeReinstallsAfterSilentMiss(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		w.livenessInterval = 30 * time.Millisecond
	})
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	// Unchanged tree: probes run (no events ever arrived) but stay quiet.
	waitFor(t, 3*time.Second, func() bool { return h.fastCallCount() >= 2 }, "idle probes")
	expectNoUpdate(t, sub, 100*time.Millisecond)
	if got := h.installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (clean probe must not reinstall)", got)
	}

	// Tree drifts with no fs event to explain it — the dead-watch shape.
	h.stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 7})
	status := recvWithin(t, sub, 3*time.Second)
	if status.Insertions != 7 {
		t.Fatalf("broadcast = %+v, want the drifted status", status)
	}
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() >= 2 },
		"forced reinstall after probe-detected miss")
}

// A live event stream stands the probe down entirely: ticks whose
// interval saw an fs event never call the fast status fn.
func TestLivenessProbeSkipsWhileEventsFlowing(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		// Interval is 10× the feed cadence so scheduler jitter can't
		// open a probe-eligible gap mid-feed.
		w.livenessInterval = 200 * time.Millisecond
	})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	// Feed events faster than the interval for several ticks.
	stop := time.After(700 * time.Millisecond)
feed:
	for {
		select {
		case <-stop:
			break feed
		default:
			h.w.eventsCh <- writeEvent(filepath.Join(h.ws, "f.txt"))
			time.Sleep(20 * time.Millisecond)
		}
	}
	if got := h.fastCallCount(); got != 0 {
		t.Fatalf("probe ran %d times while events were flowing, want 0", got)
	}

	// Silence resumes: the probe comes back.
	waitFor(t, 3*time.Second, func() bool { return h.fastCallCount() >= 1 }, "probe after silence")
}

// A refresh whose status fetch fails is retried on the polling cadence,
// so a transient failure after the last fs event of a burst still
// reaches subscribers without another event.
func TestFailedRefreshIsRetried(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, func(w *workspaceWatcher) {
		w.pollInterval = 50 * time.Millisecond
	})
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	h.stub.setStatus(gitops.GitStatus{Branch: "main", HasChanges: true})
	h.stub.failNext.Store(true)
	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, "f.txt"))
	if got := recvWithin(t, sub, 3*time.Second); !got.HasChanges {
		t.Fatalf("retried refresh delivered %+v, want the changed status", got)
	}
	expectNoUpdate(t, sub, 200*time.Millisecond)
}

// A failed open-PR lookup is asked again at the time the status reports
// (OpenPRRecheckAt), with no filesystem event: a forge that recovers
// changes nothing on disk. Once a lookup succeeds, nothing is re-checked.
func TestFailedPRLookupIsRecheckedAtItsRecheckTime(t *testing.T) {
	t.Parallel()
	h := newLivenessHarness(t, nil)
	sub := h.w.addSubscriber(gitops.GitStatus{Branch: "main"})
	waitFor(t, 3*time.Second, func() bool { return h.installs.count() == 1 }, "initial install")

	failed := gitops.GitStatus{Branch: "main", Forge: "gitlab", OpenPRLookupError: "forge unreachable", OpenPRRecheckAt: time.Now().Add(150 * time.Millisecond)}
	h.stub.setStatus(failed)
	h.w.requestRefresh()
	if got := recvWithin(t, sub, 3*time.Second); got.OpenPRLookupError == "" {
		t.Fatalf("first refresh delivered %+v, want the failed lookup", got)
	}

	recovered := gitops.GitStatus{Branch: "main", Forge: "gitlab", OpenPRURL: "https://gitlab.com/o/r/-/merge_requests/4", OpenPRNumber: 4}
	h.stub.setStatus(recovered)
	if got := recvWithin(t, sub, 3*time.Second); got.OpenPRNumber != 4 || got.OpenPRLookupError != "" {
		t.Fatalf("re-check delivered %+v, want MR !4 with no error", got)
	}
	calls := h.stub.completedCallCount()
	expectNoUpdate(t, sub, 400*time.Millisecond)
	if got := h.stub.completedCallCount(); got != calls {
		t.Fatalf("status was read %d more times after the lookup succeeded, want none", got-calls)
	}
}

// startCountedWatcher starts a watcher over a fresh workspace with the
// given status functions, counting its installs; configure runs before
// start. The workspace is returned beside the watcher.
func startCountedWatcher(t *testing.T, statusFn, fastFn StatusFn, configure func(w *workspaceWatcher)) (*workspaceWatcher, *countingInstall, *Subscription, string) {
	t.Helper()
	ws := makeRepoDir(t)
	roots := []gitops.WatchRoot{subtreeRoot(ws)}
	w := newWorkspaceWatcher(ws, statusFn, fastFn, roots, func() ([]gitops.WatchRoot, error) { return roots, nil }, new(sequence))
	if configure != nil {
		configure(w)
	}
	installs := &countingInstall{}
	w.install(installs.fn())
	sub := w.addSubscriber(gitops.GitStatus{Branch: "main"})
	w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(w.stop)
	waitFor(t, 3*time.Second, func() bool { return installs.count() == 1 }, "initial install")
	return w, installs, sub, ws
}

// slowChangedRead is a status read that outlasts quiet and finds a
// working-tree change; during, when set, runs as it begins.
func slowChangedRead(quiet time.Duration, during func()) StatusFn {
	return func(context.Context, string) (gitops.GitStatus, error) {
		if during != nil {
			during()
		}
		time.Sleep(quiet + quiet/2)
		return gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 5}, nil
	}
}

// A requested refresh whose read outlasts the quiet window, begun just
// after an event was handled, is explained by that event: the stream was
// not quiet when it began, so the watches stay.
func TestSlowRequestedRefreshAfterAnEventDoesNotReinstall(t *testing.T) {
	t.Parallel()
	const quiet = 200 * time.Millisecond
	w, installs, sub, ws := startCountedWatcher(t, slowChangedRead(quiet, nil), nil, func(w *workspaceWatcher) {
		w.livenessQuiet = quiet
	})

	w.eventsCh <- writeEvent(filepath.Join(ws, "f.txt"))
	waitFor(t, 3*time.Second, func() bool { return len(w.eventsCh) == 0 }, "event handled")
	w.requestRefresh()
	if got := recvWithin(t, sub, 3*time.Second); got.Insertions != 5 {
		t.Fatalf("broadcast = %+v, want the change", got)
	}
	expectNoUpdate(t, sub, debounceWindow+2*quiet)
	if got := installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (an event just before the refresh explains its change)", got)
	}
}

// A requested refresh during which an event is delivered is explained by
// that event, still queued when the refresh ends: the watches stay.
func TestSlowRequestedRefreshWithAnEventDuringItDoesNotReinstall(t *testing.T) {
	t.Parallel()
	const quiet = 200 * time.Millisecond
	var w *workspaceWatcher
	var delivered atomic.Bool
	deliver := func() {
		if delivered.CompareAndSwap(false, true) {
			w.eventsCh <- writeEvent(filepath.Join(w.cwd, "f.txt"))
		}
	}
	// w is set before the run loop starts, which is what reads it.
	_, installs, sub, _ := startCountedWatcher(t, slowChangedRead(quiet, deliver), nil, func(started *workspaceWatcher) {
		w = started
		w.livenessQuiet = quiet
	})

	w.requestRefresh()
	if got := recvWithin(t, sub, 3*time.Second); got.Insertions != 5 {
		t.Fatalf("broadcast = %+v, want the change", got)
	}
	expectNoUpdate(t, sub, debounceWindow+2*quiet)
	if !delivered.Load() {
		t.Fatal("the refresh delivered no event")
	}
	if got := installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (an event during the refresh explains its change)", got)
	}
}

// A liveness probe during which an event is delivered is explained by
// that event: the drift it found is the debounced refresh's to publish.
func TestLivenessProbeWithAnEventDuringItDoesNotReinstall(t *testing.T) {
	t.Parallel()
	drifted := gitops.GitStatus{Branch: "main", HasChanges: true, Insertions: 7}
	var w *workspaceWatcher
	var probes atomic.Int32
	probe := func(context.Context, string) (gitops.GitStatus, error) {
		if probes.Add(1) == 1 {
			w.eventsCh <- writeEvent(filepath.Join(w.cwd, "f.txt"))
		}
		return drifted, nil
	}
	read := func(context.Context, string) (gitops.GitStatus, error) { return drifted, nil }
	// The interval outlasts the debounce, so the next probe compares
	// against the refresh the event caused.
	// w is set before the run loop starts, which is what reads it.
	_, installs, sub, _ := startCountedWatcher(t, read, probe, func(started *workspaceWatcher) {
		w = started
		w.livenessInterval = 400 * time.Millisecond
	})

	if got := recvWithin(t, sub, 3*time.Second); !got.Equal(drifted) {
		t.Fatalf("broadcast = %+v, want the drift from the event's refresh", got)
	}
	waitFor(t, 3*time.Second, func() bool { return probes.Load() >= 2 }, "a probe after the event's refresh")
	if got := installs.count(); got != 1 {
		t.Fatalf("installs = %d, want 1 (an event during the probe explains its drift)", got)
	}
}
