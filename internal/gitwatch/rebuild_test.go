package gitwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// fakeEvent satisfies notify.EventInfo for injecting synthetic fs
// events straight into a watcher's eventsCh.
type fakeEvent struct {
	path string
	ev   notify.Event
}

func (f fakeEvent) Event() notify.Event { return f.ev }
func (f fakeEvent) Path() string        { return f.path }
func (f fakeEvent) Sys() interface{}    { return nil }

// createEvent mimics a Create (mkdir, new file, rename target);
// writeEvent mimics content churn that can never mint a directory;
// removeEvent mimics a deletion.
func createEvent(path string) fakeEvent { return fakeEvent{path: path, ev: notify.Create} }
func writeEvent(path string) fakeEvent  { return fakeEvent{path: path, ev: notify.Write} }
func removeEvent(path string) fakeEvent { return fakeEvent{path: path, ev: notify.Remove} }

func ancestorRoot(path string) gitops.WatchRoot {
	return gitops.WatchRoot{Path: path, Recursive: false, Kind: gitops.KindAncestor}
}

func subtreeRoot(path string) gitops.WatchRoot {
	return gitops.WatchRoot{Path: path, Recursive: true, Kind: gitops.KindSubtree}
}

// rebuildHarness wires a workspaceWatcher with recording seams for the
// rebuild flow: an install fn that logs every root set it installs and
// a rootsFn whose result and error are swappable mid-test. Optional
// gates let a test hold the run loop inside statusFn or installFn to
// build deterministic interleavings.
type rebuildHarness struct {
	ws   string
	stub *stubStatus
	w    *workspaceWatcher

	mu             sync.Mutex
	installs       [][]gitops.WatchRoot
	installStarted int
	installErr     error
	installGate    chan struct{} // when non-nil, installFn blocks receiving from it
	statusGate     chan struct{} // when non-nil, statusFn blocks receiving from it
	statusEntered  int           // statusFn entries, counted before the gate
	rootsResult    []gitops.WatchRoot
	rootsErr       error
	rootsFnCalls   int
	// rootsQueue, when non-empty, answers rootsFn calls in order ahead
	// of rootsResult/rootsErr, so a test can script the recomputes one
	// rebuild makes on the run goroutine.
	rootsQueue []rootsAnswer

	// seq orders installFn returns against stopFn calls so tests can
	// pin the teardown guarantee: after stop() returns, the LAST
	// unregister must have happened after the last (re)install.
	seq                  int
	lastStopSeq          int
	lastInstallReturnSeq int
}

// rootsAnswer is one scripted rootsFn result (rebuildHarness.rootsQueue).
type rootsAnswer struct {
	roots []gitops.WatchRoot
	err   error
}

// newRebuildHarness builds the watcher and absorbs the startup
// recompute (an install leaves needsRebuild set, closing the
// compute-vs-install race): it fires a throwaway event and waits for
// the resulting refresh edge's rootsFn call, so tests measure deltas
// from a settled baseline.
func newRebuildHarness(t *testing.T, initialRoots []gitops.WatchRoot, configure ...func(*workspaceWatcher)) *rebuildHarness {
	t.Helper()
	h := &rebuildHarness{
		ws:          makeRepoDir(t),
		stub:        newStubStatus(gitops.GitStatus{Branch: "main"}),
		rootsResult: initialRoots,
	}
	rootsFn := func() ([]gitops.WatchRoot, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.rootsFnCalls++
		if len(h.rootsQueue) > 0 {
			answer := h.rootsQueue[0]
			h.rootsQueue = h.rootsQueue[1:]
			return append([]gitops.WatchRoot(nil), answer.roots...), answer.err
		}
		if h.rootsErr != nil {
			return nil, h.rootsErr
		}
		return append([]gitops.WatchRoot(nil), h.rootsResult...), nil
	}
	install := func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error {
		h.mu.Lock()
		h.installStarted++
		gate := h.installGate
		err := h.installErr
		h.mu.Unlock()
		if gate != nil {
			<-gate
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.seq++
		h.lastInstallReturnSeq = h.seq
		if err != nil {
			return err
		}
		h.installs = append(h.installs, append([]gitops.WatchRoot(nil), roots...))
		return nil
	}
	inner := h.stub.fn()
	statusFn := func(ctx context.Context, cwd string) (gitops.GitStatus, error) {
		h.mu.Lock()
		h.statusEntered++
		gate := h.statusGate
		h.mu.Unlock()
		if gate != nil {
			<-gate
		}
		return inner(ctx, cwd)
	}
	h.w = newWorkspaceWatcher(h.ws, statusFn, nil, initialRoots, rootsFn, new(sequence))
	for _, fn := range configure {
		fn(h.w)
	}
	h.w.stopFn = func(ch chan<- notify.EventInfo) {
		notify.Stop(ch)
		h.mu.Lock()
		h.seq++
		h.lastStopSeq = h.seq
		h.mu.Unlock()
	}
	h.w.install(install)
	h.w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(h.w.stop)

	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, ".startup-sync"))
	// Wait past the run loop's startup recompute and the refresh this
	// event causes, so a test arming statusGate can't accidentally gate
	// that refresh, and completedCallCount baselines start settled.
	waitFor(t, 3*time.Second, func() bool { return h.rootsCalls() == 1 },
		"startup recompute to settle")
	waitFor(t, 3*time.Second, func() bool { return h.stub.completedCallCount() >= 1 },
		"startup refresh to settle")
	return h
}

