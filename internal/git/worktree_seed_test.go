package git

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// revParse returns cwd's resolution of rev, failing the test if git can't.
func revParse(t *testing.T, cwd, rev string) string {
	t.Helper()
	core := NewCore()
	result, err := core.run(cwd, "rev-parse", rev)
	if err != nil || result.exitCode != 0 {
		t.Fatalf("rev-parse %s in %s: err=%v exit=%d stderr=%s", rev, cwd, err, result.exitCode, result.stderr)
	}
	return strings.TrimSpace(result.stdout)
}

// upstreamOf returns the configured upstream of branch, or "" when it has
// none.
func upstreamOf(t *testing.T, cwd, branch string) string {
	t.Helper()
	core := NewCore()
	result, err := core.run(cwd, "rev-parse", "--verify", "--quiet", "--abbrev-ref", branch+"@{upstream}")
	if err != nil {
		t.Fatalf("rev-parse upstream of %s: %v", branch, err)
	}
	if result.exitCode != 0 {
		return ""
	}
	return strings.TrimSpace(result.stdout)
}

// staleClone is the situation the fetch-before-cut behaviour exists for:
// a clone whose local main is one commit behind origin's, with the
// remote-tracking ref still pointing at the old tip because nobody has
// fetched since.
func staleClone(t *testing.T) (repo, bare string) {
	t.Helper()
	repo, bare = testutil.InitGitRepoWithOrigin(t)
	testutil.AdvanceOriginMain(t, bare)
	return repo, bare
}

func TestWorktreeSeedCutsFromTheFetchedOriginTip(t *testing.T) {
	repo, _ := staleClone(t)
	localMainBefore := revParse(t, repo, "main")

	core := NewCore()
	worktree := filepath.Join(t.TempDir(), "seeded")
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "main", "ao-seeded")
	if err != nil {
		t.Fatalf("CreateWorktreeFromFreshBase: %v", err)
	}
	if seed.FetchErr != nil {
		t.Fatalf("fetch against a local bare origin failed: %v", seed.FetchErr)
	}
	if !seed.FromRemote || seed.Ref != "origin/main" {
		t.Fatalf("seed = %+v, want origin/main from the remote", seed)
	}

	// The new worktree starts at what origin actually has, not at the
	// stale local branch — the whole point of the feature.
	originTip := revParse(t, repo, "origin/main")
	if head := revParse(t, worktree, "HEAD"); head != originTip {
		t.Fatalf("worktree HEAD = %s, want origin/main %s", head, originTip)
	}
	if originTip == localMainBefore {
		t.Fatal("fixture is not stale: origin/main and the local main match")
	}

	// A bootstrap must not move refs the user can see: local main stays
	// exactly where it was, and the new branch acquires no upstream (which
	// git's DWIM would otherwise set to origin/main).
	if after := revParse(t, repo, "main"); after != localMainBefore {
		t.Fatalf("local main moved from %s to %s", localMainBefore, after)
	}
	if upstream := upstreamOf(t, worktree, "ao-seeded"); upstream != "" {
		t.Fatalf("new branch tracks %q; a worktree cut must not configure an upstream", upstream)
	}
}

