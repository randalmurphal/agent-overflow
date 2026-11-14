package highlight

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rssAnon is the process's resident anonymous memory in bytes.
func rssAnon(t *testing.T) int64 {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if kb, ok := strings.CutPrefix(line, "RssAnon:"); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(kb, "kB")), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n << 10
		}
	}
	t.Fatal("no RssAnon in /proc/self/status")
	return 0
}

// A burst of large parses must not leave its C heap resident once the
// highlighter is idle.
func TestCacheReturnsFreedParseMemoryOnceIdle(t *testing.T) {
	langs := []Lang{LangGo, LangTypeScript, LangPython, LangRust}
	sources := make([]string, 0, 2*len(langs))
	for _, lang := range langs {
		engineFor(lang)
		for variant := range 2 {
			var b strings.Builder
			for i := 0; b.Len() < 256<<10; i++ {
				fmt.Fprintf(&b, "const v%d_%d = { a: %d, b: 'x', c: [1, 2, 3] }; // line %d\n", variant, i, i, i)
			}
			sources = append(sources, b.String())
		}
	}
	// Every Cache releases the whole process's C heap up to releaseIdle
	// after its last computation. A release that an earlier test's cache
	// armed fires before the baseline, not during the burst.
	time.Sleep(releaseIdle + 250*time.Millisecond)
	releaseFreedMemory()
	debug.FreeOSMemory()
	base := rssAnon(t)

	// The burst must free a measurable heap before any release runs, at
	// any host speed: every parse runs to completion, since one cut off by
	// parseTimeout frees its partial tree early, and the release that
	// sustained work forces after maxDelay cannot run during a slow burst.
	prevTimeout := parseTimeout
	parseTimeout = 0
	defer func() { parseTimeout = prevTimeout }()
	cache := NewCache()
	cache.heap.maxDelay = time.Hour
	var wg sync.WaitGroup
	incomplete := make([]bool, len(sources))
	for i, source := range sources {
		wg.Go(func() { incomplete[i] = cache.CodeTransient(langs[i/2], source).Incomplete })
	}
	wg.Wait()
	for i, failed := range incomplete {
		if failed {
			t.Fatalf("burst parse %d did not complete", i)
		}
	}
	debug.FreeOSMemory()
	retained := rssAnon(t) - base
	const limit = 32 << 20
	// A burst that reused memory the baseline already held proves nothing.
	if retained < limit {
		t.Fatalf("the burst grew resident memory by %d MiB, want at least %d MiB for its release to be measurable", retained>>20, limit>>20)
	}

	deadline := time.Now().Add(releaseIdle + 5*time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		debug.FreeOSMemory()
		after := rssAnon(t) - base
		if after < limit {
			t.Logf("resident growth %d MiB after the burst, %d MiB once idle", retained>>20, after>>20)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("resident growth %d MiB after the burst, still %d MiB once idle, want under %d MiB", retained>>20, after>>20, limit>>20)
		}
	}
}
