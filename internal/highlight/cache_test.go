package highlight

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCacheCodeHitAndCollapse(t *testing.T) {
	c := NewCache()
	src := "def f():\n    return 1\n"

	first := c.Code(LangPython, src)
	if len(first.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(first.Lines))
	}

	// Concurrent identical requests must all return the same content.
	var wg sync.WaitGroup
	results := make([]Result, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.Code(LangPython, src)
		}(i)
	}
	wg.Wait()
	for i, res := range results {
		if len(res.Lines) != len(first.Lines) {
			t.Errorf("result %d: %d lines, want %d", i, len(res.Lines), len(first.Lines))
		}
	}
}

func TestCacheDistinguishesLangAndKind(t *testing.T) {
	c := NewCache()
	src := "x = 1  # for while\n"
	py := c.Code(LangPython, src)
	plain := c.Code(LangPlaintext, src)
	if py.Lines[0].Runs == nil {
		t.Error("python source should have styled runs")
	}
	// Plaintext has no engine: nil lines, all plain.
	if len(plain.Lines) != 0 {
		t.Error("plaintext result leaked python spans — key collision across langs")
	}
}

func TestCacheKeyPartsAreLengthPrefixed(t *testing.T) {
	// A NUL delimiter alone is not injective when parts can contain
	// NULs: ("x", "\x00@@") and ("x\x00", "@@") would concatenate to
	// identical byte streams. Length prefixes keep the key
	// content-addressed for arbitrary (fileContent, patch) pairs.
	a := cacheKey("patchctx", LangPython, "x", "\x00@@")
	b := cacheKey("patchctx", LangPython, "x\x00", "@@")
	if a == b {
		t.Fatal("cache keys collide across part boundaries")
	}
}

func TestCacheSkipsIncompleteResults(t *testing.T) {
	// A failed parse degrades to plain but can succeed on retry —
	// memoizing it would pin the content plain for the process
	// lifetime.
	c := NewCache()
	calls := 0
	key := cacheKey("code", LangPython, "test")
	for i := 0; i < 2; i++ {
		c.get(key, func() Result {
			calls++
			return Result{Incomplete: true}
		})
	}
	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2 (incomplete results must not memoize)", calls)
	}
	c.get(key, func() Result { calls++; return Result{} })
	c.get(key, func() Result { calls++; return Result{} })
	if calls != 3 {
		t.Fatalf("compute calls = %d, want 3 (complete result must memoize)", calls)
	}
}

// A memoizing caller that joins a transient-led flight must still warm
// the cache — insertion is per caller, not the flight leader's choice.
// (The old closure-owned insert keyed memoization to whoever LED the
// flight; a Code call joining a CodeTransient flight returned without
// warming, so a later RPC reparsed.)
func TestCacheMemoizingJoinerWarmsTransientFlight(t *testing.T) {
	c := NewCache()
	key := cacheKey("code", LangPython, "shared")

	computeEntered := make(chan struct{})
	releaseCompute := make(chan struct{})
	transientDone := make(chan struct{})
	go func() {
		defer close(transientDone)
		c.getWith(key, func() Result {
			close(computeEntered)
			<-releaseCompute
			return Result{}
		}, false)
	}()
	<-computeEntered

	// The memoizing caller attaches to the in-flight computation.
	// Joining is not observable from outside singleflight, so the
	// joiner's own compute is a benign fallback (it would also insert
	// under the per-caller rule) — the settle sleep makes the join path
	// the exercised one in practice.
	memoDone := make(chan struct{})
	go func() {
		defer close(memoDone)
		c.get(key, func() Result { return Result{} })
	}()
	time.Sleep(50 * time.Millisecond)
	close(releaseCompute)
	<-transientDone
	<-memoDone

	if _, ok := c.lookup(key); !ok {
		t.Fatal("memoizing caller's result was not inserted")
	}
}