func TestWorktreeSeedFallsBackToLocalWhenTheFetchFails(t *testing.T) {
	repo, _ := staleClone(t)
	localMain := revParse(t, repo, "main")

	core := NewCore()
	failure := errors.New("remote hung up")
	var calls atomic.Int64
	core.fetchFn = func(context.Context, string) error {
		calls.Add(1)
		return failure
	}

	worktree := filepath.Join(t.TempDir(), "fallback")
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "main", "ao-fallback")
	if err != nil {
		t.Fatalf("a failed fetch must not fail worktree creation: %v", err)
	}
	if !errors.Is(seed.FetchErr, failure) {
		t.Fatalf("seed.FetchErr = %v, want %v", seed.FetchErr, failure)
	}
	if seed.FromRemote || seed.Ref != "main" {
		t.Fatalf("seed = %+v, want the local base after a failed fetch", seed)
	}
	if head := revParse(t, worktree, "HEAD"); head != localMain {
		t.Fatalf("worktree HEAD = %s, want the local main %s", head, localMain)
	}

	// Failure is not freshness: the next cut retries rather than sitting
	// out the window on the strength of a failed fetch (off -> on).
	core.fetchFn = func(ctx context.Context, cwd string) error {
		calls.Add(1)
		return core.fetchOriginQuiet(ctx, cwd)
	}
	second := filepath.Join(t.TempDir(), "recovered")
	seed, err = core.CreateWorktreeFromFreshBase(t.Context(), repo, second, "main", "ao-recovered")
	if err != nil {
		t.Fatalf("CreateWorktreeFromFreshBase after recovery: %v", err)
	}
	if seed.FetchErr != nil || !seed.FromRemote {
		t.Fatalf("seed = %+v, want a successful fetch and the remote base", seed)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("fetch ran %d times, want 2 (the failure must not throttle the retry)", got)
	}
	if head := revParse(t, second, "HEAD"); head != revParse(t, repo, "origin/main") {
		t.Fatal("the recovered cut did not start at origin/main")
	}
}

func TestWorktreeSeedFallsBackWhenTheFetchTimesOut(t *testing.T) {
	repo, _ := staleClone(t)
	localMain := revParse(t, repo, "main")

	core := NewCore()
	entered := make(chan struct{})
	core.fetchFn = func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	previous := worktreeSeedFetchTimeout
	worktreeSeedFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { worktreeSeedFetchTimeout = previous })

	worktree := filepath.Join(t.TempDir(), "timeout")
	start := time.Now()
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "main", "ao-timeout")
	if err != nil {
		t.Fatalf("a timed-out fetch must not fail worktree creation: %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("the fetch never ran")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("worktree creation waited %s on a hung fetch", elapsed)
	}
	if seed.FetchErr == nil {
		t.Fatal("seed.FetchErr is nil after a timed-out fetch")
	}
	if seed.FromRemote {
		t.Fatalf("seed = %+v, want the local base after a timeout", seed)
	}
	if head := revParse(t, worktree, "HEAD"); head != localMain {
		t.Fatalf("worktree HEAD = %s, want the local main %s", head, localMain)
	}
}

func TestWorktreeSeedReusesTheSharedFetchWindow(t *testing.T) {
	repo, _ := staleClone(t)

	core := NewCore()
	var calls atomic.Int64
	core.fetchFn = func(ctx context.Context, cwd string) error {
		calls.Add(1)
		return core.fetchOriginQuiet(ctx, cwd)
	}

	first := filepath.Join(t.TempDir(), "first")
	if _, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, first, "main", "ao-first"); err != nil {
		t.Fatalf("first cut: %v", err)
	}
	second := filepath.Join(t.TempDir(), "second")
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, second, "main", "ao-second")
	if err != nil {
		t.Fatalf("second cut: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch ran %d times for two cuts inside the window, want 1", got)
	}
	// A skipped-because-fresh fetch still yields the remote base: the refs
	// were refreshed a moment ago by someone else.
	if !seed.FromRemote || seed.FetchErr != nil {
		t.Fatalf("seed = %+v, want the remote base with no error", seed)
	}
	if head := revParse(t, second, "HEAD"); head != revParse(t, repo, "origin/main") {
		t.Fatal("the throttled cut did not start at origin/main")
	}

	// And the window is genuinely the shared one — the branch picker's
	// warm-up must see this cut's fetch, not schedule its own.
	if fetched, err := core.MaybeFetchRemotes(repo); err != nil || fetched {
		t.Fatalf("MaybeFetchRemotes after a seeded cut: fetched=%v err=%v, want skip", fetched, err)
	}
}

