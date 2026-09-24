package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// Fork stamp reports (docs/architecture/thread-replica-sync.md#pointer-fork-stamps).
//
// A write to one thread moves the stamps of the pointer forks that show a
// row it changes: a hand-off's copy (handOffCopyTx), spans on a payload a
// fork shows (bumpPayloadReadersTx), a revision touch of a row a fork
// shows (bumpHistoryRevForItemTx) and the detach of a deleted source
// (detachForkDescendantsTx). Each of those paths already holds the forks
// from the reads it makes, or reads them in the statement that moves them,
// and records them against the write transaction. The transaction's owner
// reports them once it commits (commitReportingForks, which the item
// writes' cardTxLocked calls) to the function set with OnForkStampsMoved,
// so a client showing one of those forks re-syncs its window. A write
// that moves no fork records nothing, and a thread no fork reads runs no
// statement for it.

// forkMoveLog holds the forks each open write transaction moved. The paths
// that record hold only the transaction, so the log is keyed by it; pending
// counts its transactions, so a write that recorded nothing reads no map.
type forkMoveLog struct {
	pending atomic.Int64
	mu      sync.Mutex
	byTx    map[*sql.Tx]map[string]struct{}
}

var forkMoves = forkMoveLog{byTx: make(map[*sql.Tx]map[string]struct{})}

// recordForkMovesTx notes that tx moved the stamps of forkIDs.
func recordForkMovesTx(tx *sql.Tx, forkIDs ...string) {
	if len(forkIDs) == 0 {
		return
	}
	forkMoves.mu.Lock()
	defer forkMoves.mu.Unlock()
	moved := forkMoves.byTx[tx]
	if moved == nil {
		moved = make(map[string]struct{}, len(forkIDs))
		forkMoves.byTx[tx] = moved
		forkMoves.pending.Add(1)
	}
	for _, id := range forkIDs {
		moved[id] = struct{}{}
	}
}

// takeForkMovesTx removes what tx recorded and returns it sorted.
func takeForkMovesTx(tx *sql.Tx) []string {
	if forkMoves.pending.Load() == 0 {
		return nil
	}
	forkMoves.mu.Lock()
	defer forkMoves.mu.Unlock()
	moved, ok := forkMoves.byTx[tx]
	if !ok {
		return nil
	}
	delete(forkMoves.byTx, tx)
	forkMoves.pending.Add(-1)
	return slices.Sorted(maps.Keys(moved))
}

// dropForkMovesTx discards what a transaction that did not commit
// recorded. The owner defers it beside its Rollback.
func dropForkMovesTx(tx *sql.Tx) {
	takeForkMovesTx(tx)
}

// OnForkStampsMoved sets the function a committed write calls with the
// pointer forks whose stamps it moved: once per transaction, each fork
// once, on the writer's goroutine after the commit. Nil clears it.
func (s *Store) OnForkStampsMoved(fn func(forkIDs []string)) {
	if fn == nil {
		s.forkStampsMoved.Store(nil)
		return
	}
	s.forkStampsMoved.Store(&fn)
}

func (s *Store) reportForkMoves(forkIDs []string) {
	if len(forkIDs) == 0 {
		return
	}
	if fn := s.forkStampsMoved.Load(); fn != nil {
		(*fn)(forkIDs)
	}
}

// commitReportingForks commits tx and reports the forks it moved. Its
// owner defers dropForkMovesTx, which discards them when it does not get
// here.
func (s *Store) commitReportingForks(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	s.reportForkMoves(takeForkMovesTx(tx))
	return nil
}

// forkReadersOfRowSQL is a JSON array of the forks
// trg_items_fork_reader_stamp advances for an in-place update of the
// `items` row it is evaluated against (forkReaderLineageSQL). A touch
// returns it, so the forks come from the statement that moves them.
var forkReadersOfRowSQL = `(SELECT json_group_array(l.thread_id) ` + fmt.Sprintf(forkReaderLineageSQL, "items") + `)`

// recordForkReadersTx records the forks a forkReadersOfRowSQL array names.
func recordForkReadersTx(tx *sql.Tx, readers string) error {
	if readers == "" || readers == "[]" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(readers), &ids); err != nil {
		return fmt.Errorf("store: parse fork readers %q: %w", readers, err)
	}
	recordForkMovesTx(tx, ids...)
	return nil
}
