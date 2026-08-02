package git

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// seedForgeCacheGitHub is a test-only helper that populates the forge
// classification cache so Core.lookupOpenPR's dispatch resolves to the
// github forge without requiring the test to set up a real origin URL.
// The Core.forgeFor call would otherwise return nullForge for a bare
// t.TempDir() (no origin remote) and short-circuit gh invocation.
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

func TestLookupOpenPRUsesGHWhenAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PATH override in short mode")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho '[{\"url\":\"https://example.com/pr/7\",\"number\":7,\"title\":\"Demo PR\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	url, number, lookupErr := core.lookupOpenPR(cwd, "main")
	if lookupErr != "" {
		t.Fatalf("lookupErr = %q, want empty", lookupErr)
	}
	if url != "https://example.com/pr/7" {
		t.Fatalf("url = %q, want https://example.com/pr/7", url)
	}
	if number != 7 {
		t.Fatalf("number = %d, want 7", number)
	}
}

// TestLookupOpenPRCachesResults pins the perf optimisation that
// repeated lookups on the same (cwd, branch) inside the TTL window do
// NOT shell out again. Without the cache, gitwatch's hot path would
// translate every fs-event-debounce into a `gh pr list` round-trip.
func TestLookupOpenPRCachesResults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "calls")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\nprintf x >> " + counterFile + "\necho '[{\"url\":\"https://example.com/pr/9\",\"number\":9,\"title\":\"x\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	// First call: cold cache -> shell out.
	if url, _, lookupErr := core.lookupOpenPR(cwd, "feat-a"); url == "" || lookupErr != "" {
		t.Fatalf("cold lookup returned empty url")
	}
	// Subsequent calls within TTL: warm cache -> no shell out.
	for i := 0; i < 5; i++ {
		if url, _, lookupErr := core.lookupOpenPR(cwd, "feat-a"); url == "" || lookupErr != "" {
			t.Fatalf("warm lookup #%d returned empty url", i)
		}
	}
	calls, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got := len(calls); got != 1 {
		t.Fatalf("gh invocations = %d, want 1 (cache should absorb 5 follow-up calls)", got)
	}

	// Different branch -> different cache key -> fresh shell out.
	if url, _, lookupErr := core.lookupOpenPR(cwd, "feat-b"); url == "" || lookupErr != "" {
		t.Fatalf("different-branch lookup returned empty url")
	}
	calls, _ = os.ReadFile(counterFile)
	if got := len(calls); got != 2 {
		t.Fatalf("after different-branch lookup: gh invocations = %d, want 2", got)
	}

	// TTL expiry -> fresh shell out. Drive nowFn forward past the TTL.
	core.nowFn = func() time.Time { return time.Now().Add(prLookupTTL + time.Second) }
	if url, _, lookupErr := core.lookupOpenPR(cwd, "feat-a"); url == "" || lookupErr != "" {
		t.Fatalf("post-TTL lookup returned empty url")
	}
	calls, _ = os.ReadFile(counterFile)
	if got := len(calls); got != 3 {
		t.Fatalf("after TTL expiry: gh invocations = %d, want 3", got)
	}
}

// TestInvalidatePRCacheClearsCwdEntries verifies that a successful
// CreatePR can drop the stale "no PR" cached value so the next status
// refresh sees the freshly-opened PR rather than waiting up to 30s.
func TestInvalidatePRCacheClearsCwdEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "calls")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\nprintf x >> " + counterFile + "\necho '[]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	seedForgeCacheGitHub(t, core, cwdA)
	seedForgeCacheGitHub(t, core, cwdB)

	// Seed the cache with a "no PR" answer for two cwds.
	core.lookupOpenPR(cwdA, "main")
	core.lookupOpenPR(cwdB, "main")

	// Invalidate cwdA only - cwdB's cache must be untouched.
	core.InvalidatePRCache(cwdA)

	core.lookupOpenPR(cwdA, "main") // miss -> shell out
	core.lookupOpenPR(cwdB, "main") // hit -> no shell out

	calls, _ := os.ReadFile(counterFile)
	if got := len(calls); got != 3 {
		t.Fatalf("gh invocations = %d, want 3 (2 seeds + 1 post-invalidate refetch)", got)
	}
}

