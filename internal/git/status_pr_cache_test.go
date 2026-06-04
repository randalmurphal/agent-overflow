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
	core.storeForgeCache(cwd, "github", core.nowFn())
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

	url, number := core.lookupOpenPR(cwd, "main")
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
	if url, _ := core.lookupOpenPR(cwd, "feat-a"); url == "" {
		t.Fatalf("cold lookup returned empty url")
	}
	// Subsequent calls within TTL: warm cache -> no shell out.
	for i := 0; i < 5; i++ {
		if url, _ := core.lookupOpenPR(cwd, "feat-a"); url == "" {
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
	if url, _ := core.lookupOpenPR(cwd, "feat-b"); url == "" {
		t.Fatalf("different-branch lookup returned empty url")
	}
	calls, _ = os.ReadFile(counterFile)
	if got := len(calls); got != 2 {
		t.Fatalf("after different-branch lookup: gh invocations = %d, want 2", got)
	}

	// TTL expiry -> fresh shell out. Drive nowFn forward past the TTL.
	core.nowFn = func() time.Time { return time.Now().Add(prLookupTTL + time.Second) }
	if url, _ := core.lookupOpenPR(cwd, "feat-a"); url == "" {
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
