package git

import (
	"errors"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/forgeapi"
)

// ciCapabilityCache remembers the reads behind the CI action flags that
// the CI read itself does not carry (GitHub's repository permission for
// ReadPipeline, and which completed runs keep artifacts): an answer until
// its expiry, and a failure until its retry is due, so a capability read
// that keeps failing is asked again with the open-PR lookup's backoff
// (Core.prLookupErrorDelay), or at a rate limit's release, not every
// tick. A success resets the failure count. Bounded by gitlabCacheMax,
// evicting the entry that expires first.
type ciCapabilityCache[V any] struct {
	mu      sync.Mutex
	entries map[string]ciCapabilityEntry[V]
}

type ciCapabilityEntry[V any] struct {
	value    V
	err      error
	until    time.Time
	failures int
}

// get answers key's remembered value or failure while it is current.
func (c *ciCapabilityCache[V]) get(key string, now time.Time) (V, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !now.Before(entry.until) {
		var zero V
		return zero, nil, false
	}
	return entry.value, entry.err, true
}

// put remembers a successful answer until until.
func (c *ciCapabilityCache[V]) put(key string, value V, until time.Time) {
	c.store(key, ciCapabilityEntry[V]{value: value, until: until})
}

// fail remembers a failed read until its retry is due: delay(failures)
// after now, or a rate limit's release when that is later.
func (c *ciCapabilityCache[V]) fail(key string, err error, now time.Time, delay func(failures int) time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	failures := c.entries[key].failures + 1
	until := now.Add(delay(failures))
	if limited, ok := errors.AsType[*forgeapi.RateLimitedError](err); ok && limited.Until.After(until) {
		until = limited.Until
	}
	c.storeLocked(key, ciCapabilityEntry[V]{err: err, until: until, failures: failures})
}

// drop forgets every entry whose key starts with prefix.
func (c *ciCapabilityCache[V]) drop(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if strings.HasPrefix(key, prefix) {
			delete(c.entries, key)
		}
	}
}

func (c *ciCapabilityCache[V]) store(key string, entry ciCapabilityEntry[V]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, entry)
}

func (c *ciCapabilityCache[V]) storeLocked(key string, entry ciCapabilityEntry[V]) {
	if c.entries == nil {
		c.entries = make(map[string]ciCapabilityEntry[V])
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= gitlabCacheMax {
		first, firstUntil := "", time.Time{}
		for k, e := range c.entries {
			if first == "" || e.until.Before(firstUntil) {
				first, firstUntil = k, e.until
			}
		}
		delete(c.entries, first)
	}
	c.entries[key] = entry
}
