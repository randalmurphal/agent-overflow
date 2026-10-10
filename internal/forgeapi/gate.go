package forgeapi

import (
	"sync"
	"time"
)

// Rate gate constants from docs/architecture/forge-transport.md#rate-limits.
const (
	gateFallback    = 60 * time.Second
	gateFallbackMax = 15 * time.Minute
	// reservePercent of a pool's limit is left to interactive requests.
	reservePercent = 10
)

// gateKey names one quota pool of one token on one host.
type gateKey struct {
	host        string
	pool        string
	fingerprint string
}

// lease is what check hands a caller: the closure generation the request
// started under. A rate-limited answer to a request leased before the
// current closure extends that closure instead of opening another.
type lease struct {
	key gateKey
	gen uint64
}

// gate is one pool's state: the closure (until, its generation and how
// many consecutive closures there have been) and the latest quota
// snapshot the forge reported.
type gate struct {
	gen      uint64
	until    time.Time
	closures int

	quota *RateLimit
}

// gates holds every pool's gate, shared by every caller of a Service.
// Entries exist only for hosts with a live credential: a token change
// drops the host's gates for other tokens, and Close drops them all.
type gates struct {
	mu    sync.Mutex
	byKey map[gateKey]*gate
}

func newGates() *gates { return &gates{byKey: make(map[gateKey]*gate)} }

func (g *gates) get(key gateKey) *gate {
	entry := g.byKey[key]
	if entry == nil {
		entry = &gate{}
		g.byKey[key] = entry
	}
	return entry
}

// check admits a request to the pool or refuses it with the
// *RateLimitedError the caller returns. A closed gate refuses every
// request. An open one refuses a background request while the pool's
// remaining quota is under the reserve, and an interactive request only
// once the forge reports none left.
func (g *gates) check(key gateKey, interactive bool, now time.Time) (lease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.get(key)
	if now.Before(entry.until) {
		return lease{}, &RateLimitedError{Host: key.host, Pool: key.pool, Until: entry.until}
	}
	if q := entry.quota; q != nil && q.Reset.After(now) {
		switch {
		case q.Remaining < 1:
			return lease{}, &RateLimitedError{Host: key.host, Pool: key.pool, Until: q.Reset}
		case !interactive && q.Limit > 0 && q.Remaining*100 < q.Limit*reservePercent:
			return lease{}, &RateLimitedError{Host: key.host, Pool: key.pool, Until: q.Reset, Reserve: true}
		}
	}
	return lease{key: key, gen: entry.gen}, nil
}

// observe records a response's quota headers. Among responses the latest
// reset wins, then the latest response: an answer from an older window
// does not overwrite a newer one.
func (g *gates) observe(key gateKey, rate *RateLimit) {
	if rate == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.get(key)
	if entry.quota != nil && rate.Reset.Before(entry.quota.Reset) {
		return
	}
	snapshot := *rate
	entry.quota = &snapshot
}

// limited closes the gate after a rate-limited answer and returns when it
// reopens. until is the time the forge gave, zero for none; the fallback
// is 60s doubling per consecutive closure up to 15 minutes. A request
// leased before the current closure extends it without counting as a
// closure, so a burst of in-flight refusals doubles once. No closure ever
// shortens an open one.
func (g *gates) limited(l lease, until time.Time, now time.Time) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.get(l.key)
	current := l.gen == entry.gen
	if current {
		entry.gen++
		entry.closures++
	}
	if until.IsZero() {
		wait := gateFallback
		for i := 1; i < max(entry.closures, 1) && wait < gateFallbackMax; i++ {
			wait *= 2
		}
		until = now.Add(min(wait, gateFallbackMax))
	}
	if until.After(entry.until) {
		entry.until = until
	}
	return entry.until
}

// succeeded resets the doubling after a success on a current lease once
// the closure has passed.
func (g *gates) succeeded(l lease, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.byKey[l.key]
	if entry != nil && l.gen == entry.gen && !now.Before(entry.until) {
		entry.closures = 0
	}
}

// dropHost forgets a host's gates for every token but keep ("" drops all).
func (g *gates) dropHost(host, keep string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key := range g.byKey {
		if key.host == host && key.fingerprint != keep {
			delete(g.byKey, key)
		}
	}
}

func (g *gates) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.byKey)
}
