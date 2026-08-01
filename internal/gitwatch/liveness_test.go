package gitwatch

// Tests for the silent-death recovery layer: the requestRefresh
// miss-detection and the liveness probe ticker. Both exist because an
// fs-watch install can "succeed" and then never deliver (dead FSEvents
// stream after a macOS dark-wake, incident 2026-08-01), and every other
// rebuild trigger rides on fs events — a fully deaf watcher could never
// heal itself.

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
	fastFn := func(cwd string) (gitops.GitStatus, error) {
		h.fastCalls.Add(1)
		return inner(cwd)
	}
	h.w = newWorkspaceWatcher(h.ws, h.stub.fn(), fastFn, gitops.GitStatus{Branch: "main"}, roots, rootsFn)
	if configure != nil {
		configure(h.w)
	}
	h.w.start(h.installs.fn())
	t.Cleanup(h.w.stop)
	return h
}

func (h *livenessHarness) fastCallCount() int { return int(h.fastCalls.Load()) }

func TestStatusDiffersIgnoringPR(t *testing.T) {
	base := gitops.GitStatus{IsRepo: true, Branch: "main", Insertions: 3, Forge: "github"}

	prOnly := base
	prOnly.OpenPRURL = "https://example.com/pr/7"
	prOnly.OpenPRNumber = 7
	prOnly.OpenPRLookupError = "boom"
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

// A refresh whose only delta is the PR fields (the attach hook's cache
// warm-up doing its job) says nothing about watchpoint health: it must
// broadcast without reinstalling.
func TestRequestRefreshPRWarmupDoesNotReinstall(t *testing.T) {
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