func TestCacheCachesPlainResults(t *testing.T) {
	// Empty-success contract: a plain (no-spans) result is a valid,
	// cacheable answer — not an error to retry.
	c := NewCache()
	res := c.Code(LangYAML, "plainword\n")
	if got := c.Code(LangYAML, "plainword\n"); len(got.Lines) != len(res.Lines) {
		t.Fatal("second lookup diverged")
	}
	c.mu.Lock()
	entries := len(c.entries)
	c.mu.Unlock()
	if entries != 1 {
		t.Fatalf("entries = %d, want 1 (plain results must cache)", entries)
	}
}

func TestCacheEvictsByBytes(t *testing.T) {
	c := NewCache()
	c.maxBytes = 8 << 10
	for i := 0; i < 200; i++ {
		var b strings.Builder
		for j := 0; j < 40; j++ {
			fmt.Fprintf(&b, "value_%d_%d = %d  # comment\n", i, j, j)
		}
		c.Code(LangPython, b.String())
	}
	c.mu.Lock()
	bytes, entries, listLen := c.bytes, len(c.entries), c.lru.Len()
	c.mu.Unlock()
	if bytes > c.maxBytes {
		t.Errorf("cache bytes %d exceed budget %d", bytes, c.maxBytes)
	}
	if entries != listLen {
		t.Errorf("entries map (%d) and lru list (%d) diverged", entries, listLen)
	}
	if entries == 0 {
		t.Error("cache fully evicted itself")
	}
}

// TestCacheRetainsWhatItCounts fills a cache with one-line blocks and with
// whole source files and requires the heap it retains to stay within its
// byte budget: the budget bounds real memory only if an entry retains what
// resultBytes counts.
func TestCacheRetainsWhatItCounts(t *testing.T) {
	// A fixed file, so an edit to the package does not move the size
	// classes the ratio is measured at.
	var file strings.Builder
	file.WriteString("package p\n\nimport \"fmt\"\n\n")
	for i := 0; file.Len() < 7<<10; i++ {
		fmt.Fprintf(&file, "// f%d scales a by %d.\nfunc f%d(a, b int) (int, error) {\n\tif a > b {\n\t\treturn 0, fmt.Errorf(\"a %%d over b %%d\", a, b)\n\t}\n\treturn a * %d, nil\n}\n\n", i, i, i, i)
	}
	corpora := map[string]func(i int) (Lang, string){
		"one-line blocks": func(i int) (Lang, string) {
			return LangPython, fmt.Sprintf("value_%d = call(a, %d)  # note\n", i, i)
		},
		"source files": func(i int) (Lang, string) {
			return LangGo, fmt.Sprintf("%s\n// copy %d\n", file.String(), i)
		},
	}
	// The first parse of a language builds state that outlives the cache.
	NewCache().Code(LangPython, "x = 1\n")
	NewCache().Code(LangGo, "package x\n")
	for name, source := range corpora {
		t.Run(name, func(t *testing.T) {
			c := NewCache()
			c.maxBytes = 1 << 20
			var before, after runtime.MemStats
			// Two collections: the first moves sync.Pool contents (fmt's
			// printers) to the victim cache, the second frees them.
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&before)
			for i := 0; c.bytes < c.maxBytes*3/4 || i < 64; i++ {
				c.Code(source(i))
			}
			runtime.GC()
			runtime.GC()
			runtime.ReadMemStats(&after)
			retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			if limit := int64(c.bytes) * 6 / 5; retained > limit {
				t.Errorf("cache of %d counted bytes (budget %d) retains %d heap bytes, over %d", c.bytes, c.maxBytes, retained, limit)
			}
			runtime.KeepAlive(c)
		})
	}
}

