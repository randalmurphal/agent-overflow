package store

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// mirrorWriteRecorder counts, through SQLite's pre-update hook on the
// writer connection, every row the writer inserts, updates or deletes,
// triggers and foreign key actions included, and per committed
// transaction the deletes of mirror rows and the writes of history changes,
// of board marks and of board rows, and per statement the writes of sprint
// memberships. A statement is told apart by the count of cached statement
// runs, so a test that reads the per-statement counts is not parallel.
type mirrorWriteRecorder struct {
	s       *Store
	mu      sync.Mutex
	writes  map[string]int
	deletes map[uint64]int
	changes map[uint64]int
	marks   map[uint64]int
	rows    map[uint64]int
	sprints map[int64]int
}

func recordMirrorWrites(t *testing.T, s *Store) *mirrorWriteRecorder {
	t.Helper()
	r := &mirrorWriteRecorder{s: s}
	r.reset()
	hook := func(fn sqlite.PreUpdateHookFn) {
		conn, err := s.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := conn.Raw(func(dc any) error {
			hooks, ok := dc.(*stmtCacheConn).sqliteConn.(sqlite.HookRegisterer)
			if !ok {
				return fmt.Errorf("writer connection %T has no hooks", dc)
			}
			hooks.RegisterPreUpdateHook(fn)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	hook(func(d sqlite.SQLitePreUpdateData) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.writes[d.TableName]++
		if d.Op == sqlite3.SQLITE_DELETE && strings.HasPrefix(d.TableName, "tracker_cache_") {
			r.deletes[s.CommitCount()]++
		}
		switch d.TableName {
		case "tracker_cache_changes":
			r.changes[s.CommitCount()]++
		case "tracker_cache_board_unexplained":
			r.marks[s.CommitCount()]++
		case "tracker_cache_board_issues":
			r.rows[s.CommitCount()]++
		case "tracker_cache_issue_sprints":
			r.sprints[cachedStmtRuns.Load()]++
		}
	})
	t.Cleanup(func() { hook(nil) })
	return r
}

func (r *mirrorWriteRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes, r.deletes, r.changes, r.marks, r.rows = map[string]int{}, map[uint64]int{}, map[uint64]int{}, map[uint64]int{}, map[uint64]int{}
	r.sprints = map[int64]int{}
}

// mostSprintWritesInAStatement is the most sprint memberships one
// statement wrote (inserted or deleted) since the last reset, and the
// statements that wrote any.
func (r *mirrorWriteRecorder) mostSprintWritesInAStatement() (most, statements int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.sprints {
		most = max(most, n)
	}
	return most, len(r.sprints)
}

// rowWrites are the rows written since the last reset, by table.
func (r *mirrorWriteRecorder) rowWrites() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.writes)
}

// mostDeletedInATransaction is the most mirror rows one transaction
// deleted since the last reset, and the total deleted.
func (r *mirrorWriteRecorder) mostDeletedInATransaction() (most, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.deletes {
		most, total = max(most, n), total+n
	}
	return most, total
}

// mostChangesInATransaction is the most history changes one transaction
// wrote (inserted, updated or deleted) since the last reset, and the
// transactions that wrote any.
func (r *mirrorWriteRecorder) mostChangesInATransaction() (most, transactions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.changes {
		most = max(most, n)
	}
	return most, len(r.changes)
}

// mostMarksInATransaction is the most board marks one transaction wrote
// since the last reset, and the transactions that wrote any.
func (r *mirrorWriteRecorder) mostMarksInATransaction() (most, transactions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.marks {
		most = max(most, n)
	}
	return most, len(r.marks)
}

// mostRowsInATransaction is the most board rows one transaction wrote
// since the last reset, and the transactions that wrote any.
func (r *mirrorWriteRecorder) mostRowsInATransaction() (most, transactions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.rows {
		most = max(most, n)
	}
	return most, len(r.rows)
}
