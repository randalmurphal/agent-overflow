package git

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// seedForgeCacheGitHub is a test-only helper that populates the forge
// classification cache so Core.lookupOpenPR's dispatch resolves to the
// github forge without requiring the test to set up a real origin URL.
// The Core.forgeFor call would otherwise return nullForge for a bare
// t.TempDir() (no origin remote) and short-circuit the forge read.
func seedForgeCacheGitHub(t *testing.T, core *Core, cwd string) {
	t.Helper()
	seedForgeCacheGitHubOrigin(t, core, cwd, "https://github.com/acme/repo.git")
}

// seedForgeCacheGitHubOrigin is seedForgeCacheGitHub with an explicit origin
// URL, for the tests that care which remote a cached PR was found under.
func seedForgeCacheGitHubOrigin(t *testing.T, core *Core, cwd, originURL string) {
	t.Helper()
	if forge := core.recordOrigin(cwd, originIdentity{url: originURL, known: true}, core.nowFn()); forge != "github" {
		t.Fatalf("seeded origin %q classified as %q, want github", originURL, forge)
	}
}

// openPRForge is an httptest GitHub whose OpenPRsByHead answer is switched
// by mode ("pr7", "pr8", "none", anything else fails with a 502) and which
// counts the requests it serves.
type openPRForge struct {
	mode  atomic.Value
	calls atomic.Int32
}

func newOpenPRCore(t *testing.T, mode string) (*Core, *openPRForge) {
	t.Helper()
	forge := &openPRForge{}
	forge.mode.Store(mode)
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "OpenPRsByHead" {
			return forgeUnexpected(t, call)
		}
		forge.calls.Add(1)
		switch forge.mode.Load().(string) {
		case "pr7":
			return githubData(`{"repository":{"pullRequests":{"nodes":[{"url":"https://example.com/pr/7","number":7,"title":"seven","state":"OPEN"}]}}}`)
		case "pr8":
			return githubData(`{"repository":{"pullRequests":{"nodes":[{"url":"https://example.com/pr/8","number":8,"title":"eight","state":"OPEN"}]}}}`)
		case "none":
			return githubData(`{"repository":{"pullRequests":{"nodes":[]}}}`)
		}
		return forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}
	})
	return core, forge
}

func TestLookupOpenPRReadsTheForge(t *testing.T) {
	t.Parallel()
	core, _ := newOpenPRCore(t, "pr7")
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	url, number, lookupErr := core.lookupOpenPR(t.Context(), cwd, "main")
	if lookupErr != "" {
		t.Fatalf("lookupErr = %q, want empty", lookupErr)
	}
	if url != "https://example.com/pr/7" || number != 7 {
		t.Fatalf("lookup = (%q, %d), want PR 7", url, number)
	}
}

// TestLookupOpenPRCachesResults pins that repeated lookups on the same
// (cwd, branch) inside the TTL window do NOT reach the forge again.
// Without the cache, gitwatch's hot path would turn every fs-event
// debounce into a forge request.
func TestLookupOpenPRCachesResults(t *testing.T) {
	t.Parallel()
	core, forge := newOpenPRCore(t, "pr7")
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	if url, _, lookupErr := core.lookupOpenPR(t.Context(), cwd, "feat-a"); url == "" || lookupErr != "" {
		t.Fatalf("cold lookup returned empty url")
	}
	for i := 0; i < 5; i++ {
		if url, _, lookupErr := core.lookupOpenPR(t.Context(), cwd, "feat-a"); url == "" || lookupErr != "" {
			t.Fatalf("warm lookup #%d returned empty url", i)
		}
	}
	if got := forge.calls.Load(); got != 1 {
		t.Fatalf("forge requests = %d, want 1 (cache should absorb 5 follow-up calls)", got)
	}

	// Different branch -> different cache key -> a fresh request.
	if url, _, lookupErr := core.lookupOpenPR(t.Context(), cwd, "feat-b"); url == "" || lookupErr != "" {
		t.Fatalf("different-branch lookup returned empty url")
	}
	if got := forge.calls.Load(); got != 2 {
		t.Fatalf("after different-branch lookup: forge requests = %d, want 2", got)
	}

	// TTL expiry -> a fresh request. Drive nowFn forward past the TTL.
	core.nowFn = func() time.Time { return time.Now().Add(prLookupTTL + time.Second) }
	if url, _, lookupErr := core.lookupOpenPR(t.Context(), cwd, "feat-a"); url == "" || lookupErr != "" {
		t.Fatalf("post-TTL lookup returned empty url")
	}
	if got := forge.calls.Load(); got != 3 {
		t.Fatalf("after TTL expiry: forge requests = %d, want 3", got)
	}
}

