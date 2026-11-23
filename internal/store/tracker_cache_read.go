package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// MirrorRead is a read of one root's tracker as its mirror orders what it
// stores: a card, a row, a listing, a document or its opening, a
// structure, a history, a worklog range and a scope's recorded outcome.
// Every write that orders by a read takes one, and only BeginMirrorRead
// makes one, so every order time a root stores came from its one clock.
// A read names its root, and every writer refuses a read of another root
// (MirrorRead.in). The zero value is no read, which a write takes only where it
// documents that none is allowed (a detail write with no opening).
type MirrorRead struct {
	root int64
	// rootOrder is the read's order time, read only by At and by in,
	// which checks the root (TestOnlyTheRootCheckReadsAReadsOrder).
	rootOrder int64
	wall      int64
}

// At is the read's order time in Unix milliseconds on its root's clock,
// for comparing with the order times the root stores (MirrorSync.ReadAt,
// MirrorBody.ReadAt). It is no wall time: an interval is measured with
// Wall.
func (r MirrorRead) At() int64 { return r.rootOrder }

// Wall is the wall clock the read began at, in Unix milliseconds: the time
// an interval since the read is measured from (MirrorSync.StructureWall,
// MirrorSync.ReconciledWall, MirrorBody.Wall).
func (r MirrorRead) Wall() int64 { return r.wall }

// mirrorReadTimes are a read's times once checked against the root a
// write names (MirrorRead.in): what the writers order and measure by.
type mirrorReadTimes struct{ at, wall int64 }

// in returns the read's times for a write of rootID: a read of another
// root is refused, and no read is zero.
func (r MirrorRead) in(rootID int64) (mirrorReadTimes, error) {
	switch {
	case r == MirrorRead{}:
		return mirrorReadTimes{}, nil
	case r.root != rootID:
		return mirrorReadTimes{}, fmt.Errorf("%w: a read of tracker mirror %d written to mirror %d", ErrInvalidTrackerCache, r.root, rootID)
	}
	return mirrorReadTimes{at: r.rootOrder, wall: r.wall}, nil
}

// mirrorReadTimeColumns are the columns of each mirror table that hold a
// read's order time (MirrorRead.At). A process's first read of a root
// begins after the highest of them (mirrorReadSeedSQL), so a read begun
// after a restart, a crash included, orders after every read the root
// stored. A read time a CHECK holds at or below a listed column of its
// row is left out (mirrorBoundedReadTimes).
// TestEveryReadTimeColumnSeedsTheClock holds the list to the schema: a
// column added to a mirror table fails it until it is listed here, bounded
// there or named as no read time.
var mirrorReadTimeColumns = []struct {
	table   string
	columns []string
}{
	{"tracker_cache_users", []string{"read_at"}},
	{"tracker_cache_statuses", []string{"read_at"}},
	{"tracker_cache_boards", []string{"read_at", "rows_removed_at", "listing_read_at", "estimate_reset_at"}},
	{"tracker_cache_board_unexplained", []string{"named_at"}},
	{"tracker_cache_issues", []string{"read_at", "body_read_at", "body_opened_at", "status_read_at"}},
	{"tracker_cache_board_issues", []string{"read_at"}},
	{"tracker_cache_open_tickets", []string{"opened_at"}},
	{"tracker_cache_worklog_days", []string{"read_at"}},
	{"tracker_cache_sync", []string{"cursor_read_at", "read_at", "reconciled_at", "outcome_at"}},
}

// mirrorBoundedReadTimes are the read times the seed leaves out, each with
// the CHECK of its table that holds it at or below another read time of
// its row, which the seed reads or which is itself bounded.
var mirrorBoundedReadTimes = map[string]string{
	"tracker_cache_users.changed_at":         "changed_at BETWEEN 1 AND read_at",
	"tracker_cache_statuses.changed_at":      "changed_at BETWEEN 1 AND read_at",
	"tracker_cache_statuses.done_changed_at": "done_changed_at BETWEEN 0 AND changed_at",
	"tracker_cache_issues.status_floor_at":   "status_floor_at BETWEEN 0 AND status_read_at",
	"tracker_cache_sync.failed_at":           "outcome_at >= failed_at",
}

// mirrorReadSeedSQL reads, in one statement and so one snapshot, whether
// root ?1 is live and the highest order time it stores: one walk of the
// root's rows of each table in mirrorReadTimeColumns. No row is no live
// root.
var mirrorReadSeedSQL = func() string {
	var tables []string
	for _, t := range mirrorReadTimeColumns {
		maxima := make([]string, len(t.columns))
		for i, c := range t.columns {
			maxima[i] = `coalesce(max(` + c + `), 0)`
		}
		highest := maxima[0]
		if len(maxima) > 1 {
			highest = `max(` + strings.Join(maxima, `, `) + `)`
		}
		tables = append(tables, `(SELECT `+highest+` FROM `+t.table+` WHERE root_id = ?1)`)
	}
	return `SELECT max(0, ` + strings.Join(tables, `,
	 `) + `) FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NOT NULL`
}()

// mirrorReadClocks is each live root's read clock in this process: the
// last order time it gave. A clock is made only for a live root, by the
// statement that seeds it, and removed when its root is detached, drained
// or replaced by a restore (forgetMirrorRead, forgetMirrorReads), each under mu, so no clock
// outlives the root it was seeded from.
type mirrorReadClocks struct {
	mu    sync.Mutex
	roots map[int64]int64
}

// BeginMirrorRead begins a read of rootID's tracker at now, the caller's
// wall clock in Unix milliseconds. Its order time is now, or a millisecond
// after the root's last read when that is later, so two reads are ordered
// as they began and no read is ordered before one the root stored,
// whatever the wall clock did; its wall time is now. A process's first
// read of a root seeds the root's clock from the order times the root
// stores (mirrorReadSeedSQL), so a read begun after a restart orders after
// every read stored before it; later reads touch no row. The seed fails
// with ErrTrackerCacheGone for a detached or missing root and makes no
// clock; every write of a read refuses such a root.
func (s *Store) BeginMirrorRead(rootID, now int64) (MirrorRead, error) {
	c := &s.mirrorReads
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.roots[rootID]
	if !ok {
		err := s.db.QueryRow(mirrorReadSeedSQL, rootID).Scan(&last)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return MirrorRead{}, fmt.Errorf("store: begin a read of tracker mirror %d: %w", rootID, ErrTrackerCacheGone)
		case err != nil:
			return MirrorRead{}, fmt.Errorf("store: seed the read clock of tracker mirror %d: %w", rootID, err)
		}
		if c.roots == nil {
			c.roots = map[int64]int64{}
		}
	}
	order := max(now, last+1)
	c.roots[rootID] = order
	return MirrorRead{root: rootID, rootOrder: order, wall: now}, nil
}

// forgetMirrorRead removes rootID's read clock, after its detach or drain.
func (s *Store) forgetMirrorRead(rootID int64) {
	c := &s.mirrorReads
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.roots, rootID)
}

// forgetMirrorReads removes every root's read clock, after a restore
// replaced the mirror.
func (s *Store) forgetMirrorReads() {
	c := &s.mirrorReads
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.roots)
}
