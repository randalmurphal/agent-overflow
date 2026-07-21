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
type fakeEvent struct{ path string }

func (f fakeEvent) Event() notify.Event { return notify.Create }
func (f fakeEvent) Path() string        { return f.path }
func (f fakeEvent) Sys() interface{}    { return nil }

// rebuildHarness wires a workspaceWatcher with recording seams for the
// rebuild flow: an install fn that logs every root set it installs and
// a rootsFn whose result and error are swappable mid-test.
type rebuildHarness struct {
	ws   string
	stub *stubStatus
	w    *workspaceWatcher

	mu           sync.Mutex
	installs     [][]gitops.WatchRoot
	installErr   error
	rootsResult  []gitops.WatchRoot
	rootsErr     error
	rootsFnCalls int
}

func newRebuildHarness(t *testing.T, initialRoots []gitops.WatchRoot) *rebuildHarness {
	t.Helper()
	h := &rebuildHarness{
		ws:          makeRepoDir(t),
		stub:        newStubStatus(gitops.GitStatus{Branch: "main"}),
		rootsResult: initialRoots,
	}
	// Resolve the workspace path the way production roots arrive
	// (canonicalized); initialRoots passed by tests use h.ws already.
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
		defer h.mu.Unlock()
		if h.installErr != nil {
			return h.installErr
		}
		h.installs = append(h.installs, append([]gitops.WatchRoot(nil), roots...))
		return nil
	}
	h.w = newWorkspaceWatcher(h.ws, h.stub.fn(), gitops.GitStatus{Branch: "main"}, initialRoots, rootsFn)
	h.w.start(install)
	t.Cleanup(h.w.stop)
	return h
}

func (h *rebuildHarness) installCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.installs)
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

func (h *rebuildHarness) set(fn func(h *rebuildHarness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
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

func ancestorRoot(path string) gitops.WatchRoot {
	return gitops.WatchRoot{Path: path, Recursive: false, RebuildOnChildDir: true}
}

// TestRebuildOnNewDirUnderAncestorRoot: a directory appearing directly
// under a RebuildOnChildDir root is covered by no existing root, so the
// watcher must recompute roots and reinstall with the new set.
func TestRebuildOnNewDirUnderAncestorRoot(t *testing.T) {
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	if err := os.Mkdir(subA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{ancestorRoot(h.ws), {Path: subA, Recursive: true}}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	newDir := filepath.Join(h.ws, "b")
	if err := os.Mkdir(newDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	grown := append(append([]gitops.WatchRoot(nil), initial...), gitops.WatchRoot{Path: newDir, Recursive: true})
	h.set(func(h *rebuildHarness) { h.rootsResult = grown })

	h.w.eventsCh <- fakeEvent{newDir}
	waitFor(t, 3*time.Second, func() bool { return h.installCount() == 2 },
		"reinstall with grown root set")
	if got := h.lastInstall(); len(got) != 3 || got[2].Path != newDir {
		t.Fatalf("reinstalled roots = %+v, want grown set including %s", got, newDir)
	}
}

// TestNoRebuildForFileUnderAncestorRoot: plain file churn in an
// ancestor dir is covered by the non-recursive watch — refresh fires
// but the root set must not be recomputed.
func TestNoRebuildForFileUnderAncestorRoot(t *testing.T) {
	h := newRebuildHarness(t, nil)
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	file := filepath.Join(h.ws, "README.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := h.stub.completedCallCount()
	h.w.eventsCh <- fakeEvent{file}
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != 0 {
		t.Fatalf("rootsFn called %d times for a plain file event, want 0", calls)
	}
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d, want only the initial install", installs)
	}
}

// TestNoRebuildForAlreadyWatchedDir: an event for a directory that is
// itself a current root (churn inside it bubbles its own path) must
// not trigger recompute.
func TestNoRebuildForAlreadyWatchedDir(t *testing.T) {
	h := newRebuildHarness(t, nil)
	subA := filepath.Join(h.ws, "a")
	if err := os.Mkdir(subA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := []gitops.WatchRoot{ancestorRoot(h.ws), {Path: subA, Recursive: true}}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	before := h.stub.completedCallCount()
	h.w.eventsCh <- fakeEvent{subA}
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if calls := h.rootsCalls(); calls != 0 {
		t.Fatalf("rootsFn called %d times for an already-watched dir, want 0", calls)
	}
}

// TestGitignoreEventRecomputesButSkipsIdenticalReinstall: an ignore-
// rule edit always recomputes the roots; when they come back unchanged
// the watches must be left alone (no Stop/reinstall gap).
func TestGitignoreEventRecomputesButSkipsIdenticalReinstall(t *testing.T) {
	h := newRebuildHarness(t, nil)
	initial := []gitops.WatchRoot{ancestorRoot(h.ws)}
	h.set(func(h *rebuildHarness) { h.rootsResult = initial })
	h.w.setWatchRoots(initial)

	h.w.eventsCh <- fakeEvent{filepath.Join(h.ws, ".gitignore")}
	waitFor(t, 3*time.Second, func() bool { return h.rootsCalls() == 1 },
		"rootsFn recompute after .gitignore event")
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d, want 1 — identical roots must not reinstall", installs)
	}
}

// TestRebuildRecomputeFailureKeepsWatching: a rootsFn error keeps the
// existing (still installed) watches and the stream alive; a later
// boundary event retries and succeeds.
func TestRebuildRecomputeFailureKeepsWatching(t *testing.T) {
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
	before := h.stub.completedCallCount()
	h.w.eventsCh <- fakeEvent{newDir}
	waitForCompletedCallCount(t, h.stub, before+1, 3*time.Second)
	if installs := h.installCount(); installs != 1 {
		t.Fatalf("installs = %d after recompute failure, want 1 (keep existing watches)", installs)
	}

	// Recovery: clear the error, fire another boundary event.
	grown := append(append([]gitops.WatchRoot(nil), initial...), gitops.WatchRoot{Path: newDir, Recursive: true})
	h.set(func(h *rebuildHarness) {
		h.rootsErr = nil
		h.rootsResult = grown
	})
	h.w.eventsCh <- fakeEvent{newDir}
	waitFor(t, 3*time.Second, func() bool { return h.installCount() == 2 },
		"reinstall after recompute recovery")
}

// TestIgnoredSubtreeWritesDoNotRefresh is the end-to-end assertion the
// pruning exists for: with real WatchRoots + real notify watches on a
// real repo, writes inside an ignored subtree produce NO status
// refresh at all, while a workspace write still does.
func TestIgnoredSubtreeWritesDoNotRefresh(t *testing.T) {
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
	grown := append(append([]gitops.WatchRoot(nil), initial...), gitops.WatchRoot{Path: newDir, Recursive: true})
	h.set(func(h *rebuildHarness) {
		h.rootsResult = grown
		h.installErr = errors.New("watch limit exhausted")
	})

	before := h.stub.completedCallCount()
	h.w.eventsCh <- fakeEvent{newDir}
	// The refresh at the rebuild edge still runs (+1); after that, with
	// zero further events, only the polling ticker can drive more
	// refreshes — observing one proves the fallback engaged.
	waitForCompletedCallCount(t, h.stub, before+2, pollFallbackInterval+2*time.Second)
}