func (h *rebuildHarness) installCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.installs)
}

func (h *rebuildHarness) installStartedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.installStarted
}

// installsSince returns the root sets installed after the first n.
func (h *rebuildHarness) installsSince(n int) [][]gitops.WatchRoot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.installs[n:])
}

func (h *rebuildHarness) lastInstall() []gitops.WatchRoot {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.installs) == 0 {
		return nil
	}
	return h.installs[len(h.installs)-1]
}

func (h *rebuildHarness) rootsCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rootsFnCalls
}

func (h *rebuildHarness) statusEnteredCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusEntered
}

// finalStopAfterLastInstall reports whether the most recent unregister
// (stopFn) happened after the most recent installFn return — the
// no-leaked-watches guarantee stop() must provide.
func (h *rebuildHarness) finalStopAfterLastInstall() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastStopSeq > h.lastInstallReturnSeq
}

func (h *rebuildHarness) set(fn func(h *rebuildHarness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

// isolateGlobalGitConfig keeps the runner's global/system git config —
// most importantly a core.excludesFile that may ignore fixture dirs
// like node_modules — out of e2e pruning fixtures.
func isolateGlobalGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(d)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out after %s: %s", d, msg)
		case <-ticker.C:
		}
	}
}

// TestRebuildOnNewDirUnderAncestorRoot: a directory appearing directly
// under a KindAncestor root is covered by no existing root, so the
// watcher must recompute roots and reinstall with the new set.
func TestRebuildOnNewDirUnderAncestorRoot(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	if err := os.Mkdir(subA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{ancestorRoot(h.ws), subtreeRoot(subA)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	newDir := filepath.Join(h.ws, "b")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	grown := append(append([]gitops.WatchRoot(nil), initial...), subtreeRoot(newDir))
	h.set(func(h *rebuildHarness) { h.rootsResult = grown })

	h.w.eventsCh <- createEvent(newDir)
	waitFor(t, 3*time.Second, func() bool { return h.installCount() == 2 },
		"reinstall with grown root set")
	if got := h.lastInstall(); len(got) != 3 || got[2].Path != newDir {
		t.Fatalf("reinstalled roots = %+v, want grown set including %s", got, newDir)
	}
}

// TestNoRebuildForFileUnderAncestorRoot: a file appearing in an
// ancestor dir is covered by the non-recursive watch — refresh fires
// but the root set must not be recomputed (the Lstat dir check filters
// non-directory creates).
func TestNoRebuildForFileUnderAncestorRoot(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	file := filepath.Join(h.ws, "README.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	base := h.rootsCalls()
	before := h.stub.completedCallCount()
	h.w.eventsCh <- createEvent(file)
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != base {
		t.Fatalf("rootsFn calls went %d → %d for a plain file event, want none", base, calls)
	}
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d, want only the initial install", installs)
	}
}

// TestWriteEventOnRootPathDoesNotRebuild: non-Create churn that bubbles
// a root's own path (attrib/mtime updates from writes inside it) must
// not trigger the dead-watchpoint recovery — only Create/Rename can
// mean the root dir was recreated.
func TestWriteEventOnRootPathDoesNotRebuild(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	if err := os.Mkdir(subA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{ancestorRoot(h.ws), subtreeRoot(subA)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	base := h.rootsCalls()
	before := h.stub.completedCallCount()
	h.w.eventsCh <- writeEvent(subA)
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != base {
		t.Fatalf("rootsFn calls went %d → %d for write churn on a root path, want none", base, calls)
	}
}

// TestRootRecreateForcesReinstall is the dead-watchpoint recovery: a
// Create event whose path IS a current root means the dir was deleted
// and recreated, killing its notify watchpoint for good. The watcher
// must reinstall even though the recomputed roots are identical —
// the roots-unchanged short-circuit would otherwise leave the subtree
// permanently unwatched. The parent's watch reports the recreate as a
// Create; the root's own watch reports its deletion as a Remove, the only
// report when the recreate lands during an install before the parent's
// watch exists. The recompute after that install finds the same roots,
// so the rebuild ends there: one install and one extra scan.
func TestRootRecreateForcesReinstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		event func(path string) fakeEvent
	}{
		{name: "create", event: createEvent},
		{name: "remove", event: removeEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRebuildHarness(t, nil)
			subA := filepath.Join(h.ws, "a")
			if err := os.Mkdir(subA, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			initial := []gitops.WatchRoot{ancestorRoot(h.ws), subtreeRoot(subA)}
			h.set(func(h *rebuildHarness) { h.rootsResult = initial })
			h.w.setWatchRoots(initial)

			base := h.rootsCalls()
			before := h.stub.completedCallCount()
			h.w.eventsCh <- tc.event(subA)
			// The refresh runs after the whole rebuild.
			waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
			if installs := h.installCount(); installs != 2 {
				t.Fatalf("installs = %d, want 2 (one forced reinstall despite identical roots)", installs)
			}
			if calls := h.rootsCalls() - base; calls != 2 {
				t.Fatalf("rootsFn calls = %d, want 2 (the forced recompute and the one after its install)", calls)
			}
		})
	}
}

// TestRootRemovedForGoodDoesNotRebuild: a Remove for a root that leaves
// nothing at its path needs no rebuild; the stale root is harmless, and
// a later recreate raises a Create through the parent's watch.
func TestRootRemovedForGoodDoesNotRebuild(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	initial := []gitops.WatchRoot{ancestorRoot(h.ws), subtreeRoot(subA)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	base := h.rootsCalls()
	before := h.stub.completedCallCount()
	h.w.eventsCh <- removeEvent(subA)
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != base {
		t.Fatalf("rootsFn calls went %d -> %d for a root removed for good, want none", base, calls)
	}
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d, want only the initial install", installs)
	}
}

// retryInterval is the pollInterval the unfinished-rebuild tests run at:
// the cadence of the watcher's own rebuild retry.
const retryInterval = 50 * time.Millisecond

// expectNoMoreRootsCalls checks that a finished rebuild schedules no
// further retry: neither rootsFn nor the status read runs again over
// several retry intervals.
func expectNoMoreRootsCalls(t *testing.T, h *rebuildHarness, calls int) {
	t.Helper()
	reads := h.stub.completedCallCount()
	time.Sleep(10 * retryInterval)
	if got := h.rootsCalls(); got != calls {
		t.Fatalf("rootsFn calls went %d -> %d after the rebuild finished, want no further retry", calls, got)
	}
	if got := h.stub.completedCallCount(); got != reads {
		t.Fatalf("status reads went %d -> %d after the rebuild finished, want no further retry", reads, got)
	}
}

// TestRebuildStopsAtReinstallCap: roots that change on every recompute
// stop a rebuild after maxRebuildReinstalls reinstalls, and the watcher
// retries the unfinished rebuild itself: no event follows the trigger.
func TestRebuildStopsAtReinstallCap(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil, func(w *workspaceWatcher) { w.pollInterval = retryInterval })
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	changing := func(pass int) []gitops.WatchRoot {
		return append(slices.Clone(initial), subtreeRoot(filepath.Join(h.ws, fmt.Sprintf("d%d", pass))))
	}
	var queue []rootsAnswer
	for pass := range maxRebuildReinstalls + 1 {
		queue = append(queue, rootsAnswer{roots: changing(pass)})
	}
	base := h.rootsCalls()
	installsBefore := h.installCount()
	before := h.stub.completedCallCount()
	h.set(func(h *rebuildHarness) { h.rootsQueue = queue })
	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, ".gitignore"))

	// The rebuild reinstalls up to the cap and its last recompute still
	// differs; its refresh is the first read. The retry finds the queue
	// spent, reinstalls initial, its recompute matches, and its refresh is
	// the second.
	waitForCompletedCallCount(t, h.stub, before+2, 3*time.Second)
	var want [][]gitops.WatchRoot
	for pass := range maxRebuildReinstalls {
		want = append(want, changing(pass))
	}
	want = append(want, initial)
	if got := h.installsSince(installsBefore); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("installs = %+v, want the cap's %d then the retry's %+v", got, maxRebuildReinstalls, want)
	}
	calls := h.rootsCalls()
	if got := calls - base; got != maxRebuildReinstalls+3 {
		t.Fatalf("rootsFn calls = %d, want %d (%d for the capped rebuild, 2 for the retry)",
			got, maxRebuildReinstalls+3, maxRebuildReinstalls+1)
	}
	expectNoMoreRootsCalls(t, h, calls)
}

