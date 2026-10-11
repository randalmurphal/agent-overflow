package git

import (
	"context"
	"net/http"
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

// TestInvalidatePRCacheClearsCwdEntries verifies that a successful
// CreatePR can drop the stale "no PR" cached value so the next status
// refresh sees the freshly-opened PR rather than waiting up to 30s.
func TestInvalidatePRCacheClearsCwdEntries(t *testing.T) {
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

// TestPRCacheKeepsStickyEntryPastRefreshTTL pins the sweep horizon. The
// per-write sweep used to drop any entry past its refresh TTL, so an
// unrelated branch's lookup could delete the very value a later failure needs
// — making the badge blank intermittently, depending on which workspace
// refreshed last.
func TestPRCacheKeepsStickyEntryPastRefreshTTL(t *testing.T) {
	f := newPRFixture(t)

	f.setMode("pr7")
	f.wantPR("initial success", f.lookup("feat"), "https://example.com/pr/7", 7, false)

	// An unrelated branch refreshes after "feat" has gone stale; its write
	// runs the sweep.
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