// A lookup that read "no PR" before the PR was created, and finished after
// the invalidation that announced it, must not cache that answer: the next
// lookup asks the forge again and sees the PR.
func TestInvalidatePRCacheDiscardsALookupInFlight(t *testing.T) {
	t.Parallel()
	var core *Core
	cwd := t.TempDir()
	var calls atomic.Int32
	core, _ = newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "OpenPRsByHead" {
			return forgeUnexpected(t, call)
		}
		if calls.Add(1) == 1 {
			// The forge answered before the PR existed; the create and its
			// invalidation land before this lookup stores the answer.
			core.InvalidatePRCache(cwd)
			return githubData(`{"repository":{"pullRequests":{"nodes":[]}}}`)
		}
		return githubData(`{"repository":{"pullRequests":{"nodes":[{"url":"https://example.com/pr/7","number":7,"title":"seven","state":"OPEN"}]}}}`)
	})
	seedForgeCacheGitHub(t, core, cwd)

	if url, _, _ := core.lookupOpenPR(t.Context(), cwd, "feat"); url != "" {
		t.Fatalf("first lookup = %q, want no PR", url)
	}
	if url, number, lookupErr := core.lookupOpenPR(t.Context(), cwd, "feat"); number != 7 || lookupErr != "" {
		t.Fatalf("lookup after invalidation = (%q, %d, %q), want PR 7", url, number, lookupErr)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("forge requests = %d, want 2", got)
	}
}

// Invalidating one workspace leaves another's in-flight lookup alone: its
// answer is cached, and the next lookup asks the forge nothing.
func TestInvalidatePRCacheKeepsAnotherWorkspacesLookupInFlight(t *testing.T) {
	t.Parallel()
	var core *Core
	cwd, other := t.TempDir(), t.TempDir()
	var calls atomic.Int32
	core, _ = newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "OpenPRsByHead" {
			return forgeUnexpected(t, call)
		}
		if calls.Add(1) == 1 {
			core.InvalidatePRCache(other)
		}
		return githubData(`{"repository":{"pullRequests":{"nodes":[]}}}`)
	})
	seedForgeCacheGitHub(t, core, cwd)

	core.lookupOpenPR(t.Context(), cwd, "feat")
	core.lookupOpenPR(t.Context(), cwd, "feat")
	if got := calls.Load(); got != 1 {
		t.Fatalf("forge requests = %d, want 1: another workspace's invalidation discarded this one's answer", got)
	}
}

// TestInvalidatePRCacheStalesCwdEntries verifies that a successful
// CreatePR makes the cached "no PR" answer stale so the next status
// refresh asks the forge rather than waiting up to 30s.
func TestInvalidatePRCacheStalesCwdEntries(t *testing.T) {
	t.Parallel()
	core, forge := newOpenPRCore(t, "none")
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	seedForgeCacheGitHub(t, core, cwdA)
	seedForgeCacheGitHub(t, core, cwdB)

	// Seed the cache with a "no PR" answer for two cwds.
	core.lookupOpenPR(t.Context(), cwdA, "main")
	core.lookupOpenPR(t.Context(), cwdB, "main")

	// Invalidate cwdA only - cwdB's cache must be untouched.
	core.InvalidatePRCache(cwdA)

	core.lookupOpenPR(t.Context(), cwdA, "main") // miss -> request
	core.lookupOpenPR(t.Context(), cwdB, "main") // hit -> no request

	if got := forge.calls.Load(); got != 3 {
		t.Fatalf("forge requests = %d, want 3 (2 seeds + 1 post-invalidate refetch)", got)
	}
}

