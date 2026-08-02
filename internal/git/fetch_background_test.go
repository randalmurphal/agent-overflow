package git

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// countingFetch replaces Core.fetchFn with a counter, returning the
// recorded call count. Single-flight and worktree dedup are invisible
// from the outside otherwise: both look like "a fetch happened".
func countingFetch(core *Core) *atomic.Int64 {
	var calls atomic.Int64
	core.fetchFn = func(context.Context, string) error {
		calls.Add(1)
		return nil
	}
	return &calls
}

func TestBackgroundFetchDedupsWorktreesOntoTheCommonDir(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	testutil.RunGit(t, repo, "branch", "feature/background-fetch")
	worktree := filepath.Join(t.TempDir(), "feature-background-fetch")
	testutil.RunGit(t, repo, "worktree", "add", worktree, "feature/background-fetch")

	core := NewCore()
	calls := countingFetch(core)

	fetched, err := core.FetchRemotesBackground(t.Context(), repo)
	if err != nil {
		t.Fatalf("fetch from primary checkout: %v", err)
	}
	if !fetched {
		t.Fatal("first fetch should run (no cached timestamp)")
	}

	// The worktree is a different path with a different repo root, but
	// the same refs on disk. Keying the throttle on anything other than
	// the common dir fetches the same repository twice.
	fetched, err = core.FetchRemotesBackground(t.Context(), worktree)
	if err != nil {
		t.Fatalf("fetch from linked worktree: %v", err)
	}
	if fetched {
		t.Fatal("linked worktree must share the primary checkout's fetch window")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("git fetch ran %d times across two worktrees of one repo, want 1", got)
	}
}

func TestBackgroundFetchSingleFlightsConcurrentCallers(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	core.fetchFn = func(context.Context, string) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = core.FetchRemotesBackground(t.Context(), repo)
		}()
	}

	// Hold the first flight open until it is provably running, so the
	// second caller has to decide what to do about an in-flight fetch
	// rather than racing to a decision the throttle already made.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first fetch to start")
	}
	// The second caller must be inside Do (joined or blocked) before the
	// first completes; give it a moment and then release both.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !results[i] {
			t.Fatalf("caller %d reported no fetch; both callers share the one flight's result", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("git fetch ran %d times for concurrent callers, want 1", got)
	}
}

func TestBackgroundFetchSkipsRepoWithoutOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)

	core := NewCore()
	calls := countingFetch(core)

	fetched, err := core.FetchRemotesBackground(t.Context(), repo)
	if err != nil {
		t.Fatalf("FetchRemotesBackground: %v", err)
	}
	if fetched {
		t.Fatal("a repo with no remote must not be fetched")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("git fetch ran %d times against a remote-less repo, want 0", got)
	}

	// And the skip must not stamp the clock: adding a remote later has
	// to be picked up by the very next tick.
	_, bare := repoWithOrigin(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", bare)
	fetched, err = core.FetchRemotesBackground(t.Context(), repo)
	if err != nil {
		t.Fatalf("FetchRemotesBackground after adding origin: %v", err)
	}
	if !fetched {
		t.Fatal("a remote added after a skip must be fetched on the next call")
	}
}