// TestUnfinishedRebuildRetriesWithoutAnEvent: a recompute that fails
// leaves the rebuild unfinished, and the watcher retries it itself with
// no further event. A failure after a reinstall keeps the watches it
// installed: no unregister follows that install.
func TestUnfinishedRebuildRetriesWithoutAnEvent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		queue func(grown []gitops.WatchRoot) []rootsAnswer
	}{
		{
			name:  "recompute fails before any reinstall",
			queue: func([]gitops.WatchRoot) []rootsAnswer { return []rootsAnswer{{err: errors.New("boom")}} },
		},
		{
			name: "recompute fails after a reinstall",
			queue: func(grown []gitops.WatchRoot) []rootsAnswer {
				return []rootsAnswer{{roots: grown}, {err: errors.New("boom")}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRebuildHarness(t, nil, func(w *workspaceWatcher) { w.pollInterval = retryInterval })
			initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
			grown := append(slices.Clone(initial), subtreeRoot(filepath.Join(h.ws, "b")))
			h.set(func(h *rebuildHarness) { h.rootsResult = initial })
			h.w.setWatchRoots(initial)

			base := h.rootsCalls()
			installsBefore := h.installCount()
			before := h.stub.completedCallCount()
			h.set(func(h *rebuildHarness) {
				h.rootsQueue = tc.queue(grown)
				h.rootsResult = grown
			})
			h.w.eventsCh <- writeEvent(filepath.Join(h.ws, ".gitignore"))

			// The failed rebuild's refresh, then the retry's.
			waitForCompletedCallCount(t, h.stub, before+2, 3*time.Second)
			if got := h.installsSince(installsBefore); !slices.EqualFunc(got, [][]gitops.WatchRoot{grown}, slices.Equal) {
				t.Fatalf("installs = %+v, want one of %+v", got, grown)
			}
			calls := h.rootsCalls()
			if got := calls - base; got != 3 {
				t.Fatalf("rootsFn calls = %d, want 3", got)
			}
			if h.finalStopAfterLastInstall() {
				t.Fatal("the watches were unregistered after their install; a failed recompute must keep them")
			}
			expectNoMoreRootsCalls(t, h, calls)
		})
	}
}