// A failed lookup is cached for a backoff that doubles per consecutive
// failure of the (cwd, branch), from prLookupErrorBase up to
// prLookupErrorMax, and a success resets it.
func TestLookupOpenPRBacksOffFailures(t *testing.T) {
	t.Parallel()
	core, forge := newOpenPRCore(t, "fail")
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)
	now := time.Now()
	core.nowFn = func() time.Time { return now }

	url, number, lookupErr := core.lookupOpenPR(t.Context(), cwd, "main")
	if url != "" || number != 0 || lookupErr == "" {
		t.Fatalf("lookup = (%q, %d, %q), want an error and no PR", url, number, lookupErr)
	}
	if _, _, cachedErr, cached := core.lookupOpenPRCached(cwd, "main"); !cached || cachedErr == "" {
		t.Fatal("the lookup error is not cached")
	}
	// Each consecutive failure is cached twice as long as the last: a
	// lookup just inside the delay is answered from the cache, one just
	// past it asks the forge again. The walk outlasts forgeDetectionTTL,
	// so the origin identity is seeded again at every step.
	requests := int32(1)
	for delay := prLookupErrorBase; ; delay = min(delay*2, prLookupErrorMax) {
		now = now.Add(delay - time.Second)
		seedForgeCacheGitHub(t, core, cwd)
		core.lookupOpenPR(t.Context(), cwd, "main")
		if got := forge.calls.Load(); got != requests {
			t.Fatalf("inside a %s backoff: forge requests = %d, want %d", delay, got, requests)
		}
		now = now.Add(2 * time.Second)
		core.lookupOpenPR(t.Context(), cwd, "main")
		requests++
		if got := forge.calls.Load(); got != requests {
			t.Fatalf("past a %s backoff: forge requests = %d, want %d", delay, got, requests)
		}
		if delay == prLookupErrorMax {
			break
		}
	}

	// A success resets the backoff: the next failure is cached for the
	// base again.
	forge.mode.Store("pr7")
	now = now.Add(prLookupErrorMax + time.Second)
	seedForgeCacheGitHub(t, core, cwd)
	if url, _, lookupErr := core.lookupOpenPR(t.Context(), cwd, "main"); url == "" || lookupErr != "" {
		t.Fatalf("recovery lookup = (%q, %q), want PR 7", url, lookupErr)
	}
	forge.mode.Store("fail")
	now = now.Add(prLookupTTL + time.Second)
	core.lookupOpenPR(t.Context(), cwd, "main")
	before := forge.calls.Load()
	now = now.Add(prLookupErrorBase + time.Second)
	core.lookupOpenPR(t.Context(), cwd, "main")
	if got := forge.calls.Load(); got != before+1 {
		t.Fatalf("after a success the next failure was cached past the base: requests = %d, want %d", got, before+1)
	}
}

