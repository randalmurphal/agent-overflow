package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// failingFetch replaces Core.fetchFn with one that fails with whatever
// *failure holds (nil succeeds) and counts its calls, and pins the Core's
// clock to *now.
func failingFetch(core *Core, now *time.Time, failure *error) *atomic.Int64 {
	var calls atomic.Int64
	core.nowFn = func() time.Time { return *now }
	core.fetchFn = func(context.Context, string) error {
		calls.Add(1)
		return *failure
	}
	return &calls
}

// The unattended cadence ticks every minute. A repository that keeps
// failing is fetched on a doubling delay up to fetchRetryMax, and every
// tick in between answers with the same failure without running git.
func TestBackgroundFetchBacksOffWhileItFails(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("remote hung up")
	calls := failingFetch(core, &now, &failure)

	tick := func() {
		t.Helper()
		if _, err := core.FetchRemotesBackground(t.Context(), repo); !errors.Is(err, failure) {
			t.Fatalf("FetchRemotesBackground = %v, want %v", err, failure)
		}
	}
	tick()
	for _, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		before := calls.Load()
		now = now.Add(delay - time.Second)
		tick()
		if got := calls.Load(); got != before {
			t.Fatalf("fetched %s after a failure, before the %s retry was due", delay-time.Second, delay)
		}
		now = now.Add(time.Second)
		tick()
		if got := calls.Load(); got != before+1 {
			t.Fatalf("fetch ran %d times once the %s retry was due, want 1", got-before, delay)
		}
	}
}

func TestBackgroundFetchSuccessEndsTheBackoff(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("remote hung up")
	calls := failingFetch(core, &now, &failure)

	for range 3 {
		if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
			t.Fatal("expected the fetch to fail")
		}
		now = now.Add(fetchRetryMax)
	}
	var none error
	failed := failure
	failure = none
	if fetched, err := core.FetchRemotesBackground(t.Context(), repo); err != nil || !fetched {
		t.Fatalf("recovered fetch = %v, %v; want a fetch", fetched, err)
	}

	// The next failure starts from the base delay again.
	failure = failed
	now = now.Add(FetchStaleWindow)
	before := calls.Load()
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
		t.Fatal("expected the fetch to fail")
	}
	now = now.Add(fetchRetryBase)
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
		t.Fatal("expected the fetch to fail")
	}
	if got := calls.Load() - before; got != 2 {
		t.Fatalf("after a success, %d fetches ran across one base delay, want 2", got)
	}
}

// A fetch its caller ended says nothing about the remote: it starts no
// backoff and marks nothing.
func TestBackgroundFetchCancelledIsNotAFailure(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	var calls atomic.Int64
	started := make(chan struct{}, 1)
	core.fetchFn = func(ctx context.Context, _ string) error {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return errors.New("fatal: could not read Username: terminal prompts disabled")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = core.FetchRemotesBackground(ctx, repo)
	}()
	<-started
	cancel()
	<-done
	// The flight ends after its last waiter left; the next call starts a
	// fresh one at once.
	go func() { <-started; cancel() }()
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	_, _ = core.FetchRemotesBackground(ctx, repo)
	if got := calls.Load(); got != 2 {
		t.Fatalf("fetch ran %d times, want 2: a cancelled fetch must not start a backoff", got)
	}
	if got := core.RemoteSignInError(repo); got != "" {
		t.Fatalf("RemoteSignInError = %q after cancelled fetches, want none", got)
	}
}