// TestStartupReinstallIsRechecked: a startup recompute that reinstalls is
// itself followed by a recompute, which installs the directory created
// while that reinstall had the watches out.
func TestStartupReinstallIsRechecked(t *testing.T) {
	t.Parallel()
	ws := makeRepoDir(t)
	initial := []gitops.WatchRoot{ancestorRoot(ws)}
	grown := append(slices.Clone(initial), subtreeRoot(filepath.Join(ws, "b")))
	regrown := append(slices.Clone(grown), subtreeRoot(filepath.Join(ws, "c")))
	var mu sync.Mutex
	answers := [][]gitops.WatchRoot{grown, regrown}
	var installs [][]gitops.WatchRoot
	rootsFn := func() ([]gitops.WatchRoot, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(answers) > 1 {
			next := answers[0]
			answers = answers[1:]
			return slices.Clone(next), nil
		}
		return slices.Clone(answers[0]), nil
	}
	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	w := newWorkspaceWatcher(ws, stub.fn(), nil, initial, rootsFn, new(sequence))
	w.install(func(roots []gitops.WatchRoot, _ chan<- notify.EventInfo) error {
		mu.Lock()
		defer mu.Unlock()
		installs = append(installs, slices.Clone(roots))
		return nil
	})
	w.start(gitops.GitStatus{Branch: "main"})
	t.Cleanup(w.stop)

	// The refresh runs after the whole startup rebuild.
	waitForCompletedCallCount(t, stub, 1, 3*time.Second)
	mu.Lock()
	defer mu.Unlock()
	want := [][]gitops.WatchRoot{initial, grown, regrown}
	if !slices.EqualFunc(installs, want, slices.Equal) {
		t.Fatalf("installs = %+v, want %+v", installs, want)
	}
}

// TestGitignoreEventRecomputesButSkipsIdenticalReinstall: an ignore-
// rule edit always recomputes the roots; when they come back unchanged
// the watches must be left alone (no Stop/reinstall gap).
func TestGitignoreEventRecomputesButSkipsIdenticalReinstall(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	base := h.rootsCalls()
	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, ".gitignore"))
	waitFor(t, 3*time.Second, func() bool { return h.rootsCalls() == base+1 },
		"rootsFn recompute after .gitignore event")
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d, want 1 — identical roots must not reinstall", installs)
	}
}

// TestIndexWriteTriggersRebuild: `git add -f` re-legitimizes paths
// inside a pruned ignored subtree via an index write, so an "index"
// event under a KindGitMeta root must recompute the roots. Lock churn
// (index.lock) and unrelated metadata files must not.
func TestIndexWriteTriggersRebuild(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	gitDir := filepath.Join(h.ws, ".git")
	initial := []gitops.WatchRoot{
		subtreeRoot(h.ws),
		{Path: gitDir, Recursive: false, Kind: gitops.KindGitMeta},
	}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	base := h.rootsCalls()
	before := h.stub.completedCallCount()
	h.w.eventsCh <- writeEvent(filepath.Join(gitDir, "COMMIT_EDITMSG"))
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	h.w.eventsCh <- writeEvent(filepath.Join(gitDir, "index.lock"))
	waitForCompletedCallCount(t, h.stub, before+2, 3*time.Second)
	if calls := h.rootsCalls(); calls != base {
		t.Fatalf("rootsFn calls went %d → %d for non-index metadata churn, want none", base, calls)
	}

	h.w.eventsCh <- writeEvent(filepath.Join(gitDir, "index"))
	waitFor(t, 3*time.Second, func() bool { return h.rootsCalls() == base+1 },
		"rootsFn recompute after index write")
}