// A branch origin does not have cannot head a PR: the lookup answers "no
// PR" from the missing refs/remotes/origin/<branch> without asking the
// forge. Once the branch is pushed, the forge is asked.
func TestLookupOpenPRSkipsTheForgeForAnUnpushedBranch(t *testing.T) {
	t.Parallel()
	core, forge := newOpenPRCore(t, "pr7")
	repo, _ := testutil.InitGitRepoWithOrigin(t)
	seedForgeCacheGitHub(t, core, repo)
	testutil.RunGit(t, repo, "checkout", "-b", "feat")

	url, number, lookupErr := core.lookupOpenPR(t.Context(), repo, "feat")
	if url != "" || number != 0 || lookupErr != "" {
		t.Fatalf("unpushed branch lookup = (%q, %d, %q), want no PR", url, number, lookupErr)
	}
	if got := forge.calls.Load(); got != 0 {
		t.Fatalf("an unpushed branch made %d forge requests, want none", got)
	}
	if _, _, _, cached := core.lookupOpenPRCached(repo, "feat"); !cached {
		t.Fatal("the unpushed answer is not cached, so StatusFast would report it pending")
	}

	testutil.RunGit(t, repo, "push", "-u", "origin", "feat")
	core.InvalidatePRCache(repo)
	url, number, lookupErr = core.lookupOpenPR(t.Context(), repo, "feat")
	if url != "https://example.com/pr/7" || number != 7 || lookupErr != "" {
		t.Fatalf("pushed branch lookup = (%q, %d, %q), want PR 7", url, number, lookupErr)
	}
	if got := forge.calls.Load(); got != 1 {
		t.Fatalf("a pushed branch made %d forge requests, want 1", got)
	}
}

// prLookup is one lookupOpenPR result, named so the sticky-cache tests read
// as transitions rather than as three positional returns.
type prLookup struct {
	url    string
	number int
	err    string
}

// prFixture drives lookupOpenPR over a forge whose next answer is switched
// by mode, against a Core with a controllable clock. Both are needed to
// exercise *sequences*: the sticky last-known-PR behaviour is defined by
// what a failure does to the result of the lookup before it, so state
// coverage alone would miss it.
type prFixture struct {
	t     *testing.T
	core  *Core
	forge *openPRForge
	cwd   string
	now   time.Time
}

func newPRFixture(t *testing.T) *prFixture {
	t.Helper()
	core, forge := newOpenPRCore(t, "fail")
	f := &prFixture{t: t, core: core, forge: forge, cwd: t.TempDir(), now: time.Now()}
	f.core.nowFn = func() time.Time { return f.now }
	seedForgeCacheGitHub(t, f.core, f.cwd)
	return f
}

// setMode selects what the next open-PR read answers and advances the
// clock past the success TTL so the call is a genuine re-fetch rather than
// a cache hit.
func (f *prFixture) setMode(mode string) {
	f.forge.mode.Store(mode)
	f.expireLookup()
}

// expireLookup steps the clock just past the longest-lived cached lookup
// so the next call re-fetches. The fixtures fail only a few times in a
// row, so each step stays well inside forgeDetectionTTL and the seeded
// origin identity remains live across a test.
func (f *prFixture) expireLookup() {
	until := f.now.Add(prLookupTTL)
	f.core.prCacheMu.RLock()
	for _, entry := range f.core.prCache {
		if entry.expiresAt.After(until) {
			until = entry.expiresAt
		}
	}
	f.core.prCacheMu.RUnlock()
	f.now = until.Add(time.Second)
}

func (f *prFixture) lookup(branch string) prLookup {
	f.t.Helper()
	url, number, err := f.core.lookupOpenPR(f.t.Context(), f.cwd, branch)
	return prLookup{url: url, number: number, err: err}
}

func (f *prFixture) wantPR(stage string, got prLookup, url string, number int, wantErr bool) {
	f.t.Helper()
	if got.url != url || got.number != number {
		f.t.Errorf("%s: PR = (%q, %d), want (%q, %d)", stage, got.url, got.number, url, number)
	}
	if wantErr && got.err == "" {
		f.t.Errorf("%s: lookup error is empty, want the forge failure surfaced alongside the PR", stage)
	}
	if !wantErr && got.err != "" {
		f.t.Errorf("%s: lookup error = %q, want empty", stage, got.err)
	}
}