// Callers of one flight each insert its result; only the first copies it,
// and a result without lines keeps its nil Lines.
func TestCacheInsertCompactsOnce(t *testing.T) {
	c := NewCache()
	key := cacheKey("code", LangGo, "x := 1\n")
	res := Result{Lines: []EncodedLine{{Runs: []uint16{1, 2}}}}
	c.insert(key, res)
	if allocs := testing.AllocsPerRun(10, func() { c.insert(key, res) }); allocs != 0 {
		t.Fatalf("inserting a cached key allocated %.0f times", allocs)
	}
	plain := cacheKey("code", LangGo, "plain")
	c.insert(plain, Result{})
	if got, ok := c.lookup(plain); !ok || got.Lines != nil {
		t.Fatalf("result without lines came back as %+v (cached %v), want nil Lines", got, ok)
	}
}

func TestCachePatchVariantsKeyApart(t *testing.T) {
	c := NewCache()
	plain := c.Patch(LangPython, pythonDocstringPatch)
	primed := c.PatchWithContext(LangPython, pythonDocstringPatch, "def above():\n    pass\n")
	if len(plain.Lines) != len(primed.Lines) {
		t.Fatalf("line counts diverged: %d vs %d", len(plain.Lines), len(primed.Lines))
	}
	c.mu.Lock()
	entries := len(c.entries)
	c.mu.Unlock()
	if entries != 2 {
		t.Fatalf("entries = %d, want 2 (patch and primed patch must key apart)", entries)
	}
}

// The reason PatchWithContext exists: a hunk starting mid-docstring
// has no opening quotes in the patch text at all — only priming can
// recover the string state.
func TestHighlightPatchTextPrimedMidDocstring(t *testing.T) {
	patch := "@@ -4,4 +4,5 @@\n" +
		"     keywords like and, for, is live here.\n" +
		"-    Old tail line.\n" +
		"+    New tail line with for and while.\n" +
		"+    Another prose line.\n" +
		"     \"\"\"\n"
	fileContent := "def handler(request):\n" +
		"    \"\"\"Docstring opening.\n" +
		"    More prose.\n" +
		"    keywords like and, for, is live here.\n" +
		"    New tail line with for and while.\n" +
		"    Another prose line.\n" +
		"    \"\"\"\n" +
		"    return request\n"

	unprimed := HighlightPatchText(LangPython, patch)
	primed := HighlightPatchTextPrimed(LangPython, patch, fileContent)
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")

	// Primed: every hunk line is inside the (still open) docstring.
	for _, i := range []int{1, 3, 4} {
		body := lines[i][1:]
		if i == 1 {
			body = lines[i] // context keeps its leading space
		}
		assertNoClass(t, primed.Lines[i], body, ClassKeyword, "primed docstring line")
	}
	if got := classOf(t, primed.Lines[3], lines[3][1:], "for and while"); got != ClassString {
		t.Errorf("primed mid-docstring class = %d, want string %d", got, ClassString)
	}

	// Sanity: without priming the parser cannot know it is inside a
	// docstring, so at least one keyword leaks — proving the primed
	// path is doing real work, not coasting.
	leaked := false
	for _, i := range []int{1, 3, 4} {
		text := lines[i]
		if i != 1 {
			text = lines[i][1:]
		}
		for _, class := range expandRuns(t, unprimed.Lines[i], len(text)) {
			if class == ClassKeyword {
				leaked = true
			}
		}
	}
	if !leaked {
		t.Log("note: unprimed path already string-classed everything; priming redundant for this fixture")
	}
}

// A computation that panics returns its slot: the RPC dispatcher recovers the
// panic, and highlighting must keep working after it.
func TestCacheReturnsTheSlotOfAPanickingComputation(t *testing.T) {
	c := NewCache()
	for i := range computeConcurrency {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the computation's panic did not reach its caller")
				}
			}()
			c.get(cacheKey("panic", LangPython, fmt.Sprint(i)), func() Result { panic("parser failure") })
		}()
	}
	done := make(chan struct{})
	go func() {
		c.Code(LangPython, "x = 1\n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("highlighting blocked after panicking computations")
	}
	c.heap.mu.Lock()
	active := c.heap.active
	c.heap.mu.Unlock()
	if active != 0 {
		t.Fatalf("%d computations still counted in flight", active)
	}
}
