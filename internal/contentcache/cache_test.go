package contentcache

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestCache(maxBytes int64, ttl time.Duration) (*Cache[[]byte], *time.Time) {
	clock := time.Unix(1_700_000_000, 0)
	cache := New(Config[[]byte]{
		MaxBytes: maxBytes,
		TTL:      ttl,
		Size:     func(v []byte) int64 { return int64(len(v)) },
		Now:      func() time.Time { return clock },
	})
	return cache, &clock
}

func put(t *testing.T, cache *Cache[[]byte], key string, size int) Entry[[]byte] {
	t.Helper()
	entry, err := cache.Put(key, make([]byte, size))
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	return entry
}

// The bound is on BYTES, and the entry that goes is the one nobody has read
// for longest, not the oldest, or a read would not protect anything.
func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache, _ := newTestCache(30, time.Hour)
	a := put(t, cache, "a", 10)
	b := put(t, cache, "b", 10)
	if _, ok := cache.Get(a.ID); !ok {
		t.Fatal("a is missing before any eviction")
	}
	c := put(t, cache, "c", 10)
	// Full at 30 bytes; the next 10 evicts b, which a's Get just demoted to
	// least recently used.
	d := put(t, cache, "d", 10)

	if _, ok := cache.Get(b.ID); ok {
		t.Error("b survived: eviction is by insertion order, not by use")
	}
	for _, kept := range []Entry[[]byte]{a, c, d} {
		if _, ok := cache.Get(kept.ID); !ok {
			t.Errorf("%s was evicted while a less recently used entry remained", kept.Key)
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
	// The bytes go with it: an expiry that only hid the entry would leave
	// the bound counting values nothing can reach.
	if got := cache.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d after expiry, want 0", got)
	}
}

// Nothing calls Get on an entry nobody wants, so a Put has to be what
// reclaims it.
func TestCachePutSweepsExpiredEntries(t *testing.T) {
	cache, clock := newTestCache(1_000, time.Minute)
	put(t, cache, "stale", 100)
	*clock = clock.Add(2 * time.Minute)
	put(t, cache, "fresh", 10)
	if got := cache.Bytes(); got != 10 {
		t.Fatalf("Bytes() = %d, want 10: the expired entry was never reclaimed", got)
	}
}

func TestCacheRefusesValuesItCannotBound(t *testing.T) {
	cache, _ := newTestCache(100, time.Hour)
	_, err := cache.Put("a", make([]byte, 101))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Put returned %v, want ErrTooLarge", err)
	}
	if err == nil || !strings.Contains(err.Error(), "101") {
		t.Fatalf("Put error = %v, want it to name the value's size", err)
	}
	// Refused, not partially applied.
	if got := cache.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d after a refusal, want 0", got)
	}
	if _, err := cache.Put("a", nil); err == nil {
		t.Fatal("Put accepted a value the bound could never evict")
	}
}

func TestCacheLookupFindsByKey(t *testing.T) {
	cache, _ := newTestCache(1_000, time.Hour)
	stored := put(t, cache, "k", 10)

	found, ok := cache.Lookup("k")
	if !ok || found.ID != stored.ID {
		t.Fatalf("Lookup = (%v, %v), want the entry stored under the key", found.ID, ok)
	}
	if _, ok := cache.Lookup(""); ok {
		t.Fatal("Lookup answered for the empty key")
	}
	if _, ok := cache.Lookup("never stored"); ok {
		t.Fatal("Lookup answered for a key nothing stored")
	}
}

// Two callers that both missed Lookup resolve the same content and both Put
// it. The second must not retire the id the first already handed out, or
// that client's ticketed GET answers 404 for content the cache holds.
func TestCachePutKeepsALiveEntryUnderTheSameKey(t *testing.T) {
	cache, clock := newTestCache(1_000, time.Minute)
	first := put(t, cache, "k", 10)
	second := put(t, cache, "k", 20)
	if second.ID != first.ID {
		t.Fatalf("second Put answered id %q, want the held %q", second.ID, first.ID)
	}
	if _, ok := cache.Get(first.ID); !ok {
		t.Fatal("the first caller's id stopped resolving")
	}
	if got := cache.Bytes(); got != 10 {
		t.Fatalf("Bytes() = %d, want 10: the duplicate was stored as well", got)
	}

	// Once the held entry expires, a Put under the key is a re-resolve and
	// replaces it under a fresh id.
	*clock = clock.Add(time.Minute)
	replaced := put(t, cache, "k", 20)
	if replaced.ID == first.ID {
		t.Fatal("a Put after expiry reused the expired id")
	}
	if got := cache.Bytes(); got != 20 {
		t.Fatalf("Bytes() = %d after the replacement, want 20", got)
	}
}

// Remove retires one id: its key misses afterwards, its bytes are released,
// and a stale id does not reach the entry a later Put stored under the key.
func TestCacheRemoveDropsOneEntryByID(t *testing.T) {
	cache, _ := newTestCache(1_000, time.Hour)
	first := put(t, cache, "k", 10)
	other := put(t, cache, "other", 5)
	cache.Remove(first.ID)
	if _, ok := cache.Get(first.ID); ok {
		t.Fatal("the removed id still resolves")
	}
	if _, ok := cache.Lookup("k"); ok {
		t.Fatal("the removed entry's key still resolves")
	}
	if got := cache.Bytes(); got != 5 {
		t.Fatalf("Bytes() = %d after Remove, want 5", got)
	}
	replaced := put(t, cache, "k", 20)
	cache.Remove(first.ID)
	cache.Remove("never-stored")
	for _, kept := range []Entry[[]byte]{replaced, other} {
		if _, ok := cache.Get(kept.ID); !ok {
			t.Fatalf("Remove of a stale id dropped %q", kept.Key)
		}
	}
	if got := cache.Bytes(); got != 25 {
		t.Fatalf("Bytes() = %d, want 25", got)
	}
}

// Pins the character class the routes' path segment and the frontend's
// transfer-URL check both admit.
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

// Runs under -race: a route reads while bound methods write, and every path
// mutates the LRU list.
func TestCacheIsConcurrentlyUsable(t *testing.T) {
	cache := New(Config[[]byte]{
		MaxBytes: 64 << 10,
		TTL:      time.Minute,
		Size:     func(v []byte) int64 { return int64(len(v)) },
	})
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				key := fmt.Sprintf("w%d-%d", worker, i%7)
				entry, err := cache.Put(key, make([]byte, 512))
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

func TestNewRefusesAnUnboundedCache(t *testing.T) {
	size := func(v []byte) int64 { return int64(len(v)) }
	for name, cfg := range map[string]Config[[]byte]{
		"no bytes": {TTL: time.Minute, Size: size},
		"no ttl":   {MaxBytes: 1, Size: size},
		"no size":  {MaxBytes: 1, TTL: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New accepted a cache it cannot bound")
				}
			}()
			New(cfg)
		})
	}
}