// TestLookupOpenPRKeepsLastKnownPRAcrossTransientFailure walks the transition
// that blanked the badge: a forge rate-limit or auth blip in the middle of an
// otherwise healthy branch must keep showing the PR (with the error beside
// it), and a later success must still be able to move it.
func TestLookupOpenPRKeepsLastKnownPRAcrossTransientFailure(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	f.setMode("fail")
	f.wantPR("first failure", f.lookup("feat"), "https://example.com/pr/7", 7, true)

	// Still failing after the error TTL expires: the sticky value must chain
	// through a genuine re-fetch rather than decay to empty on the second miss.
	f.expireLookup()
	f.wantPR("repeated failure", f.lookup("feat"), "https://example.com/pr/7", 7, true)

	f.setMode("pr8")
	f.wantPR("recovery", f.lookup("feat"), "https://example.com/pr/8", 8, false)
}

// TestLookupOpenPRClearsPRWhenForgeAnswersNone pins the other side of the
// sticky rule: only a *failed* lookup keeps the old PR. A successful lookup
// that finds none means the PR was merged or closed, and the badge must go.
func TestLookupOpenPRClearsPRWhenForgeAnswersNone(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	f.setMode("none")
	f.wantPR("successful empty answer", f.lookup("feat"), "", 0, false)

	f.setMode("fail")
	f.wantPR("failure after empty answer", f.lookup("feat"), "", 0, true)
}

// TestLookupOpenPRDropsLastKnownPRWhenOriginRetargets is the head-identity
// guard: the repo's origin now reads as a different remote, so the cached PR
// may belong to a repository the branch no longer tracks.
func TestLookupOpenPRDropsLastKnownPRWhenOriginRetargets(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	seedForgeCacheGitHubOrigin(t, f.core, f.cwd, "https://github.com/other/fork.git")

	f.setMode("fail")
	f.wantPR("failure after origin change", f.lookup("feat"), "", 0, true)
}

// TestLookupOpenPRKeepsLastKnownPRWhenOriginUnknown is the inverse, and the
// one that is easy to get backwards: failing to *read* the origin remote is
// an absence of information, not evidence that the remote changed. Dropping
// the badge there would blank it on exactly the transient failures the sticky
// value exists to survive.
func TestLookupOpenPRKeepsLastKnownPRWhenOriginUnknown(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	// Drop the cached classification: cwd is a bare temp dir, so the next
	// detection re-reads `git remote get-url origin`, fails, and records an
	// unknown identity (which also routes the lookup itself into an error).
	f.core.InvalidateForgeCache(f.cwd)
	f.expireLookup()

	f.wantPR("failure with unreadable origin", f.lookup("feat"), "https://example.com/pr/7", 7, true)
}

// TestLookupOpenPRWithoutPriorSuccessStaysEmpty guards the cold-start case:
// there is nothing to be sticky about, and repeated failures must not invent
// a PR or hold on to one.
func TestLookupOpenPRWithoutPriorSuccessStaysEmpty(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("fail")
	f.wantPR("first failure", f.lookup("feat"), "", 0, true)
	f.expireLookup()
	f.wantPR("second failure", f.lookup("feat"), "", 0, true)
}

// TestPRCacheKeepsStickyEntryPastRefreshTTL pins that no entry is dropped
// for its age. A per-write sweep used to drop entries past their refresh
// TTL (later, past a fixed retention), so an unrelated branch's lookup
// could delete the very value a later failure needs, and a branch left
// alone long enough lost its badge on the first failed re-check.
func TestPRCacheKeepsStickyEntryPastRefreshTTL(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	// A day later an unrelated branch refreshes; its write is where a sweep
	// would run. The day outlasts forgeDetectionTTL, so the origin identity
	// is seeded again.
	f.now = f.now.Add(24 * time.Hour)
	seedForgeCacheGitHub(t, f.core, f.cwd)
	f.setMode("none")
	f.wantPR("sibling branch", f.lookup("other"), "", 0, false)

	f.core.prCacheMu.RLock()
	_, kept := f.core.prCache[prCacheKey(f.cwd, "feat")]
	f.core.prCacheMu.RUnlock()
	if !kept {
		t.Fatal("expired-but-retained entry for feat was swept by a sibling write")
	}

	f.setMode("fail")
	f.wantPR("failure after sibling sweep", f.lookup("feat"), "https://example.com/pr/7", 7, true)
}