// TestGlobalIgnoreFileEventTriggersRebuild: an event for a root's
// TriggerFile (the global ignore file, watched via its parent dir) must
// recompute the roots; sibling churn in the same directory must not —
// core.excludesFile commonly lives in $HOME, where unrelated writes
// (shell history, dotfiles) are routine.
func TestGlobalIgnoreFileEventTriggersRebuild(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	ignoreDir := filepath.Join(t.TempDir(), "git")
	if err := os.MkdirAll(ignoreDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{
		subtreeRoot(h.ws),
		{Path: ignoreDir, Recursive: false, Kind: gitops.KindSubtree, TriggerFile: "ignore"},
	}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	base := h.rootsCalls()
	before := h.stub.completedCallCount()
	h.w.eventsCh <- writeEvent(filepath.Join(ignoreDir, ".bash_history"))
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != base {
		t.Fatalf("rootsFn calls went %d → %d for sibling churn beside the ignore file, want none", base, calls)
	}

	h.w.eventsCh <- writeEvent(filepath.Join(ignoreDir, "ignore"))
	waitFor(t, 3*time.Second, func() bool { return h.rootsCalls() == base+1 },
		"rootsFn recompute after global-ignore edit")
}

// TestRebuildRecomputeFailureRetriesAtNextEdge: a rootsFn error keeps
// the existing (still installed) watches AND keeps the rebuild flag
// set — the tree that needs watching may never produce another event,
// so ANY later refresh edge (here: plain file churn) must retry.
func TestRebuildRecomputeFailureRetriesAtNextEdge(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) {
		h.rootsResult = initial
		h.rootsErr = errors.New("boom")
	})
	h.w.setWatchRoots(initial)

	newDir := filepath.Join(h.ws, "b")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	base := h.rootsCalls()
	before := h.stub.completedCallCount()
	h.w.eventsCh <- createEvent(newDir)
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != base+1 {
		t.Fatalf("rootsFn calls = %d, want %d (one failed recompute)", calls, base+1)
	}
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d after recompute failure, want 1 (keep existing watches)", installs)
	}

	// Recovery rides a PLAIN file event: no new boundary trigger, the
	// retained flag alone must drive the retry.
	grown := append(append([]gitops.WatchRoot(nil), initial...), subtreeRoot(newDir))
	h.set(func(h *rebuildHarness) {
		h.rootsErr = nil
		h.rootsResult = grown
	})
	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, "unrelated.txt"))
	waitFor(t, 3*time.Second, func() bool { return h.installCount() == 2 },
		"reinstall after recompute retry on a plain refresh edge")
}

// TestOverflowBurstForcesReinstall: when the event queue fills, notify
// drops events — possibly including a rebuild trigger, which rides
// individual events and would be lost for good. Draining a full queue
// must pessimistically recompute AND reinstall.
func TestOverflowBurstForcesReinstall(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	initial := []gitops.WatchRoot{subtreeRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	// Hold the run loop inside statusFn so the queue can be filled
	// deterministically behind it.
	gate := make(chan struct{})
	entered := h.statusEnteredCount()
	h.set(func(h *rebuildHarness) { h.statusGate = gate })
	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, "spark.txt"))
	waitFor(t, 3*time.Second, func() bool { return h.statusEnteredCount() > entered },
		"run loop to reach the gated statusFn")

	base := h.rootsCalls()
	installsBefore := h.installCount()
	for i := 0; i < notifyChannelSize; i++ {
		h.w.eventsCh <- writeEvent(filepath.Join(h.ws, "churn.txt"))
	}
	h.set(func(h *rebuildHarness) { h.statusGate = nil })
	close(gate)

	waitFor(t, 5*time.Second, func() bool {
		return h.rootsCalls() > base && h.installCount() > installsBefore
	}, "pessimistic recompute + forced reinstall after a full-queue drain")
}

// TestStopDuringRebuildDoesNotLeakWatches: stop() concurrent with a
// rebuild can land its unregister between the rebuild's Stop and
// reinstall — the reinstall then re-registers watches that nothing
// would ever stop. The run loop's deferred stopFn (before done closes)
// closes that hole: after stop() returns, the LAST unregister must
// have happened after the reinstall returned, which the harness pins
// via sequence numbers. Also asserts stop() blocks until the gated
// install completes and then returns promptly. Run with -race.
func TestStopDuringRebuildDoesNotLeakWatches(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	if err := os.Mkdir(subA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	gate := make(chan struct{})
	grown := append(append([]gitops.WatchRoot(nil), initial...), subtreeRoot(subA))
	startedBefore := h.installStartedCount()
	h.set(func(h *rebuildHarness) {
		h.rootsResult = grown
		h.installGate = gate
	})
	h.w.eventsCh <- createEvent(subA)
	waitFor(t, 3*time.Second, func() bool { return h.installStartedCount() > startedBefore },
		"run loop to enter the gated installFn")

	stopped := make(chan struct{})
	go func() {
		h.w.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop() returned while installFn was still blocked")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop() did not return after installFn unblocked")
	}
	if !h.finalStopAfterLastInstall() {
		t.Fatal("final unregister happened before the racing reinstall returned — " +
			"the reinstalled watches would leak for the process lifetime")
	}
}

