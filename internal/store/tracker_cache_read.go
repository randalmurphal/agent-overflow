package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// MirrorRead is when a read of a tracker began, as a root's mirror orders
// what it stores: a card, a row, a listing, a document or its opening, a
// structure, a history, a worklog range and a scope's recorded outcome.
// Every write that orders by a read's begin time takes one, and only
// BeginMirrorRead makes one, so every such time a root stores came from
// its one clock. The zero value is no read, which a write takes only where
// it documents that none is allowed (a detail write with no opening).
type MirrorRead struct{ at int64 }

// At is the read's begin time in Unix milliseconds on the root's clock.
func (r MirrorRead) At() int64 { return r.at }

// mirrorReadLease is how far past the newest read it began a root's lease
// reaches (BeginMirrorRead).
const mirrorReadLease = 60_000

// mirrorReadClocks is each root's read clock in this process: the last
// read it began and the lease stored for the root. A root's ID is never
// reused, a restore's empty mirror included (its sqlite_sequence row
// stays), so a clock never outlives its root into another.
type mirrorReadClocks struct {
	mu    sync.Mutex
	roots map[int64]*mirrorReadClock
}

type mirrorReadClock struct{ last, lease int64 }

// BeginMirrorRead begins a read of rootID's tracker at now, the caller's
// wall clock in Unix milliseconds: it returns now, or a millisecond after
// the root's last read when that is later, so two reads are ordered as
// they began and no read begins before one the root stored, whatever the
// wall clock did. The root's lease (tracker_cache_read_leases) is stored
// above every read begun: a read that would pass it first stores a new
// lease mirrorReadLease past itself, one statement and one row write per
// root per minute of reading, and a process's first read of a root reads
// the lease and begins after it, so a read begun after a restart, a crash
// included, is later than every read the root stored. A read that stores
// a lease fails with ErrTrackerCacheGone when the root is detached or
// gone; one within the lease touches no row, and every write of what it
// read refuses such a root. The process keeps one clock per root it read.
func (s *Store) BeginMirrorRead(rootID, now int64) (MirrorRead, error) {
	c := &s.mirrorReads
	c.mu.Lock()
	defer c.mu.Unlock()
	clock := c.roots[rootID]
	if clock == nil {
		var lease int64
		err := s.db.QueryRow(`SELECT lease FROM tracker_cache_read_leases WHERE root_id = ?`, rootID).Scan(&lease)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return MirrorRead{}, fmt.Errorf("store: read the read lease of tracker mirror %d: %w", rootID, err)
		}
		clock = &mirrorReadClock{last: lease, lease: lease}
		if c.roots == nil {
			c.roots = map[int64]*mirrorReadClock{}
		}
		c.roots[rootID] = clock
	}
	next := max(now, clock.last+1)
	if next > clock.lease {
		lease := next + mirrorReadLease
		var stored int64
		err := s.db.QueryRow(`INSERT INTO tracker_cache_read_leases(root_id, lease)
			SELECT id, ?2 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NOT NULL
			ON CONFLICT(root_id) DO UPDATE SET lease = max(lease, excluded.lease) RETURNING lease`, rootID, lease).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return MirrorRead{}, fmt.Errorf("store: begin a read of tracker mirror %d: %w", rootID, ErrTrackerCacheGone)
		case err != nil:
			return MirrorRead{}, fmt.Errorf("store: store the read lease of tracker mirror %d: %w", rootID, err)
		}
		clock.lease = stored
	}
	clock.last = next
	return MirrorRead{at: next}, nil
}