func TestWorktreeSeedSkipsTheFetchWithoutAnOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "elsewhere", t.TempDir())
	core := NewCore()
	calls := countingFetch(core)

	worktree := filepath.Join(t.TempDir(), "no-origin")
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "main", "ao-no-origin")
	if err != nil {
		t.Fatalf("CreateWorktreeFromFreshBase: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fetch ran %d times against a repo with no origin, want 0", got)
	}
	if seed.FetchErr != nil {
		t.Fatalf("a repo with no origin is not a failure: %v", seed.FetchErr)
	}
	if seed.FromRemote || seed.Ref != "main" {
		t.Fatalf("seed = %+v, want the local base", seed)
	}
	if head := revParse(t, worktree, "HEAD"); head != revParse(t, repo, "main") {
		t.Fatal("the cut did not start at the local main")
	}
}

func TestWorktreeSeedUsesTheLocalBaseWhenOriginHasNoSuchBranch(t *testing.T) {
	repo, _ := staleClone(t)
	testutil.RunGit(t, repo, "branch", "local-only", "main")
	localOnly := revParse(t, repo, "local-only")

	core := NewCore()
	calls := countingFetch(core)
	worktree := filepath.Join(t.TempDir(), "local-only")
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "local-only", "ao-local-only")
	if err != nil {
		t.Fatalf("CreateWorktreeFromFreshBase: %v", err)
	}
	// The fetch still runs — whether origin has this branch is only
	// knowable after it — but the cut falls back to the local ref.
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch ran %d times, want 1", got)
	}
	if seed.FromRemote || seed.Ref != "local-only" {
		t.Fatalf("seed = %+v, want the local branch", seed)
	}
	if head := revParse(t, worktree, "HEAD"); head != localOnly {
		t.Fatalf("worktree HEAD = %s, want local-only %s", head, localOnly)
	}
}

func TestWorktreeSeedWithoutABaseCutsFromHeadAndNeverFetches(t *testing.T) {
	repo, _ := staleClone(t)
	core := NewCore()
	calls := countingFetch(core)

	worktree := filepath.Join(t.TempDir(), "from-head")
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "", "ao-from-head")
	if err != nil {
		t.Fatalf("CreateWorktreeFromFreshBase: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fetch ran %d times with no base branch to resolve, want 0", got)
	}
	if seed.Ref != "" || seed.FromRemote {
		t.Fatalf("seed = %+v, want the empty (HEAD) seed", seed)
	}
	if head := revParse(t, worktree, "HEAD"); head != revParse(t, repo, "HEAD") {
		t.Fatal("the cut did not start at HEAD")
	}
}

func TestWorktreeSeedRejectsAFlagShapedBaseBeforeFetching(t *testing.T) {
	repo, _ := staleClone(t)
	core := NewCore()
	calls := countingFetch(core)

	worktree := filepath.Join(t.TempDir(), "rejected")
	if _, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "--upload-pack=x", "ao-rejected"); err == nil {
		t.Fatal("expected a flag-shaped base branch to be rejected")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fetch ran %d times for an invalid base, want 0", got)
	}
}

func TestWorktreeSeedReportsCreationFailureWithTheFetchDiagnostics(t *testing.T) {
	repo, _ := staleClone(t)
	core := NewCore()

	worktree := filepath.Join(t.TempDir(), "conflict")
	// `main` already exists, so the cut fails at the branch preflight —
	// after the fetch. The seed still describes what the cut would have
	// used, so a caller logging FetchErr can't lose it to an error return.
	seed, err := core.CreateWorktreeFromFreshBase(t.Context(), repo, worktree, "main", "main")
	if err == nil {
		t.Fatal("expected the cut to fail on an existing branch")
	}
	if !seed.FromRemote || seed.Ref != "origin/main" {
		t.Fatalf("seed = %+v, want the resolved base to survive the failure", seed)
	}
}

func TestWorktreeSeedRejectsANonRepository(t *testing.T) {
	core := NewCore()
	calls := countingFetch(core)
	if _, err := core.CreateWorktreeFromFreshBase(
		t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "wt"), "main", "ao-nonrepo",
	); err == nil {
		t.Fatal("expected an error outside a repository")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("fetch ran %d times outside a repository, want 0", got)
	}
}
