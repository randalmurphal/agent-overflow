// Package quota keeps byte budgets and the reservations held against them.
// A Reservation holds the same amount of every pool it was taken from, all
// or none, from Reserve to its one Settle or Release: a settle keeps the
// actual bytes charged, a release returns them all, and whichever comes
// first is the only one that acts. There is no way to give a reservation
// back and take it again, so whoever holds one keeps its bytes for as long
// as it holds it, and a holder that hands it on (Take) leaves none behind.
package quota

import (
	"fmt"
	"sync/atomic"
)

// Pool is one budget: the bytes charged against it, at most its limit for
// every reservation it admits.
type Pool struct {
	name  string
	limit int64
	used  atomic.Int64
	// registration is the pool's place in the Registry that made it, nil
	// for a pool of none.
	registration *registration
}

// NewPool answers an empty pool of limit bytes, named in its refusals.
func NewPool(name string, limit int64) *Pool {
	return &Pool{name: name, limit: limit}
}

// Used is the bytes charged against the pool.
func (p *Pool) Used() int64 { return p.used.Load() }

// SetUsed charges the pool with what is already there, measured apart from
// any reservation (a scan of the files it bounds).
func (p *Pool) SetUsed(bytes int64) { p.used.Store(bytes) }

// fullError is a reservation refused by the pool that had no room for it.
type fullError struct {
	pool  string
	limit int64
}

func (e *fullError) Error() string { return fmt.Sprintf("%s exceed %d bytes", e.pool, e.limit) }

// take charges bytes when that stays within the limit. A refusal never
// touches the counter, which other holders change concurrently.
func (p *Pool) take(bytes int64) bool {
	for {
		current := p.used.Load()
		if bytes < 0 || current > p.limit-bytes {
			return false
		}
		if p.used.CompareAndSwap(current, current+bytes) {
			return true
		}
	}
}

// Reservation is bytes held of one or more pools.
type Reservation struct {
	pools  []*Pool
	amount int64
	done   atomic.Bool
}

// Reserve holds amount of every pool, all or none, or answers the
// *fullError of the first that has no room, or errReplaced for a registry's
// pool it forgot and replaced. A reservation keeps each registry's pool
// registered until it ends.
func Reserve(amount int64, pools ...*Pool) (*Reservation, error) {
	for i, pool := range pools {
		if err := pool.hold(); err != nil {
			for _, held := range pools[:i] {
				held.unhold()
			}
			return nil, err
		}
	}
	for i, pool := range pools {
		if !pool.take(amount) {
			for _, taken := range pools[:i] {
				taken.used.Add(-amount)
			}
			for _, held := range pools {
				held.unhold()
			}
			return nil, &fullError{pool: pool.name, limit: pool.limit}
		}
	}
	return &Reservation{pools: pools, amount: amount}, nil
}

// Settle ends the reservation keeping actual bytes charged against each of
// its pools, and answers whether this call ended it. A nil reservation
// holds nothing.
func (r *Reservation) Settle(actual int64) bool {
	if r == nil || !r.done.CompareAndSwap(false, true) {
		return false
	}
	for _, pool := range r.pools {
		pool.used.Add(actual - r.amount)
		pool.unhold()
	}
	return true
}

// Release ends the reservation returning all its bytes, and answers whether
// this call ended it.
func (r *Reservation) Release() bool { return r.Settle(0) }

// Take moves the reservation out of *slot, leaving it empty, for its new
// holder.
func Take(slot **Reservation) *Reservation {
	r := *slot
	*slot = nil
	return r
}