// TestIgnoredSubtreeWritesDoNotRefresh is the end-to-end assertion the
// pruning exists for: with real WatchRoots + real notify watches on a
// real repo, writes inside an ignored subtree produce NO status
// refresh at all, while a workspace write still does.
func TestIgnoredSubtreeWritesDoNotRefresh(t *testing.T) {
	isolateGlobalGitConfig(t)
	repo := testutil.InitGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	modDir := filepath.Join(repo, "node_modules", "pkg")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	m := NewManager(ManagerConfig{
		StatusFn:     stub.fn(),
		WatchRootsFn: gitops.NewCore().WatchRoots,
	})
	t.Cleanup(m.Close)
	sub, err := m.Subscribe(t.Context(), repo)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	baseline := stub.callCount()

	// Ignored-subtree churn: no fs event reaches the watcher, so the
	// status fn is never invoked (not merely deduped).
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(modDir, "out.js"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write ignored: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	expectNoUpdate(t, sub, debounceWindow+500*time.Millisecond)
	if got := stub.callCount(); got != baseline {
		t.Fatalf("status calls went %d → %d on ignored-subtree writes, want none", baseline, got)
	}

	// Workspace write: refresh fires and the changed status is delivered.
	stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	status := recvWithin(t, sub, 5*time.Second)
	if status.FileCount != 1 {
		t.Fatalf("workspace write delivered %+v, want FileCount=1", status)
	}
}

// countingInstall wraps installNotifyWatcher so e2e tests can wait for
// a reinstall to complete before writing into a freshly (re)watched
// subtree — refresh counts alone can't distinguish "reinstalled" from
// "straggler refresh of the previous burst". With stub set it also
// records stub's completed reads at each install, for
// awaitRefreshAfterInstall.
type countingInstall struct {
	stub *stubStatus

	mu             sync.Mutex
	installs       int
	readsAtInstall int
}

func (c *countingInstall) fn() func([]gitops.WatchRoot, chan<- notify.EventInfo) error {
	return c.wrap(installNotifyWatcher)
}

// wrap counts the installs of install, a test's own sequence of real
// installNotifyWatcher calls.
func (c *countingInstall) wrap(install func([]gitops.WatchRoot, chan<- notify.EventInfo) error) func([]gitops.WatchRoot, chan<- notify.EventInfo) error {
	return func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error {
		if err := install(roots, ch); err != nil {
			return err
		}
		c.mu.Lock()
		c.installs++
		if c.stub != nil {
			c.readsAtInstall = c.stub.completedCallCount()
		}
		c.mu.Unlock()
		return nil
	}
}

func (c *countingInstall) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.installs
}

// awaitRefreshAfterInstall waits for a status read to complete after the
// latest install: the refresh that follows a reinstall on the run loop. A
// status the test sets after it reaches a subscriber only through a later
// read, which an event from the installed watches has to cause.
func (c *countingInstall) awaitRefreshAfterInstall(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	reads := c.readsAtInstall
	c.mu.Unlock()
	waitForCompletedCallCount(t, c.stub, reads+1, 5*time.Second)
}