// TestStatusFastReportsAPendingLookupUntilTheCacheIsWarm: the fast status
// a subscribe answers with cannot tell "no PR" from "not looked up yet" by
// its PR fields alone, so it says which. A warm entry, PR or none, is an
// answer; a cold or expired one is not.
func TestStatusFastReportsAPendingLookupUntilTheCacheIsWarm(t *testing.T) {
	t.Parallel()
	core, _ := newOpenPRCore(t, "none")
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/repo.git")
	seedForgeCacheGitHub(t, core, repo)

	cold, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast cold: %v", err)
	}
	if !cold.OpenPRLookupPending || cold.OpenPRURL != "" {
		t.Fatalf("cold fast status = pending %v url %q, want pending with no PR fields", cold.OpenPRLookupPending, cold.OpenPRURL)
	}

	full, err := core.Status(t.Context(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if full.OpenPRLookupPending {
		t.Fatal("a full status is never pending")
	}
	if full.Equal(cold) {
		t.Fatal("the warmed answer must differ from the pending one, or it is never broadcast")
	}

	warm, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast warm: %v", err)
	}
	if warm.OpenPRLookupPending || warm.OpenPRURL != "" {
		t.Fatalf("warm fast status = pending %v url %q, want a settled answer of no PR", warm.OpenPRLookupPending, warm.OpenPRURL)
	}

	core.nowFn = func() time.Time { return time.Now().Add(prLookupTTL + time.Second) }
	expired, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast expired: %v", err)
	}
	if !expired.OpenPRLookupPending {
		t.Fatal("an expired entry is a cold cache again")
	}
}

// TestLookupOpenPRCancelledCallLeavesCacheAlone: a caller that gave up
// (a closed watcher, a dropped RPC) learned nothing about the forge, so
// its failure must not become the branch's cached answer for the next
// caller.
func TestLookupOpenPRCancelledCallLeavesCacheAlone(t *testing.T) {
	f := newPRFixture(t)
	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	f.expireLookup()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, lookupErr := f.core.lookupOpenPR(ctx, f.cwd, "feat"); lookupErr == "" {
		t.Fatal("a cancelled lookup reported no error")
	}
	if _, _, _, cached := f.core.lookupOpenPRCached(f.cwd, "feat"); cached {
		t.Fatal("a cancelled lookup was cached as the branch's answer")
	}
	f.wantPR("after cancel", f.lookup("feat"), "https://example.com/pr/7", 7, false)
}

// The bug this guards: opening a thread invalidates its workspace's PR
// cache, and that used to delete the entries, so a forge failure on the
// re-check that follows had no last known PR to carry and the badge
// blanked. Invalidation makes the answer stale; a failed re-check still
// shows the PR beside the error, and the forge was asked.
func TestInvalidatePRCacheKeepsTheLastKnownPRThroughAFailedRecheck(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	f.core.InvalidatePRCache(f.cwd)
	f.forge.mode.Store("fail")
	before := f.forge.calls.Load()
	f.wantPR("failed re-check after invalidation", f.lookup("feat"), "https://example.com/pr/7", 7, true)
	if got := f.forge.calls.Load(); got != before+1 {
		t.Fatalf("forge requests after invalidation = %d, want %d: a stale entry must be asked again", got, before+1)
	}

	// Only an answer from the forge replaces it.
	f.setMode("none")
	f.wantPR("forge answers none", f.lookup("feat"), "", 0, false)
}

// ForgetPRCache is for a workspace that is gone: unlike invalidation it
// drops the last known PR, so a checkout re-created at the path starts
// with nothing.
func TestForgetPRCacheDropsTheLastKnownPR(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	f.core.ForgetPRCache(f.cwd)
	f.forge.mode.Store("fail")
	f.wantPR("failure after forget", f.lookup("feat"), "", 0, true)
}

