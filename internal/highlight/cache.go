package highlight

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"sync"

	"golang.org/x/sync/singleflight"
)

// Cache is a content-addressed LRU over highlight results. Keys hash
// the full input, so entries never go stale and need no TTL; one entry
// serves both themes because spans are theme-free. Concurrent requests
// for the same input collapse to one computation (singleflight), and
// total concurrent tree-sitter work is bounded by a small semaphore so
// a burst of diff cards cannot fan out unbounded cgo parses.
//
// The App owns one Cache (lazy sync.Once init, same shape as
// mcpStatusCache); this package keeps no global state.
type Cache struct {
	group singleflight.Group
	sem   chan struct{}

	mu       sync.Mutex
	entries  map[[32]byte]*list.Element
	lru      *list.List // front = most recently used
	bytes    int
	maxBytes int
}

const (
	// defaultCacheBytes bounds retained encoded spans (~32 MB ≈ a few
	// hundred large diffs).
	defaultCacheBytes = 32 << 20

	// computeConcurrency bounds simultaneous highlight computations
	// (same bound as attachment thumbnailing).
	computeConcurrency = 4
)

type cacheEntry struct {
	key    [32]byte
	result Result
	size   int
}

// NewCache returns a Cache bounded to the default byte budget.
func NewCache() *Cache {
	return &Cache{
		sem:      make(chan struct{}, computeConcurrency),
		entries:  map[[32]byte]*list.Element{},
		lru:      list.New(),
		maxBytes: defaultCacheBytes,
	}
}

// Code returns spans for a raw source text (markdown code blocks, any
// free-standing text).
func (c *Cache) Code(lang Lang, source string) Result {
	return c.get(cacheKey("code", lang, source), func() Result {
		return Highlight(lang, []byte(source))
	})
}

// CodeTransient is Code without memoization: a hit is served from the
// cache, but a computed result is NOT inserted. Streaming prefixes
// (the highlight seed push re-parses a growing code block every flush
// window) are parsed once and never requested again, so caching them
// would only churn the LRU with dead entries. Shares the compute
// semaphore and in-flight collapse with the cached paths.
func (c *Cache) CodeTransient(lang Lang, source string) Result {
	return c.getWith(cacheKey("code", lang, source), func() Result {
		return Highlight(lang, []byte(source))
	}, false)
}

// Patch returns patch-aligned spans for one file's unified diff.
func (c *Cache) Patch(lang Lang, patch string) Result {
	return c.get(cacheKey("patch", lang, patch), func() Result {
		return HighlightPatchText(lang, patch)
	})
}

// PatchWithContext is Patch primed with file content above each hunk
// (the review pane's fidelity upgrade). The context text is part of
// the key: different scopes/revisions hash apart.
func (c *Cache) PatchWithContext(lang Lang, patch, fileContent string) Result {
	return c.get(cacheKey("patchctx", lang, fileContent, patch), func() Result {
		return HighlightPatchTextPrimed(lang, patch, fileContent)
	})
}

// cacheKey hashes the full input. Parts are length-prefixed — a
// delimiter byte alone is not injective when parts can contain it
// (PatchWithContext hashes two arbitrary strings).
func cacheKey(kind string, lang Lang, parts ...string) [32]byte {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0, byte(lang), byte(lang >> 8)})
	var lenBuf [8]byte
	for _, p := range parts {
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(p)))
		h.Write(lenBuf[:])
		h.Write([]byte(p))
	}
	var key [32]byte
	h.Sum(key[:0])
	return key
}

func (c *Cache) get(key [32]byte, compute func() Result) Result {
	return c.getWith(key, compute, true)
}

func (c *Cache) getWith(key [32]byte, compute func() Result, memoize bool) Result {
	if res, ok := c.lookup(key); ok {
		return res
	}
	groupKey := string(key[:])
	v, _, _ := c.group.Do(groupKey, func() (any, error) {
		// A previous winner may have inserted between our miss and
		// joining the flight.
		if res, ok := c.lookup(key); ok {
			return res, nil
		}
		c.sem <- struct{}{}
		res := compute()
		<-c.sem
		return res, nil
	})
	res := v.(Result)
	// Insertion is per caller, outside the flight closure: a transient
	// (non-memoizing) leader can be joined by a memoizing caller whose
	// cache-warming promise must hold regardless of who led the flight;
	// insert() dedupes when several memoizing waiters land. A failed
	// parse (timeout under load, parser error) degrades to plain but
	// can succeed on retry — memoizing it would pin the content plain
	// for the process lifetime.
	if memoize && !res.Incomplete {
		c.insert(key, res)
	}
	return res
}

func (c *Cache) lookup(key [32]byte) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return Result{}, false
	}
	c.lru.MoveToFront(elem)
	return elem.Value.(*cacheEntry).result, true
}

func (c *Cache) insert(key [32]byte, res Result) {
	size := resultBytes(res)
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		c.lru.MoveToFront(elem)
		return
	}
	if size > c.maxBytes {
		return // pathological single entry; recompute on demand
	}
	c.entries[key] = c.lru.PushFront(&cacheEntry{key: key, result: res, size: size})
	c.bytes += size
	for c.bytes > c.maxBytes {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		c.lru.Remove(oldest)
		delete(c.entries, entry.key)
		c.bytes -= entry.size
	}
}

// resultBytes estimates an entry's retained size: run pairs are
// uint16s, plus per-line and per-entry overhead.
func resultBytes(res Result) int {
	size := 64
	for _, line := range res.Lines {
		size += 24 + 2*len(line.Runs)
	}
	return size
}