// TestUnignoreRestoresWatch is the recovery mirror of
// TestIgnoredSubtreeWritesDoNotRefresh: removing the ignore rule must
// reinstall watches so writes inside the previously pruned subtree
// refresh status again — proving the .gitignore trigger works against
// real notify events and the real WatchRoots pipeline.
func TestUnignoreRestoresWatch(t *testing.T) {
	isolateGlobalGitConfig(t)
	repo := testutil.InitGitRepo(t)
	gitignore := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	modDir := filepath.Join(repo, "node_modules", "pkg")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	installs := &countingInstall{stub: stub}
	m := NewManager(ManagerConfig{
		StatusFn:     stub.fn(),
		WatchRootsFn: gitops.NewCore().WatchRoots,
	})
	m.installFn = installs.fn()
	t.Cleanup(m.Close)
	sub, err := m.Subscribe(t.Context(), repo)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	// Pruned: writes inside node_modules must not reach the watcher.
	if err := os.WriteFile(filepath.Join(modDir, "a.js"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	expectNoUpdate(t, sub, debounceWindow+500*time.Millisecond)

	// Un-ignore: the .gitignore edit is a real event under the watched
	// workspace, flags a rebuild, and the recomputed roots now cover
	// node_modules — wait for the reinstall to land.
	if err := os.WriteFile(gitignore, []byte("\n"), 0o644); err != nil {
		t.Fatalf("rewrite .gitignore: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return installs.count() >= 2 },
		"reinstall after un-ignoring node_modules")
	installs.awaitRefreshAfterInstall(t)

	stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
	if err := os.WriteFile(filepath.Join(modDir, "b.js"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write formerly ignored: %v", err)
	}
	status := recvWithin(t, sub, 5*time.Second)
	if status.FileCount != 1 {
		t.Fatalf("write in un-ignored subtree delivered %+v, want FileCount=1", status)
	}
}

// gatedStartupRoots holds a Manager watcher's startup root recompute,
// the second WatchRootsFn call (Subscribe's install makes the first),
// until release closes, and closes done when that recompute returns. A
// test pins a filesystem change before or after the recompute with it.
type gatedStartupRoots struct {
	calls   atomic.Int32
	release chan struct{}
	once    sync.Once
	done    chan struct{}
}

func newGatedStartupRoots() *gatedStartupRoots {
	return &gatedStartupRoots{release: make(chan struct{}), done: make(chan struct{})}
}

func (g *gatedStartupRoots) fn(inner WatchRootsFn) WatchRootsFn {
	return func(cwd string) ([]gitops.WatchRoot, error) {
		if g.calls.Add(1) != 2 {
			return inner(cwd)
		}
		<-g.release
		defer close(g.done)
		return inner(cwd)
	}
}

// open lets the held recompute run. Idempotent, so a test can also
// defer it to keep teardown from waiting on a recompute it never let go.
func (g *gatedStartupRoots) open() { g.once.Do(func() { close(g.release) }) }

// TestRecreatedSubtreeRegainsWatch: deleting and recreating a watched
// subtree root (rm -rf src && restore, routine for build tools and
// agents) kills its notify watchpoint permanently, and writes inside the
// recreated tree must refresh status again. A deletion raises no rebuild,
// so the root set the recreate meets depends on whether the watcher's
// startup recompute ran before the deletion. Both orders are pinned:
//   - deleted before it: the recompute drops src, and the recreate is a
//     new directory under the repo's ancestor root;
//   - deleted after it: src is still a root, and its recreate forces a
//     reinstall of identical roots.
func TestRecreatedSubtreeRegainsWatch(t *testing.T) {
	isolateGlobalGitConfig(t)
	for _, tc := range []struct {
		name                  string
		deleteBeforeRecompute bool
		// installs counts every install once the recreate's has landed.
		installs int
	}{
		{name: "deleted before the startup recompute", deleteBeforeRecompute: true, installs: 3},
		{name: "deleted after the startup recompute", deleteBeforeRecompute: false, installs: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.InitGitRepo(t)
			if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
				t.Fatalf("write .gitignore: %v", err)
			}
			// node_modules forces the pruned layout: repo root becomes a
			// non-recursive ancestor root and src its own recursive root.
			for _, dir := range []string{"node_modules/pkg", "src"} {
				if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			if err := os.WriteFile(filepath.Join(repo, "src", "f.txt"), []byte("x"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			stub := newStubStatus(gitops.GitStatus{Branch: "main"})
			installs := &countingInstall{stub: stub}
			roots := newGatedStartupRoots()
			m := NewManager(ManagerConfig{
				StatusFn:     stub.fn(),
				WatchRootsFn: roots.fn(gitops.NewCore().WatchRoots),
			})
			m.installFn = installs.fn()
			t.Cleanup(m.Close)
			sub, err := m.Subscribe(t.Context(), repo)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			t.Cleanup(sub.Close)
			t.Cleanup(roots.open) // runs before sub.Close stops the run loop

			srcDir := filepath.Join(repo, "src")
			if !tc.deleteBeforeRecompute {
				roots.open()
				select {
				case <-roots.done:
				case <-time.After(5 * time.Second):
					t.Fatal("timed out after 5s: startup recompute")
				}
			}
			if err := os.RemoveAll(srcDir); err != nil {
				t.Fatalf("rm -rf src: %v", err)
			}
			if tc.deleteBeforeRecompute {
				roots.open()
				// The recompute finds src gone and reinstalls without it, so
				// the recreate below meets the ancestor root alone.
				waitFor(t, 5*time.Second, func() bool { return installs.count() >= 2 },
					"reinstall without the deleted src root")
			}
			if err := os.Mkdir(srcDir, 0o755); err != nil {
				t.Fatalf("recreate src: %v", err)
			}
			waitFor(t, 5*time.Second, func() bool { return installs.count() >= tc.installs },
				"reinstall after src recreate")
			installs.awaitRefreshAfterInstall(t)

			stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
			if err := os.WriteFile(filepath.Join(srcDir, "g.txt"), []byte("y"), 0o644); err != nil {
				t.Fatalf("write recreated: %v", err)
			}
			status := recvWithin(t, sub, 5*time.Second)
			if status.FileCount != 1 {
				t.Fatalf("write in recreated subtree delivered %+v, want FileCount=1", status)
			}
		})
	}
}

// TestDirCreatedDuringReinstallIsWatched: a directory created under the
// repo's ancestor root while a rebuild has the watches out (after its
// unregister, before its install) raises no event. The recompute after
// that install must find it and install it, and a write inside it must
// refresh status. The rebuild's install is held so the directory lands
// inside that window every run.
func TestDirCreatedDuringReinstallIsWatched(t *testing.T) {
	isolateGlobalGitConfig(t)
	repo := testutil.InitGitRepo(t)
	gitignore := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	for _, dir := range []string{"node_modules/pkg", "src"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	installs := &countingInstall{stub: stub}
	roots := newGatedStartupRoots()
	m := NewManager(ManagerConfig{
		StatusFn:     stub.fn(),
		WatchRootsFn: roots.fn(gitops.NewCore().WatchRoots),
	})
	// Install 1 is Subscribe's; the startup recompute matches it, so
	// install 2 is the rebuild's, called after its unregister.
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	open := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	counted := installs.fn()
	m.installFn = func(r []gitops.WatchRoot, ch chan<- notify.EventInfo) error {
		if calls.Add(1) == 2 {
			close(held)
			<-release
		}
		return counted(r, ch)
	}
	t.Cleanup(m.Close)
	sub, err := m.Subscribe(t.Context(), repo)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	t.Cleanup(roots.open) // both run before sub.Close stops the run loop
	t.Cleanup(open)

	roots.open()
	select {
	case <-roots.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out after 5s: startup recompute")
	}
	// Un-ignoring node_modules changes the roots, so the rebuild reinstalls.
	if err := os.WriteFile(gitignore, []byte("\n"), 0o644); err != nil {
		t.Fatalf("rewrite .gitignore: %v", err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out after 5s: rebuild reaching its install")
	}
	libDir := filepath.Join(repo, "lib")
	if err := os.Mkdir(libDir, 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	open()
	waitFor(t, 5*time.Second, func() bool { return installs.count() >= 3 },
		"reinstall covering the directory created while the watches were out")
	installs.awaitRefreshAfterInstall(t)

	stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
	if err := os.WriteFile(filepath.Join(libDir, "x.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write in lib: %v", err)
	}
	status := recvWithin(t, sub, 5*time.Second)
	if status.FileCount != 1 {
		t.Fatalf("write in lib delivered %+v, want FileCount=1", status)
	}
}

// TestRootReplacedDuringInstallIsRewatched: roots install deepest first,
// so a recursive root is registered before its non-recursive ancestor.
// Deleting and recreating the root between the two kills the root's
// watch, and its parent is not watched yet to report the recreate; the
// recompute after the install finds the same paths. The install must
// notice that the root is no longer the directory it watched and
// reinstall, so a write in the new directory refreshes status. The test
// splits the watcher's first install around the replacement.
func TestRootReplacedDuringInstallIsRewatched(t *testing.T) {
	isolateGlobalGitConfig(t)
	// Canonical, as the installed roots are: the install below picks the
	// src root out by path, and a temporary directory can sit behind a
	// symlink.
	repo := testutil.CanonicalPath(t, testutil.InitGitRepo(t))
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	for _, dir := range []string{"node_modules/pkg", "src"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	srcDir := filepath.Join(repo, "src")
	if err := os.WriteFile(filepath.Join(srcDir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	stub := newStubStatus(gitops.GitStatus{Branch: "main"})
	installs := &countingInstall{stub: stub}
	var calls atomic.Int32
	m := NewManager(ManagerConfig{StatusFn: stub.fn(), WatchRootsFn: gitops.NewCore().WatchRoots})
	m.installFn = installs.wrap(func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error {
		if calls.Add(1) != 1 {
			return installNotifyWatcher(roots, ch)
		}
		var src, rest []gitops.WatchRoot
		for _, root := range roots {
			if root.Path == srcDir {
				src = append(src, root)
			} else {
				rest = append(rest, root)
			}
		}
		if len(src) != 1 || !src[0].Recursive {
			return fmt.Errorf("first install roots %+v carry no recursive %s root", roots, srcDir)
		}
		if err := installNotifyWatcher(src, ch); err != nil {
			return err
		}
		if err := os.RemoveAll(srcDir); err != nil {
			return fmt.Errorf("rm -rf src: %w", err)
		}
		if err := os.Mkdir(srcDir, 0o755); err != nil {
			return fmt.Errorf("recreate src: %w", err)
		}
		return installNotifyWatcher(rest, ch)
	})
	t.Cleanup(m.Close)
	sub, err := m.Subscribe(t.Context(), repo)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	waitFor(t, 5*time.Second, func() bool { return installs.count() >= 2 },
		"reinstall of the root replaced during the install")
	installs.awaitRefreshAfterInstall(t)

	stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
	if err := os.WriteFile(filepath.Join(srcDir, "g.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write in recreated src: %v", err)
	}
	status := recvWithin(t, sub, 5*time.Second)
	if status.FileCount != 1 {
		t.Fatalf("write in recreated src delivered %+v, want FileCount=1", status)
	}
}

// TestReinstallFailureFallsBackToPolling: a rebuild whose reinstall
// fails leaves the watcher with no fs watches — it must escalate to
// interval polling so the status stream stays alive.
func TestReinstallFailureFallsBackToPolling(t *testing.T) {
	t.Parallel()
	h := newRebuildHarness(t, nil, func(w *workspaceWatcher) { w.pollInterval = 50 * time.Millisecond })
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	newDir := filepath.Join(h.ws, "b")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	grown := append(append([]gitops.WatchRoot(nil), initial...), subtreeRoot(newDir))
	h.set(func(h *rebuildHarness) {
		h.rootsResult = grown
		h.installErr = errors.New("watch limit exhausted")
	})

	before := h.stub.completedCallCount()
	h.w.eventsCh <- createEvent(newDir)
	// The refresh at the rebuild edge still runs (+1); after that, with
	// zero further events, only the polling ticker can drive more
	// refreshes — observing one proves the fallback engaged.
	waitForCompletedCallCount(t, h.stub, before+2, 3*time.Second)
}