func TestBackgroundFetchRespectsAndSharesTheStaleWindow(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	calls := countingFetch(core)
	start := time.Unix(20_000, 0)
	now := start
	core.nowFn = func() time.Time { return now }

	if fetched, err := core.FetchRemotesBackground(t.Context(), repo); err != nil || !fetched {
		t.Fatalf("first fetch: fetched=%v err=%v", fetched, err)
	}

	now = start.Add(FetchStaleWindow - time.Nanosecond)
	if fetched, err := core.FetchRemotesBackground(t.Context(), repo); err != nil || fetched {
		t.Fatalf("just under the window: fetched=%v err=%v, want skip", fetched, err)
	}
	// The picker's warm-up shares the same clock — the background tick
	// must not make the picker re-fetch, or vice versa.
	if fetched, err := core.MaybeFetchRemotes(repo); err != nil || fetched {
		t.Fatalf("picker warm-up inside the window: fetched=%v err=%v, want skip", fetched, err)
	}

	now = start.Add(FetchStaleWindow)
	if fetched, err := core.FetchRemotesBackground(t.Context(), repo); err != nil || !fetched {
		t.Fatalf("at the window boundary: fetched=%v err=%v, want fetch", fetched, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("git fetch ran %d times, want 2", got)
	}
}

func TestBackgroundFetchFailureLeavesTheClockUnstamped(t *testing.T) {
	repo, _ := repoWithOrigin(t)

	core := NewCore()
	var calls atomic.Int64
	failure := errors.New("remote hung up")
	core.fetchFn = func(context.Context, string) error {
		calls.Add(1)
		return failure
	}

	if _, err := core.FetchRemotesBackground(t.Context(), repo); !errors.Is(err, failure) {
		t.Fatalf("FetchRemotesBackground error = %v, want %v", err, failure)
	}
	// A failed fetch must not look like a fresh one: the next tick has
	// to retry, not sit out the window on the strength of a failure.
	if _, err := core.FetchRemotesBackground(t.Context(), repo); !errors.Is(err, failure) {
		t.Fatalf("second FetchRemotesBackground error = %v, want %v", err, failure)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("git fetch ran %d times after a failure, want 2 (retry, not throttle)", got)
	}
}

func TestBackgroundFetchRejectsNonRepo(t *testing.T) {
	core := NewCore()
	calls := countingFetch(core)

	if _, err := core.FetchRemotesBackground(t.Context(), t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory that is not a git repository")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("git fetch ran %d times outside a repository, want 0", got)
	}
}

// TestBackgroundFetchMovesBehindCount is the end-to-end shape of the
// feature: a real `git fetch` against a real (local) remote, and the
// status the sidebar renders moving from stale to correct without any
// user action.
func TestBackgroundFetchMovesBehindCount(t *testing.T) {
	repo, bare := repoWithOrigin(t)
	advanceOriginMain(t, bare)

	core := NewCore()

	before, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast before: %v", err)
	}
	if before.BehindCount != 0 {
		t.Fatalf("BehindCount before fetch = %d, want 0 (remote-tracking refs still stale)", before.BehindCount)
	}

	fetched, err := core.FetchRemotesBackground(t.Context(), repo)
	if err != nil {
		t.Fatalf("FetchRemotesBackground: %v", err)
	}
	if !fetched {
		t.Fatal("expected the background fetch to run")
	}

	after, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast after: %v", err)
	}
	if after.BehindCount != 1 {
		t.Fatalf("BehindCount after fetch = %d, want 1", after.BehindCount)
	}
}

func TestCommonDirIsSharedAcrossWorktreesAndCached(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/common-dir")
	worktree := filepath.Join(t.TempDir(), "feature-common-dir")
	testutil.RunGit(t, repo, "worktree", "add", worktree, "feature/common-dir")

	core := NewCore()
	primary, err := core.CommonDir(repo)
	if err != nil {
		t.Fatalf("CommonDir(primary): %v", err)
	}
	linked, err := core.CommonDir(worktree)
	if err != nil {
		t.Fatalf("CommonDir(worktree): %v", err)
	}
	if primary != linked {
		t.Fatalf("common dir differs between worktrees: %q vs %q", primary, linked)
	}
	if want := CanonicalPath(filepath.Join(repo, ".git")); primary != want {
		t.Fatalf("CommonDir = %q, want %q", primary, want)
	}

	// The linked worktree's own git dir is NOT the common dir — that is
	// the whole reason this helper exists rather than resolveGitDir.
	if gitDir := core.resolveGitDir(worktree); CanonicalPath(gitDir) == linked {
		t.Fatalf("worktree git dir %q must differ from the common dir %q", gitDir, linked)
	}

	core.commonDirCacheMu.RLock()
	_, cached := core.commonDirCache[repo]
	core.commonDirCacheMu.RUnlock()
	if !cached {
		t.Fatal("expected a successful CommonDir resolution to be memoized")
	}
}

func TestCommonDirDoesNotCacheFailures(t *testing.T) {
	dir := t.TempDir()
	core := NewCore()

	if _, err := core.CommonDir(dir); err == nil {
		t.Fatal("expected an error outside a repository")
	}
	core.commonDirCacheMu.RLock()
	entries := len(core.commonDirCache)
	core.commonDirCacheMu.RUnlock()
	if entries != 0 {
		t.Fatalf("failure cached %d entries; a later `git init` would never be seen", entries)
	}

	testutil.RunGit(t, dir, "init", "-b", "main")
	if _, err := core.CommonDir(dir); err != nil {
		t.Fatalf("CommonDir after init: %v", err)
	}
}

// TestBackgroundFetchAbortsOnCancelledContext pins the shutdown
// property: the cadence's owner cancels, and the fetch is over. Without
// it, teardown would have to wait out the subprocess timeout for a
// repository whose remote is hanging.
func TestBackgroundFetchAbortsOnCancelledContext(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	core := NewCore()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := core.FetchRemotesBackground(ctx, repo)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a cancelled fetch to report an error, not a silent success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled background fetch did not return")
	}

	// A cancelled fetch is not a successful one: the window must stay
	// open so the next tick (in a live app) still refreshes.
	if core.fetchIsFresh(mustCommonDir(t, core, repo)) {
		t.Fatal("a cancelled fetch stamped the staleness clock")
	}
}

func mustCommonDir(t *testing.T, core *Core, cwd string) string {
	t.Helper()
	dir, err := core.CommonDir(cwd)
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	return dir
}