// A remote command that succeeded may mean the login is back: the failing
// repository's next unattended fetch is due at once. The mark stays until
// that fetch answers, because a push does not prove origin's fetch works.
func TestSuccessfulRemoteCommandMakesTheRetryDue(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	calls := failingFetch(core, &now, &failure)
	for range 4 {
		_, _ = core.FetchRemotesBackground(t.Context(), repo)
		now = now.Add(fetchRetryMax)
	}
	_, _ = core.FetchRemotesBackground(t.Context(), repo)
	before := calls.Load()
	if _, _ = core.FetchRemotesBackground(t.Context(), repo); calls.Load() != before {
		t.Fatal("fetched again while backing off")
	}

	if err := os.WriteFile(filepath.Join(repo, "pushed.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "add", "pushed.txt")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "pushed")
	if err := core.Push(repo); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := core.RemoteSignInError(repo); got == "" {
		t.Fatal("a push cleared the sign-in mark before any fetch of origin succeeded")
	}
	var none error
	failure = none
	if fetched, err := core.FetchRemotesBackground(t.Context(), repo); err != nil || !fetched {
		t.Fatalf("fetch after a successful push = %v, %v; want it to run at once", fetched, err)
	}
	if got := core.RemoteSignInError(repo); got != "" {
		t.Fatalf("RemoteSignInError = %q after a successful fetch, want none", got)
	}
	if got := calls.Load(); got != before+1 {
		t.Fatalf("%d fetches ran after the push, want 1", got-before)
	}
}

// A repository can keep pushing to a remote that works while origin still
// wants a login. Only the first such success makes origin's retry due
// early; after that the backoff holds.
func TestRepeatedRemoteSuccessDoesNotHoldTheBackoffOpen(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	calls := failingFetch(core, &now, &failure)
	push := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.RunGit(t, repo, "add", name)
		testutil.RunGit(t, repo, "commit", "-q", "-m", name)
		if err := core.Push(repo); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	_, _ = core.FetchRemotesBackground(t.Context(), repo)

	push("first.txt")
	now = now.Add(time.Second)
	_, _ = core.FetchRemotesBackground(t.Context(), repo)
	if got := calls.Load(); got != 2 {
		t.Fatalf("%d fetches ran, want 2: the first successful push makes the retry due", got)
	}

	push("second.txt")
	now = now.Add(time.Second)
	_, _ = core.FetchRemotesBackground(t.Context(), repo)
	if got := calls.Load(); got != 2 {
		t.Fatalf("%d fetches ran, want 2: a second push must not make the retry due again", got)
	}
}

// A read of origin's URL that failed is not origin having moved: the pass
// reports the read's error and the recorded failure stays.
func TestUnreadableOriginKeepsTheFailure(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	calls := failingFetch(core, &now, &failure)
	_, _ = core.FetchRemotesBackground(t.Context(), repo)
	mark := core.RemoteSignInError(repo)
	if mark == "" {
		t.Fatal("expected a sign-in mark")
	}

	config := filepath.Join(repo, ".git", "config")
	saved, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(saved, []byte("\n[broken\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(fetchRetryMax)
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil || errors.Is(err, failure) {
		t.Fatalf("FetchRemotesBackground with an unreadable config = %v, want the read's own error", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d fetches ran, want 1: an unreadable origin is not fetched", got)
	}
	if got := core.RemoteSignInError(repo); got != mark {
		t.Fatalf("RemoteSignInError = %q after an unreadable origin, want it kept as %q", got, mark)
	}

	if err := os.WriteFile(config, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := core.FetchRemotesBackground(t.Context(), repo); !errors.Is(err, failure) || calls.Load() != 2 {
		t.Fatalf("FetchRemotesBackground once the config reads again = %v after %d fetches, want the fetch's failure after 2", err, calls.Load())
	}
}

// A failure is about the origin URL it was fetched from. Pointing origin
// elsewhere, or removing it, ends the record: the mark goes, the observer
// hears it, and the new remote is fetched without waiting out the old one's
// backoff.
func TestMovedOriginEndsTheFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		move func(t *testing.T, repo string)
		want bool
	}{
		{"re-pointed", func(t *testing.T, repo string) {
			testutil.RunGit(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "elsewhere.git"))
		}, true},
		{"removed", func(t *testing.T, repo string) {
			testutil.RunGit(t, repo, "remote", "remove", "origin")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, _ := repoWithOrigin(t)
			core := NewCore()
			now := time.Unix(10_000, 0)
			failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
			calls := failingFetch(core, &now, &failure)
			for range 5 {
				_, _ = core.FetchRemotesBackground(t.Context(), repo)
				now = now.Add(fetchRetryMax)
			}
			_, _ = core.FetchRemotesBackground(t.Context(), repo)
			before := calls.Load()
			var heard atomic.Int64
			core.SetRemoteSignInObserver(func(string) { heard.Add(1) })

			tc.move(t, repo)
			var none error
			failure = none
			fetched, err := core.FetchRemotesBackground(t.Context(), repo)
			if err != nil || fetched != tc.want {
				t.Fatalf("FetchRemotesBackground after origin moved = %v, %v; want %v, nil", fetched, err, tc.want)
			}
			if got := core.RemoteSignInError(repo); got != "" {
				t.Fatalf("RemoteSignInError = %q after origin moved, want none", got)
			}
			if got := heard.Load(); got != 1 {
				t.Fatalf("observer heard %d changes, want 1", got)
			}
			wantCalls := before
			if tc.want {
				wantCalls++
			}
			if got := calls.Load(); got != wantCalls {
				t.Fatalf("%d fetches ran after origin moved, want %d", got-before, wantCalls-before)
			}
		})
	}
}

// A failed fetch can report after a newer fetch of the same repository
// already succeeded (the two only share the remote-refs lock while git
// runs). The older failure must not mark a repository that just fetched.
func TestFetchFailureOlderThanASuccessIsDropped(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	key := mustCommonDir(t, core, repo)
	now := time.Unix(10_000, 0)
	core.nowFn = func() time.Time { return now }
	core.fetchFn = func(context.Context, string) error {
		// While this fetch was failing, a clicked one finished.
		now = now.Add(time.Second)
		core.stampFetchCache(key)
		return errors.New("fatal: Authentication failed for 'https://example.com/'")
	}
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
		t.Fatal("expected the fetch to fail")
	}
	if got := core.RemoteSignInError(repo); got != "" {
		t.Fatalf("RemoteSignInError = %q from a failure older than a success, want none", got)
	}
}

// The cadence's fetch can join a fetch a worktree cut started, whose own
// failure marks nothing. The cadence's caller still records it, once, even
// with two cadence callers waiting on the one fetch.
func TestBackgroundFetchJoiningASeedFetchRecordsItsFailure(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	key := mustCommonDir(t, core, repo)
	var calls atomic.Int64
	release := make(chan struct{})
	core.fetchFn = func(context.Context, string) error {
		calls.Add(1)
		<-release
		return errors.New("fatal: Authentication failed for 'https://example.com/'")
	}
	waiters := func() int {
		core.fetchFlight.mu.Lock()
		defer core.fetchFlight.mu.Unlock()
		if flight := core.fetchFlight.flights[key]; flight != nil {
			return flight.waiters
		}
		return 0
	}
	await := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for waiters() != want {
			if time.Now().After(deadline) {
				t.Fatalf("the fetch has %d waiters, want %d", waiters(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	var wg sync.WaitGroup
	wg.Go(func() { _, _ = core.fetchOriginForSeed(t.Context(), repo) })
	await(1)
	for range 2 {
		wg.Go(func() { _, _ = core.FetchRemotesBackground(t.Context(), repo) })
	}
	await(3)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("%d fetches ran, want the one all three shared", got)
	}
	if got := core.RemoteSignInError(repo); got == "" {
		t.Fatal("a cadence fetch that joined a seed fetch recorded no sign-in failure")
	}
	core.fetchCacheMu.RLock()
	failures := core.fetchFailures[key].failures
	core.fetchCacheMu.RUnlock()
	if failures != 1 {
		t.Fatalf("one failed fetch was counted %d times", failures)
	}
}

// InvalidateFetchCache promises the next call refetches, failing or not.
func TestInvalidateFetchCacheEndsAFailureToo(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	calls := failingFetch(core, &now, &failure)
	_, _ = core.FetchRemotesBackground(t.Context(), repo)

	core.InvalidateFetchCache(repo)
	if got := core.RemoteSignInError(repo); got != "" {
		t.Fatalf("RemoteSignInError = %q after InvalidateFetchCache, want none", got)
	}
	if _, _ = core.FetchRemotesBackground(t.Context(), repo); calls.Load() != 2 {
		t.Fatal("the call after InvalidateFetchCache did not refetch")
	}
}

// A worktree cut's fetch reports its own failure: it starts no backoff and
// marks nothing, so a repository is never marked while no cadence retries it.
func TestSeedFetchFailureMarksNothing(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	calls := failingFetch(core, &now, &failure)
	if _, err := core.fetchOriginForSeed(t.Context(), repo); err == nil {
		t.Fatal("expected the seed fetch to fail")
	}
	if got := core.RemoteSignInError(repo); got != "" {
		t.Fatalf("RemoteSignInError = %q after a failed seed fetch, want none", got)
	}
	if _, _ = core.FetchRemotesBackground(t.Context(), repo); calls.Load() != 2 {
		t.Fatal("a failed seed fetch held the cadence's fetch back")
	}
}
