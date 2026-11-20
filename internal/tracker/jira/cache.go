package jira

import (
	"container/list"
	"sync"

	"agent-overflow/internal/tracker/trackerapi"
)

// cache is every cache of the package: at most size values by key, the
// least recently used dropped first. It hands out and stores copies made
// by clone, so no caller shares memory it can change with the cache or
// with another caller; a value no caller can change (a *Catalog) needs
// no clone.
// A generation fences it: a reader takes the epoch before its request
// and stores under it, and a clear in between makes that store a no-op,
// so a read that started before a clear never undoes it.
//
// Every value is what Jira answered one credential, so each is kept with
// the credential read its answer was drawn under and handed out only
// while the transport holds that read (now, the transport's
// CredentialMark). A credential read again, forgotten or replaced after
// a refusal retires every value, whichever call drew the answer that
// caused it; no entry point has to report it.
type cache[K comparable, V any] struct {
	mu    sync.Mutex
	size  int
	clone func(V) V // nil when V shares no memory a caller can change
	now   func() trackerapi.CredentialMark
	order *list.List
	byKey map[K]*list.Element
	gen   uint64
}

type cacheEntry[K comparable, V any] struct {
	key  K
	val  V
	mark trackerapi.CredentialMark
}

func newCache[K comparable, V any](size int, clone func(V) V, now func() trackerapi.CredentialMark) *cache[K, V] {
	return &cache[K, V]{size: size, clone: clone, now: now, order: list.New(), byKey: map[K]*list.Element{}}
}

func (c *cache[K, V]) copy(v V) V {
	if c.clone == nil {
		return v
	}
	return c.clone(v)
}

// get returns a copy of the value under k, while the credential read it
// was drawn under is the one the transport holds; a value of another
// read is dropped.
func (c *cache[K, V]) get(k K) (V, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	e, ok := c.byKey[k]
	if !ok {
		return zero, false
	}
	entry := e.Value.(cacheEntry[K, V])
	if !sameRead(entry.mark, now) {
		c.order.Remove(e)
		delete(c.byKey, k)
		return zero, false
	}
	c.order.MoveToFront(e)
	return c.copy(entry.val), true
}

// epoch is the generation a read stores under; take it before the read's
// request.
func (c *cache[K, V]) epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// put stores a copy of v under k, drawn under the credential read mark,
// unless the cache was cleared since epoch was taken or the transport no
// longer holds that read (an answer of no credential, mark zero, is
// never kept).
func (c *cache[K, V]) put(epoch uint64, mark trackerapi.CredentialMark, k K, v V) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.gen || !sameRead(mark, now) {
		return
	}
	v = c.copy(v)
	if e, ok := c.byKey[k]; ok {
		e.Value = cacheEntry[K, V]{k, v, mark}
		c.order.MoveToFront(e)
		return
	}
	c.byKey[k] = c.order.PushFront(cacheEntry[K, V]{k, v, mark})
	if c.order.Len() > c.size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.byKey, last.Value.(cacheEntry[K, V]).key)
	}
}

// drop drops the value under k and fences off the reads in flight, of
// every key: a read that started before cannot tell which key it was
// for once it stores, so none stores, and each is read again on its
// next get.
func (c *cache[K, V]) drop(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byKey[k]; ok {
		c.order.Remove(e)
		delete(c.byKey, k)
	}
	c.gen++
}

// clear drops every value and fences off the reads in flight.
func (c *cache[K, V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.byKey)
	c.gen++
}