func TestLookupOpenPRCachesErrorsBriefly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	counterFile := filepath.Join(binDir, "calls")
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\nprintf x >> " + counterFile + "\necho 'auth required' 1>&2\nexit 1\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	cwd := t.TempDir()
	seedForgeCacheGitHub(t, core, cwd)

	url, number, lookupErr := core.lookupOpenPR(cwd, "main")
	if url != "" || number != 0 {
		t.Fatalf("lookup result = (%q, %d), want empty on error", url, number)
	}
	if lookupErr == "" {
		t.Fatal("expected lookup error")
	}

	if _, _, cachedErr := core.lookupOpenPRCached(cwd, "main"); cachedErr == "" {
		t.Fatal("cached lookup error is empty")
	}
	core.lookupOpenPR(cwd, "main")
	calls, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got := len(calls); got != 1 {
		t.Fatalf("gh invocations before error TTL = %d, want 1", got)
	}

	core.nowFn = func() time.Time { return time.Now().Add(prLookupErrorTTL + time.Second) }
	core.lookupOpenPR(cwd, "main")
	calls, _ = os.ReadFile(counterFile)
	if got := len(calls); got != 2 {
		t.Fatalf("gh invocations after error TTL = %d, want 2", got)
	}
}

// prLookup is one lookupOpenPR result, named so the sticky-cache tests read
// as transitions rather than as three positional returns.
type prLookup struct {
	url    string
	number int
	err    string
}

// prFixture drives lookupOpenPR over a mock `gh` whose next answer is
// switched by writing a mode word to a file, against a Core with a
// controllable clock. Both are needed to exercise *sequences*: the sticky
// last-known-PR behaviour is defined by what a failure does to the result of
// the lookup before it, so state coverage alone would miss it.
type prFixture struct {
	t        *testing.T
	core     *Core
	cwd      string
	modePath string
	now      time.Time
}

const mockGHModes = `#!/bin/sh
case "$(cat "$AO_GH_MODE")" in
  pr7)  echo '[{"url":"https://example.com/pr/7","number":7,"title":"seven","state":"OPEN"}]' ;;
  pr8)  echo '[{"url":"https://example.com/pr/8","number":8,"title":"eight","state":"OPEN"}]' ;;
  none) echo '[]' ;;
  *)    echo 'HTTP 403: API rate limit exceeded' 1>&2; exit 1 ;;
esac
`

func newPRFixture(t *testing.T) *prFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(mockGHModes), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	modePath := filepath.Join(binDir, "mode")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_GH_MODE", modePath)

	f := &prFixture{t: t, core: NewCore(), cwd: t.TempDir(), modePath: modePath, now: time.Now()}
	f.core.nowFn = func() time.Time { return f.now }
	seedForgeCacheGitHub(t, f.core, f.cwd)
	return f
}

// setMode selects what the next `gh pr list` does and advances the clock past
// the success TTL so the call is a genuine re-fetch rather than a cache hit.
func (f *prFixture) setMode(mode string) {
	f.t.Helper()
	if err := os.WriteFile(f.modePath, []byte(mode+"\n"), 0o644); err != nil {
		f.t.Fatalf("write gh mode: %v", err)
	}
	f.expireLookup()
}

// expireLookup steps the clock past both lookup TTLs so the next call
// re-fetches. Each step stays well inside forgeDetectionTTL, so the seeded
// origin identity remains live across a test.
func (f *prFixture) expireLookup() {
	f.now = f.now.Add(prLookupTTL + time.Second)
}

func (f *prFixture) lookup(branch string) prLookup {
	f.t.Helper()
	url, number, err := f.core.lookupOpenPR(f.cwd, branch)
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
// that blanked the badge: a `gh` rate-limit or auth blip in the middle of an
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
