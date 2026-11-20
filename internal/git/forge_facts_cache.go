package git

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// factCache holds answers by key and the reads in flight that will answer
// them, so concurrent callers asking for one key, in one batch or in
// overlapping ones, share one read. It holds at most maxEntries entries
// whose estimated sizes (size) total at most maxBytes, evicting the least
// recently used first; an entry past its expiry is dropped when it is
// next asked for or evicted. An entry larger than maxEntryBytes
// (maxBytes/64 unless the cache's maker sets it) is never held. A flight
// is held only while its owner reads it, so flights are bounded by the
// keys of the calls in flight.
type factCache[V any] struct {
	maxEntries    int
	maxBytes      int64
	maxEntryBytes int64
	size          func(key string, value V) int64

	mu      sync.Mutex
	lru     list.List // front is most recently used; values are *factEntry[V]
	entries map[string]*list.Element
	bytes   int64
	flights map[string]*factFlight[V]
	// gen counts clears: a flight settles into the cache only in the
	// generation it was claimed in.
	gen uint64
	// onJoin, for tests, is told (under mu) of each claim that joins
	// another caller's flight.
	onJoin func(key string)
}

type factEntry[V any] struct {
	key     string
	value   V
	expires time.Time // zero never expires
	bytes   int64
}

// factFlight is one key's read in flight. done is closed once value is
// the answer, and ok says whether there is one (false: the read failed,
// and a waiter answers as its owner did not).
type factFlight[V any] struct {
	done  chan struct{}
	value V
	ok    bool
	gen   uint64
}

func newFactCache[V any](maxEntries int, maxBytes int64, size func(string, V) int64) *factCache[V] {
	return &factCache[V]{maxEntries: maxEntries, maxBytes: maxBytes, maxEntryBytes: maxBytes / 64, size: size}
}

// claim answers key's held value (hit), the flight another caller reads
// it in (wait), or a new flight this caller now owns and must settle
// (own). Exactly one of the three is set. With fresh, a held value that
// can change (one with an expiry) is not answered: the key is read again,
// or the read in flight joined, and the held value stays until that read
// settles a value that replaces it. A value held without expiry answers
// either way.
func (c *factCache[V]) claim(key string, now time.Time, fresh bool) (value V, hit bool, wait, own *factFlight[V]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*factEntry[V])
		switch {
		case entry.expires.IsZero():
			c.lru.MoveToFront(element)
			return entry.value, true, nil, nil
		case !now.Before(entry.expires):
			c.removeLocked(element)
		case !fresh:
			c.lru.MoveToFront(element)
			return entry.value, true, nil, nil
		}
	}
	if flight, ok := c.flights[key]; ok {
		if c.onJoin != nil {
			c.onJoin(key)
		}
		return value, false, flight, nil
	}
	if c.flights == nil {
		c.flights = map[string]*factFlight[V]{}
	}
	flight := &factFlight[V]{done: make(chan struct{}), gen: c.gen}
	c.flights[key] = flight
	return value, false, nil, flight
}

// get answers key's held value without claiming a read.
func (c *factCache[V]) get(key string, now time.Time) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*factEntry[V])
		if entry.expires.IsZero() || now.Before(entry.expires) {
			c.lru.MoveToFront(element)
			return entry.value, true
		}
		c.removeLocked(element)
	}
	var zero V
	return zero, false
}

// put holds value under key until expires (zero: no expiry), evicting as
// the bounds require.
func (c *factCache[V]) put(key string, value V, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(key, value, expires)
}

func (c *factCache[V]) putLocked(key string, value V, expires time.Time) {
	bytes := c.size(key, value)
	if bytes > c.maxEntryBytes {
		return
	}
	if element, ok := c.entries[key]; ok {
		c.removeLocked(element)
	}
	if c.entries == nil {
		c.entries = map[string]*list.Element{}
	}
	for c.lru.Len() > 0 && (c.lru.Len() >= c.maxEntries || c.bytes+bytes > c.maxBytes) {
		c.removeLocked(c.lru.Back())
	}
	c.entries[key] = c.lru.PushFront(&factEntry[V]{key: key, value: value, expires: expires, bytes: bytes})
	c.bytes += bytes
}

// settle ends flight, which the caller owns: waiters are answered value
// (ok false: no answer), and with keep the value is held until expires.
func (c *factCache[V]) settle(key string, flight *factFlight[V], value V, ok, keep bool, expires time.Time) {
	c.mu.Lock()
	if c.flights[key] == flight {
		delete(c.flights, key)
	}
	if ok && keep && flight.gen == c.gen {
		c.putLocked(key, value, expires)
	}
	c.mu.Unlock()
	flight.value, flight.ok = value, ok
	close(flight.done)
}

// await answers flight's value once it settles, or false when ctx ends
// first or the flight had no answer.
func (f *factFlight[V]) await(ctx context.Context) (V, bool) {
	select {
	case <-f.done:
		return f.value, f.ok
	case <-ctx.Done():
		var zero V
		return zero, false
	}
}

// clear forgets every entry; flights in progress answer their waiters and
// hold nothing.
func (c *factCache[V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.lru.Init()
	c.entries = nil
	c.bytes = 0
	c.flights = nil
}

// held reports the entries and estimated bytes the cache holds.
func (c *factCache[V]) held() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}

func (c *factCache[V]) removeLocked(element *list.Element) {
	entry := element.Value.(*factEntry[V])
	c.lru.Remove(element)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
}

// factBytes estimates what a held fact costs: its strings, a label list's
// labels and the entry's fixed overhead (the list element, the map slot
// and the struct).
func factBytes(key string, fact ForgeFact) int64 {
	const overhead = 256
	bytes := int64(overhead + 2*len(key) + len(fact.URL) + len(fact.Title) + len(fact.Kind) + len(fact.State) +
		len(fact.Color) + len(fact.TextColor) + len(fact.OID) + len(fact.MIME) + len(fact.Text) + len(fact.Path))
	for _, label := range fact.Labels {
		bytes += labelBytes(label)
	}
	return bytes
}

// labelBytes estimates one label of a list: its slot in the list (three
// string headers, 48 bytes, with allocation rounding) and its strings.
func labelBytes(label ForgeLabel) int64 {
	const slot = 64
	return int64(slot + len(label.Title) + len(label.Color) + len(label.TextColor))
}
