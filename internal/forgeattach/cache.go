package forgeattach

import (
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Entry is one fetched attachment held for the byte route.
//
// Data is NOT copied on the way in or out: the cache is the only owner
// after Put, and a reader must treat the slice as immutable. Copying a
// 100 MiB video twice per render is the cost this avoids, and there is
// no writer on either side to protect against.
type Entry struct {
	// ID is assigned by Put and is what the ticketed URL names. 16
	// random bytes in unpadded base64url, so the whole id is [A-Za-z0-9_-]
	// and needs no escaping anywhere it travels.
	ID string
	// Key is the caller's dedupe key, normally CacheKey(...). Empty means
	// the entry is reachable by id alone.
	Key      string
	Data     []byte
	MimeType string
	Kind     string
	Filename string
	// StoredAt is set by Put and backs the route's Last-Modified.
	StoredAt time.Time
}

// ErrTooLargeToCache reports a body the cache could never hold, which is
// a different failure from an eviction: no amount of room would help.
var ErrTooLargeToCache = errors.New("attachment is too large to cache")

// Cache is a bounded, TTL'd, LRU byte cache for fetched attachments.
//
// Bounded because the bytes are held for a route rather than for a
// render: nothing downstream frees them, so the bound is the only thing
// that does. TTL'd because a review pane that closed should not pin a
// video for the life of the process, and the frontend re-fetches through
// the same bound method when an expired id 404s.
//
// Expiry is reclaimed on every operation rather than by a timer. A timer
// would be a goroutine with a lifetime to own for a cache that is empty
// most of the time; the cost of this is that a process which touches the
// cache once and then never again holds that entry until it does.
type Cache struct {
	maxBytes int64
	ttl      time.Duration
	// now is injectable so tests move time instead of sleeping.
	now func() time.Time

	mu    sync.Mutex
	order *list.List // front = most recently used; values are *Entry
	byID  map[string]*list.Element
	byKey map[string]*list.Element
	bytes int64
}

// NewCache returns an empty cache. A non-positive maxBytes or ttl falls
// back to the package defaults rather than producing a cache that
// refuses or expires everything.
func NewCache(maxBytes int64, ttl time.Duration) *Cache {
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache{
		maxBytes: maxBytes,
		ttl:      ttl,
		now:      time.Now,
		order:    list.New(),
		byID:     make(map[string]*list.Element),
		byKey:    make(map[string]*list.Element),
	}
}

// Put stores one entry and returns it with ID and StoredAt filled in.
//
// An entry whose Key is already held REPLACES it under a fresh id. The
// previous id stops resolving, which is correct: the only way to reach
// this path is a re-fetch, and a re-fetch means the caller wants the
// bytes it just read rather than the ones it read before.
func (c *Cache) Put(entry Entry) (Entry, error) {
	if len(entry.Data) == 0 {
		return Entry{}, errors.New("attachment body is empty")
	}
	size := int64(len(entry.Data))
	if size > c.maxBytes {
		return Entry{}, fmt.Errorf("%w: %d bytes exceeds the %d byte cache", ErrTooLargeToCache, size, c.maxBytes)
	}
	id, err := newContentID()
	if err != nil {
		return Entry{}, err
	}
	entry.ID = id

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	entry.StoredAt = now
	c.dropExpiredLocked(now)
	if entry.Key != "" {
		if element, ok := c.byKey[entry.Key]; ok {
			c.removeLocked(element)
		}
	}
	for c.bytes+size > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeLocked(oldest)
	}
	stored := entry
	element := c.order.PushFront(&stored)
	c.byID[stored.ID] = element
	if stored.Key != "" {
		c.byKey[stored.Key] = element
	}
	c.bytes += size
	return stored, nil
}

// Get resolves one entry by id, refreshing its position. An expired
// entry is dropped and reported missing.
func (c *Cache) Get(id string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return c.touchLocked(c.byID[id])
}

// Lookup resolves one entry by the caller's dedupe key, with the same
// expiry and LRU handling Get has.
func (c *Cache) Lookup(key string) (Entry, bool) {
	if key == "" {
		return Entry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return c.touchLocked(c.byKey[key])
}

// Bytes reports the currently retained payload total, after reclaiming
// anything expired. Tests and diagnostics only.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return c.bytes
}

func (c *Cache) touchLocked(element *list.Element) (Entry, bool) {
	if element == nil {
		return Entry{}, false
	}
	entry := element.Value.(*Entry)
	if c.now().Sub(entry.StoredAt) >= c.ttl {
		c.removeLocked(element)
		return Entry{}, false
	}
	c.order.MoveToFront(element)
	return *entry, true
}

func (c *Cache) dropExpiredLocked(now time.Time) {
	for element := c.order.Back(); element != nil; {
		previous := element.Prev()
		if now.Sub(element.Value.(*Entry).StoredAt) >= c.ttl {
			c.removeLocked(element)
		}
		element = previous
	}
}

func (c *Cache) removeLocked(element *list.Element) {
	entry := element.Value.(*Entry)
	c.order.Remove(element)
	delete(c.byID, entry.ID)
	if entry.Key != "" && c.byKey[entry.Key] == element {
		delete(c.byKey, entry.Key)
	}
	c.bytes -= int64(len(entry.Data))
}

// newContentID mints the id the ticketed URL carries. Unpadded base64url
// keeps it inside [A-Za-z0-9_-], which is the character class the route's
// path segment and the frontend's URL check both admit without escaping.
func newContentID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("forgeattach: generate content id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
