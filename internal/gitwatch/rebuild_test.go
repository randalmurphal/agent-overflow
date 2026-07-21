package gitwatch

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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
// writeEvent mimics content churn that can never mint a directory.
func createEvent(path string) fakeEvent { return fakeEvent{path: path, ev: notify.Create} }
func writeEvent(path string) fakeEvent  { return fakeEvent{path: path, ev: notify.Write} }

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

	// seq orders installFn returns against stopFn calls so tests can
	// pin the teardown guarantee: after stop() returns, the LAST
	// unregister must have happened after the last (re)install.
	seq                  int
	lastStopSeq          int
	lastInstallReturnSeq int
}

// newRebuildHarness builds the watcher and absorbs the startup
// recompute (watchers start with needsRebuild set to close the
// subscribe-vs-install race): it fires a throwaway event and waits for
// the resulting refresh edge's rootsFn call, so tests measure deltas
// from a settled baseline.
func newRebuildHarness(t *testing.T, initialRoots []gitops.WatchRoot) *rebuildHarness {
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
	statusFn := func(cwd string) (gitops.GitStatus, error) {
		h.mu.Lock()
		h.statusEntered++
		gate := h.statusGate
		h.mu.Unlock()
		if gate != nil {
			<-gate
		}
		return inner(cwd)
	}
	h.w = newWorkspaceWatcher(h.ws, statusFn, gitops.GitStatus{Branch: "main"}, initialRoots, rootsFn)
	h.w.stopFn = func(ch chan<- notify.EventInfo) {
		notify.Stop(ch)
		h.mu.Lock()
		h.seq++
		h.lastStopSeq = h.seq
		h.mu.Unlock()
	}
	h.w.start(install)
	t.Cleanup(h.w.stop)

	h.w.eventsCh <- writeEvent(filepath.Join(h.ws, ".startup-sync"))
	// Wait past the whole startup edge — recompute AND its refresh —
	// so a test arming statusGate can't accidentally gate the startup
	// refresh, and completedCallCount baselines start settled.
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
// permanently unwatched.
func TestRootRecreateForcesReinstall(t *testing.T) {
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	if err := os.Mkdir(subA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{ancestorRoot(h.ws), subtreeRoot(subA)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	h.w.eventsCh <- createEvent(subA)
	waitFor(t, 3*time.Second, func() bool { return h.installCount() == 2 },
		"forced reinstall after root dir recreate despite identical roots")
}

// TestGitignoreEventRecomputesButSkipsIdenticalReinstall: an ignore-
// rule edit always recomputes the roots; when they come back unchanged
// the watches must be left alone (no Stop/reinstall gap).
func TestGitignoreEventRecomputesButSkipsIdenticalReinstall(t *testing.T) {
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
	sub, err := m.Subscribe(repo)
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
// "straggler refresh of the previous burst".
type countingInstall struct {
	mu       sync.Mutex
	installs int
}

func (c *countingInstall) fn() func([]gitops.WatchRoot, chan<- notify.EventInfo) error {
	return func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error {
		if err := installNotifyWatcher(roots, ch); err != nil {
			return err
		}
		c.mu.Lock()
		c.installs++
		c.mu.Unlock()
		return nil
	}
}

func (c *countingInstall) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.installs
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
	installs := &countingInstall{}
	m := NewManager(ManagerConfig{
		StatusFn:     stub.fn(),
		WatchRootsFn: gitops.NewCore().WatchRoots,
	})
	m.installFn = installs.fn()
	t.Cleanup(m.Close)
	sub, err := m.Subscribe(repo)
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

	stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
	if err := os.WriteFile(filepath.Join(modDir, "b.js"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write formerly ignored: %v", err)
	}
	status := recvWithin(t, sub, 5*time.Second)
	if status.FileCount != 1 {
		t.Fatalf("write in un-ignored subtree delivered %+v, want FileCount=1", status)
	}
}

// TestRecreatedSubtreeRegainsWatch: deleting and recreating a watched
// subtree root (rm -rf src && restore — routine for build tools and
// agents) kills its notify watchpoint permanently. The recreate event
// arrives via the parent ancestor root and must force a reinstall so
// writes inside the recreated tree refresh status again.
func TestRecreatedSubtreeRegainsWatch(t *testing.T) {
	isolateGlobalGitConfig(t)
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
	installs := &countingInstall{}
	m := NewManager(ManagerConfig{
		StatusFn:     stub.fn(),
		WatchRootsFn: gitops.NewCore().WatchRoots,
	})
	m.installFn = installs.fn()
	t.Cleanup(m.Close)
	sub, err := m.Subscribe(repo)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	srcDir := filepath.Join(repo, "src")
	if err := os.RemoveAll(srcDir); err != nil {
		t.Fatalf("rm -rf src: %v", err)
	}
	// The deletion edge recomputes (watchers start rebuild-flagged) and
	// reinstalls without the vanished src root. Waiting for it keeps the
	// recreate below unambiguous: its Create event then triggers the
	// new-dir-under-ancestor rebuild that must restore the src watch.
	// (The recreate-while-still-a-root force path is unit-covered by
	// TestRootRecreateForcesReinstall.)
	waitFor(t, 5*time.Second, func() bool { return installs.count() >= 2 },
		"reinstall after src deletion")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatalf("recreate src: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return installs.count() >= 3 },
		"reinstall after src recreate")

	stub.setStatus(gitops.GitStatus{Branch: "main", FileCount: 1})
	if err := os.WriteFile(filepath.Join(srcDir, "g.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write recreated: %v", err)
	}
	status := recvWithin(t, sub, 5*time.Second)
	if status.FileCount != 1 {
		t.Fatalf("write in recreated subtree delivered %+v, want FileCount=1", status)
	}
}

// TestReinstallFailureFallsBackToPolling: a rebuild whose reinstall
// fails leaves the watcher with no fs watches — it must escalate to
// interval polling so the status stream stays alive.
func TestReinstallFailureFallsBackToPolling(t *testing.T) {
	h := newRebuildHarness(t, nil)
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
	waitForCompletedCallCount(t, h.stub, before+2, pollFallbackInterval+2*time.Second)
}
