package jira

import (
	"container/list"
	"sync"
)

// cache is every cache of the package: at most size values by key, the
// least recently used dropped first. It hands out and stores copies made
// by clone, so no caller shares memory with it or with another caller.
// A generation fences it: a reader takes the epoch before its request
// and stores under it, and a clear in between makes that store a no-op,
// so a read that started before a clear never undoes it.
type cache[K comparable, V any] struct {
	mu    sync.Mutex
	size  int
	clone func(V) V // nil when V shares no memory
	order *list.List
	byKey map[K]*list.Element
	gen   uint64
}

type cacheEntry[K comparable, V any] struct {
	key K
	val V
}

func newCache[K comparable, V any](size int, clone func(V) V) *cache[K, V] {
	return &cache[K, V]{size: size, clone: clone, order: list.New(), byKey: map[K]*list.Element{}}
}

func (c *cache[K, V]) copy(v V) V {
	if c.clone == nil {
		return v
	}
	return c.clone(v)
}

// get returns a copy of the value under k.
func (c *cache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[k]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(e)
	return c.copy(e.Value.(cacheEntry[K, V]).val), true
}

// epoch is the generation a read stores under; take it before the read's
// request.
func (c *cache[K, V]) epoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// put stores a copy of v under k, unless the cache was cleared since
// epoch was taken.
func (c *cache[K, V]) put(epoch uint64, k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.gen {
		return
	}
	v = c.copy(v)
	if e, ok := c.byKey[k]; ok {
		e.Value = cacheEntry[K, V]{k, v}
		c.order.MoveToFront(e)
		return
	}
	c.byKey[k] = c.order.PushFront(cacheEntry[K, V]{k, v})
	if c.order.Len() > c.size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.byKey, last.Value.(cacheEntry[K, V]).key)
	}
}

// clear drops every value and fences off the reads in flight.
func (c *cache[K, V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.byKey)
	c.gen++
}