// A removed worktree's answers go with it, so the cache does not hold a
// last known PR for a path that no longer exists.
func TestRemoveWorktreeForgetsItsPRCache(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	worktree := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feat", worktree)
	core := NewCore()
	now := time.Now()
	core.storePRLookup(worktree, prCacheKey(worktree, "feat"), 0, now, prCacheEntry{url: "https://example.com/pr/7", number: 7, expiresAt: now.Add(prLookupTTL)})
	core.storePRLookup(repo, prCacheKey(repo, "main"), 0, now, prCacheEntry{url: "https://example.com/pr/1", number: 1, expiresAt: now.Add(prLookupTTL)})

	if err := core.RemoveWorktreeForce(repo, worktree, false); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	core.prCacheMu.RLock()
	_, kept := core.prCache[prCacheKey(worktree, "feat")]
	_, other := core.prCache[prCacheKey(repo, "main")]
	core.prCacheMu.RUnlock()
	if kept {
		t.Fatal("the removed worktree's open-PR answer is still cached")
	}
	if !other {
		t.Fatal("removing a worktree dropped another workspace's open-PR answer")
	}
}

// The cache is bounded by a count, since no entry ages out: a new key past
// prCacheMaxEntries evicts the entry stored longest ago, and rewriting a
// held key evicts nothing.
func TestPRCacheEvictsTheEntryStoredLongestAgo(t *testing.T) {
	t.Parallel()
	core := NewCore()
	cwd := t.TempDir()
	start := time.Now()
	key := func(i int) string { return prCacheKey(cwd, fmt.Sprintf("b%d", i)) }
	for i := range prCacheMaxEntries {
		core.storePRLookup(cwd, key(i), 0, start.Add(time.Duration(i)*time.Second), prCacheEntry{})
	}
	// Rewriting the oldest key makes it the newest, and evicts nothing.
	core.storePRLookup(cwd, key(0), 0, start.Add(time.Duration(prCacheMaxEntries)*time.Second), prCacheEntry{})
	core.storePRLookup(cwd, key(prCacheMaxEntries), 0, start.Add(time.Duration(prCacheMaxEntries+1)*time.Second), prCacheEntry{})

	core.prCacheMu.RLock()
	defer core.prCacheMu.RUnlock()
	if got := len(core.prCache); got != prCacheMaxEntries {
		t.Fatalf("cache holds %d entries, want the bound %d", got, prCacheMaxEntries)
	}
	if _, ok := core.prCache[key(1)]; ok {
		t.Fatal("the entry stored longest ago (b1) was not evicted")
	}
	for _, i := range []int{0, 2, prCacheMaxEntries} {
		if _, ok := core.prCache[key(i)]; !ok {
			t.Fatalf("b%d was evicted, want only b1 gone", i)
		}
	}
}

// statusRepo is a repository whose origin is a GitHub URL and whose main
// branch origin has, so Status asks the fake forge for main's open PR.
func statusRepo(t *testing.T) string {
	t.Helper()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/repo.git")
	testutil.RunGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	return repo
}

