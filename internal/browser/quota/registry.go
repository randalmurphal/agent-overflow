package quota

import (
	"errors"
	"sync"
	"sync/atomic"
)

// Registry hands out one Pool per key, the same one for as long as anything
// holds it: a Hold not yet released, or a reservation against the pool not
// yet ended. Once nothing holds a key's pool it is forgotten, and the key's
// next Hold starts an empty one. A key never has two pools at once, so a
// reservation taken under a key always counts against the pool every later
// reservation under that key meets.
type Registry struct {
	name  string
	limit int64
	mu    sync.Mutex
	pools map[string]*Pool
}

// NewRegistry answers a registry whose pools have limit bytes each, named
// in their refusals.
func NewRegistry(name string, limit int64) *Registry {
	return &Registry{name: name, limit: limit, pools: make(map[string]*Pool)}
}

// registration is a pool's place in its registry. Guarded by Registry.mu.
type registration struct {
	registry *Registry
	key      string
	// holders counts the Holds and live reservations holding the pool.
	holders int
}

// Hold answers key's pool, registered until the hold's Release.
func (r *Registry) Hold(key string) *PoolHold {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool := r.pools[key]
	if pool == nil {
		pool = NewPool(r.name, r.limit)
		pool.registration = &registration{registry: r, key: key}
		r.pools[key] = pool
	}
	pool.registration.holders++
	return &PoolHold{pool: pool}
}

// Len is how many keys have a pool.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pools)
}

// PoolHold keeps a key's pool registered until its one Release.
type PoolHold struct {
	pool     *Pool
	released atomic.Bool
}

// Pool is the held pool.
func (h *PoolHold) Pool() *Pool { return h.pool }

// Release lets go of the pool, and answers whether this call did. A nil
// hold holds nothing.
func (h *PoolHold) Release() bool {
	if h == nil || !h.released.CompareAndSwap(false, true) {
		return false
	}
	h.pool.unhold()
	return true
}

// ErrReplaced refuses a reservation against a pool its registry forgot and
// replaced with a new one for its key: the reservation would make the key's
// second pool.
var ErrReplaced = errors.New("quota: the pool was replaced")

// hold adds a holder to a registered pool, registering it again if its
// registry forgot it and its key has no pool since. A pool of no registry
// needs none.
func (p *Pool) hold() error {
	reg := p.registration
	if reg == nil {
		return nil
	}
	reg.registry.mu.Lock()
	defer reg.registry.mu.Unlock()
	switch current := reg.registry.pools[reg.key]; current {
	case p:
	case nil:
		reg.registry.pools[reg.key] = p
	default:
		return ErrReplaced
	}
	reg.holders++
	return nil
}

// unhold removes a holder, and the pool from its registry with the last.
func (p *Pool) unhold() {
	reg := p.registration
	if reg == nil {
		return
	}
	reg.registry.mu.Lock()
	defer reg.registry.mu.Unlock()
	reg.holders--
	if reg.holders == 0 && reg.registry.pools[reg.key] == p {
		delete(reg.registry.pools, reg.key)
	}
}
