// Package contentcache is the bounded, TTL'd, LRU cache behind the ticketed
// byte routes: a bound method stores what it resolved, the route serves it by
// an opaque random content id, and a caller can find it again by its own
// dedupe key.
package contentcache

import (
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Entry is one cached value with the identifiers the cache assigned.
//
// Value is NOT copied on the way in or out: the cache is the only owner
// after Put, and a reader must treat anything it references (a byte slice)
// as immutable. Copying a 100 MiB video twice per render is the cost this
// avoids, and there is no writer on either side to protect against.
type Entry[V any] struct {
	// ID is assigned by Put and is what a ticketed URL names. 16 random
	// bytes in unpadded base64url, so the whole id is [A-Za-z0-9_-] and
	// needs no escaping anywhere it travels.
	ID string
	// Key is the caller's dedupe key. Empty means the entry is reachable
	// by id alone.
	Key   string
	Value V
	// StoredAt is set by Put and backs a route's Last-Modified.
	StoredAt time.Time
}

// ErrTooLarge reports a value the cache could never hold, which is a
// different failure from an eviction: no amount of room would help.
var ErrTooLarge = errors.New("too large to cache")

// Config sizes one cache.
type Config[V any] struct {
	// MaxBytes bounds the summed Size of every held value.
	MaxBytes int64
	// TTL is how long a value stays resolvable after Put.
	TTL time.Duration
	// Size weighs one value against MaxBytes and must answer the same for a
	// value every time it is asked. Put refuses a value it weighs at zero or
	// less: the bound is the only thing that frees entries, and a weightless
	// one would never be evicted by it.
	Size func(V) int64
	// Now defaults to time.Now; tests move time instead of sleeping.
	Now func() time.Time
}

// Cache is bounded because values are held for a route rather than for a
// render: nothing downstream frees them, so the bound is the only thing that
// does. TTL'd because a pane that closed should not pin bytes for the life
// of the process, and a client re-resolves through the same bound method
// when an expired id 404s.
//
// Expiry is reclaimed on every operation rather than by a timer. A timer
// would be a goroutine with a lifetime to own for a cache that is empty most
// of the time; the cost is that a process which touches the cache once and
// then never again holds that entry until it does.
type Cache[V any] struct {
	maxBytes int64
	ttl      time.Duration
	size     func(V) int64
	now      func() time.Time

	mu    sync.Mutex
	order *list.List // front = most recently used; values are *Entry[V]
	byID  map[string]*list.Element
	byKey map[string]*list.Element
	bytes int64
}

// New returns an empty cache. It panics on a non-positive bound or a nil
// Size, which are programming errors rather than conditions a caller can
// recover from.
func New[V any](cfg Config[V]) *Cache[V] {
	if cfg.MaxBytes <= 0 || cfg.TTL <= 0 || cfg.Size == nil {
		panic("contentcache: New needs a positive MaxBytes and TTL and a Size function")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Cache[V]{
		maxBytes: cfg.MaxBytes,
		ttl:      cfg.TTL,
		size:     cfg.Size,
		now:      now,
		order:    list.New(),
		byID:     make(map[string]*list.Element),
		byKey:    make(map[string]*list.Element),
	}
}

// Put stores one value under key and returns the entry with ID and StoredAt
// filled in.
//
// A key that already holds a live entry keeps it, and Put returns that entry
// instead of storing value. A caller reaches Put only after Lookup missed, so
// a live entry here was stored by a concurrent caller resolving the same
// content; replacing it would retire the id that caller already handed to a
// client. An expired entry under the key is replaced.
func (c *Cache[V]) Put(key string, value V) (Entry[V], error) {
	size := c.size(value)
	if size <= 0 {
		return Entry[V]{}, errors.New("contentcache: value has no size")
	}
	if size > c.maxBytes {
		return Entry[V]{}, fmt.Errorf("%w: %d bytes exceeds the %d byte cache", ErrTooLarge, size, c.maxBytes)
	}
	id, err := newContentID()
	if err != nil {
		return Entry[V]{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.dropExpiredLocked(now)
	if key != "" {
		if held, ok := c.touchLocked(c.byKey[key]); ok {
			return held, nil
		}
	}
	for c.bytes+size > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.removeLocked(oldest)
	}
	stored := &Entry[V]{ID: id, Key: key, Value: value, StoredAt: now}
	element := c.order.PushFront(stored)
	c.byID[id] = element
	if key != "" {
		c.byKey[key] = element
	}
	c.bytes += size
	return *stored, nil
}

// Get resolves one entry by id, refreshing its position. An expired entry is
// dropped and reported missing.
func (c *Cache[V]) Get(id string) (Entry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return c.touchLocked(c.byID[id])
}

// Lookup resolves one entry by the caller's dedupe key, with the same expiry
// and LRU handling Get has.
func (c *Cache[V]) Lookup(key string) (Entry[V], bool) {
	if key == "" {
		return Entry[V]{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return c.touchLocked(c.byKey[key])
}

// Bytes reports the currently retained total, after reclaiming anything
// expired. Tests and diagnostics only.
func (c *Cache[V]) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropExpiredLocked(c.now())
	return c.bytes
}

func (c *Cache[V]) touchLocked(element *list.Element) (Entry[V], bool) {
	if element == nil {
		return Entry[V]{}, false
	}
	entry := element.Value.(*Entry[V])
	if c.now().Sub(entry.StoredAt) >= c.ttl {
		c.removeLocked(element)
		return Entry[V]{}, false
	}
	c.order.MoveToFront(element)
	return *entry, true
}

func (c *Cache[V]) dropExpiredLocked(now time.Time) {
	for element := c.order.Back(); element != nil; {
		previous := element.Prev()
		if now.Sub(element.Value.(*Entry[V]).StoredAt) >= c.ttl {
			c.removeLocked(element)
		}
		element = previous
	}
}

func (c *Cache[V]) removeLocked(element *list.Element) {
	entry := element.Value.(*Entry[V])
	c.order.Remove(element)
	delete(c.byID, entry.ID)
	if entry.Key != "" && c.byKey[entry.Key] == element {
		delete(c.byKey, entry.Key)
	}
	c.bytes -= c.size(entry.Value)
}

// newContentID mints the id a ticketed URL carries. Unpadded base64url keeps
// it inside [A-Za-z0-9_-], which is the character class the routes' path
// segment and the frontend's URL check both admit without escaping.
func newContentID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("contentcache: generate content id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