// The fast status a re-attach answers with carries the branch's last
// known PR while its lookup is pending, so the badge and review pane keep
// it instead of blanking until the forge answers. An origin that now reads
// as a different remote withholds it.
func TestStatusFastShowsTheLastKnownPRWhileItsLookupIsPending(t *testing.T) {
	t.Parallel()
	core, _ := newOpenPRCore(t, "pr7")
	repo := statusRepo(t)

	full, err := core.Status(t.Context(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if full.OpenPRNumber != 7 {
		t.Fatalf("Status PR = %d, want 7", full.OpenPRNumber)
	}

	core.InvalidatePRCache(repo)
	pending, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast: %v", err)
	}
	if !pending.OpenPRLookupPending || pending.OpenPRNumber != 7 || pending.OpenPRURL != "https://example.com/pr/7" {
		t.Fatalf("fast status after invalidation = pending %v PR (%q, %d), want pending with PR 7",
			pending.OpenPRLookupPending, pending.OpenPRURL, pending.OpenPRNumber)
	}
	if pending.OpenPRLookupError != "" {
		t.Fatalf("a pending status carries the error %q, want none until the re-check answers", pending.OpenPRLookupError)
	}

	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://github.com/other/fork.git")
	core.InvalidateForgeCache(repo)
	retargeted, err := core.StatusFast(repo)
	if err != nil {
		t.Fatalf("StatusFast after retarget: %v", err)
	}
	if !retargeted.OpenPRLookupPending || retargeted.OpenPRURL != "" || retargeted.OpenPRNumber != 0 {
		t.Fatalf("fast status after the origin moved = pending %v PR (%q, %d), want pending with no PR",
			retargeted.OpenPRLookupPending, retargeted.OpenPRURL, retargeted.OpenPRNumber)
	}
}

// Status reports when a failed lookup is asked again, so the status
// watcher can re-check without a local change, and reports nothing once
// a lookup succeeds. WithPRLookupErrorBase sets the first delay.
func TestStatusReportsWhenAFailedLookupIsRecheckedNext(t *testing.T) {
	t.Parallel()
	core, forge := newOpenPRCore(t, "fail")
	WithPRLookupErrorBase(500 * time.Millisecond)(core)
	now := time.Now()
	core.nowFn = func() time.Time { return now }
	repo := statusRepo(t)

	failed, err := core.Status(t.Context(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if failed.OpenPRLookupError == "" {
		t.Fatal("the failed lookup reported no error")
	}
	if want := now.Add(500 * time.Millisecond); !failed.OpenPRRecheckAt.Equal(want) {
		t.Fatalf("OpenPRRecheckAt = %v, want %v", failed.OpenPRRecheckAt, want)
	}

	forge.mode.Store("pr7")
	now = now.Add(time.Second)
	recovered, err := core.Status(t.Context(), repo)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if recovered.OpenPRNumber != 7 || !recovered.OpenPRRecheckAt.IsZero() {
		t.Fatalf("recovered status = PR %d recheck %v, want PR 7 and no recheck", recovered.OpenPRNumber, recovered.OpenPRRecheckAt)
	}
}

// An answer's lifetime, and a failure's wait, run from when the forge
// answered, not from when the request went out: a request that hangs for
// longer than the wait must not be followed at once by another. The fake
// clock moves only inside the forge's handler, standing in for the time the
// request took.
func TestLookupOpenPRTimesItsAnswerFromWhenTheForgeAnswered(t *testing.T) {
	t.Parallel()
	const took = 45 * time.Second
	start := time.Now()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	var mode atomic.Value
	mode.Store("fail")
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "OpenPRsByHead" {
			return forgeUnexpected(t, call)
		}
		clock.Add(int64(took))
		if mode.Load().(string) == "pr7" {
			return githubData(`{"repository":{"pullRequests":{"nodes":[{"url":"https://example.com/pr/7","number":7,"title":"seven","state":"OPEN"}]}}}`)
		}
		return forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}
	})
	core.nowFn = func() time.Time { return time.Unix(0, clock.Load()) }
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	core.lookupOpenPR(t.Context(), cwd, "feat")
	if got, want := core.openPRRecheckAt(cwd, "feat"), start.Add(took+prLookupErrorBase); !got.Equal(want) {
		t.Fatalf("failed lookup re-checked at %v, want %v: the wait must start when the forge answered", got, want)
	}

	mode.Store("pr7")
	clock.Add(int64(prLookupErrorBase))
	answered := time.Unix(0, clock.Load()).Add(took)
	core.lookupOpenPR(t.Context(), cwd, "feat")
	core.prCacheMu.RLock()
	expiresAt := core.prCache[prCacheKey(cwd, "feat")].expiresAt
	core.prCacheMu.RUnlock()
	if want := answered.Add(prLookupTTL); !expiresAt.Equal(want) {
		t.Fatalf("successful lookup expires at %v, want %v", expiresAt, want)
	}
}
