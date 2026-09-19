package forgeattach

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestCache(maxBytes int64, ttl time.Duration) (*Cache, *time.Time) {
	clock := time.Unix(1_700_000_000, 0)
	cache := NewCache(maxBytes, ttl)
	cache.now = func() time.Time { return clock }
	return cache, &clock
}

func put(t *testing.T, cache *Cache, key string, size int) Entry {
	t.Helper()
	entry, err := cache.Put(Entry{Key: key, Data: make([]byte, size), MimeType: "image/png", Kind: KindImage, Filename: key})
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	return entry
}

// TestCacheEvictsLeastRecentlyUsed: the bound is on BYTES, and the entry
// that goes is the one nobody has read for longest — not the oldest, or
// a read would not protect anything.
func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache, _ := newTestCache(30, time.Hour)
	a := put(t, cache, "a", 10)
	b := put(t, cache, "b", 10)
	if _, ok := cache.Get(a.ID); !ok {
		t.Fatal("a is missing before any eviction")
	}
	c := put(t, cache, "c", 10)
	// Full at 30 bytes; the next 10 evicts b, which a's Get just
	// demoted to least recently used.
	d := put(t, cache, "d", 10)

	if _, ok := cache.Get(b.ID); ok {
		t.Error("b survived: eviction is by insertion order, not by use")
	}
	for _, kept := range []Entry{a, c, d} {
		if _, ok := cache.Get(kept.ID); !ok {
			t.Errorf("%s was evicted while a less recently used entry remained", kept.Filename)
		}
	}
	if got := cache.Bytes(); got != 30 {
		t.Errorf("Bytes() = %d, want 30", got)
	}
}

func TestCacheExpiresEntries(t *testing.T) {
	cache, clock := newTestCache(1_000, time.Minute)
	entry := put(t, cache, "a", 10)

	*clock = clock.Add(59 * time.Second)
	if _, ok := cache.Get(entry.ID); !ok {
		t.Fatal("entry expired before its TTL")
	}
	*clock = clock.Add(time.Second)
	if _, ok := cache.Get(entry.ID); ok {
		t.Fatal("entry survived its TTL")
	}
	// The bytes go with it: an expiry that only hid the entry would
	// leave the bound counting payload nothing can reach.
	if got := cache.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d after expiry, want 0", got)
	}
}

// TestCachePutSweepsExpiredEntries: nothing calls Get on an entry nobody
// wants, so a Put has to be what reclaims it.
func TestCachePutSweepsExpiredEntries(t *testing.T) {
	cache, clock := newTestCache(1_000, time.Minute)
	put(t, cache, "stale", 100)
	*clock = clock.Add(2 * time.Minute)
	put(t, cache, "fresh", 10)
	if got := cache.Bytes(); got != 10 {
		t.Fatalf("Bytes() = %d, want 10: the expired entry was never reclaimed", got)
	}
}

func TestCacheRefusesAnEntryLargerThanItself(t *testing.T) {
	cache, _ := newTestCache(100, time.Hour)
	_, err := cache.Put(Entry{Key: "a", Data: make([]byte, 101)})
	if !errors.Is(err, ErrTooLargeToCache) {
		t.Fatalf("Put returned %v, want ErrTooLargeToCache", err)
	}
	// Refused, not partially applied.
	if got := cache.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d after a refusal, want 0", got)
	}
	if _, err := cache.Put(Entry{Key: "a", Data: nil}); err == nil {
		t.Fatal("Put accepted an empty body")
	}
}

// TestCacheLookupDedupesByKey is the whole reason Lookup exists: a body
// that references the same image three times must spawn one forge CLI.
func TestCacheLookupDedupesByKey(t *testing.T) {
	cache, _ := newTestCache(1_000, time.Hour)
	key := CacheKey("gitlab", "g/r", 7, "/uploads/x/a.png")
	stored := put(t, cache, key, 10)

	found, ok := cache.Lookup(key)
	if !ok {
		t.Fatal("Lookup missed the key it was stored under")
	}
	if found.ID != stored.ID {
		t.Fatalf("Lookup returned id %q, want %q", found.ID, stored.ID)
	}
	if _, ok := cache.Lookup(""); ok {
		t.Fatal("Lookup answered for the empty key")
	}
	if _, ok := cache.Lookup("never stored"); ok {
		t.Fatal("Lookup answered for a key nothing stored")
	}

	// A re-fetch replaces rather than accumulates: same key, one entry,
	// and the previous id stops resolving.
	replaced := put(t, cache, key, 20)
	if replaced.ID == stored.ID {
		t.Fatal("replacing an entry reused its id")
	}
	if _, ok := cache.Get(stored.ID); ok {
		t.Fatal("the replaced entry is still reachable by its old id")
	}
	if got := cache.Bytes(); got != 20 {
		t.Fatalf("Bytes() = %d after a replacement, want 20", got)
	}
}

// TestCacheIDsAreURLSafe pins the character class the route's path
// segment and the frontend's transfer-URL check both admit.
func TestCacheIDsAreURLSafe(t *testing.T) {
	cache, _ := newTestCache(1_000_000, time.Hour)
	safe := regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	seen := map[string]bool{}
	for i := range 200 {
		entry := put(t, cache, fmt.Sprintf("k%d", i), 1)
		if !safe.MatchString(entry.ID) {
			t.Fatalf("id %q is outside [A-Za-z0-9_-]", entry.ID)
		}
		if seen[entry.ID] {
			t.Fatalf("id %q was issued twice", entry.ID)
		}
		seen[entry.ID] = true
	}
}

// TestCacheIsConcurrentlyUsable runs under -race: the route reads while
// bound methods write, and every path mutates the LRU list.
func TestCacheIsConcurrentlyUsable(t *testing.T) {
	cache := NewCache(64<<10, time.Minute)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				key := fmt.Sprintf("w%d-%d", worker, i%7)
				entry, err := cache.Put(Entry{Key: key, Data: make([]byte, 512), Kind: KindImage})
				if err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				cache.Get(entry.ID)
				cache.Lookup(key)
				cache.Bytes()
			}
		}()
	}
	wg.Wait()
	if got := cache.Bytes(); got < 0 || got > 64<<10 {
		t.Fatalf("Bytes() = %d, outside the cache bound", got)
	}
}

func TestCacheDefaultsReplaceNonPositiveLimits(t *testing.T) {
	cache := NewCache(0, 0)
	if cache.maxBytes != DefaultCacheBytes || cache.ttl != DefaultTTL {
		t.Fatalf("NewCache(0, 0) = (%d, %s), want the package defaults", cache.maxBytes, cache.ttl)
	}
}

func TestCacheErrorNamesTheLimit(t *testing.T) {
	cache, _ := newTestCache(100, time.Hour)
	_, err := cache.Put(Entry{Data: make([]byte, 200)})
	if err == nil || !strings.Contains(err.Error(), "200") {
		t.Fatalf("Put error = %v, want it to name the payload size", err)
	}
}
