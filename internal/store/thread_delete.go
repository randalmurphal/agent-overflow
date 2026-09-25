package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"agent-overflow/internal/threadmode"
)

// deleteThreadItemChunk bounds how many items a single DELETE statement
// (and therefore a single write transaction) removes while a thread is
// being deleted. Each item delete fires the two payload-GC triggers, so
// a 38k-item thread deleted through the FK cascade alone is one ~6s
// write transaction, longer than the 5s busy_timeout, meaning any
// concurrent writer errors SQLITE_BUSY instead of briefly waiting.
// 500-item chunks keep every write transaction in the tens of
// milliseconds.
const deleteThreadItemChunk = 500

// deleteThreadItemChunkSQL deletes one chunk of a thread's rows at or
// after a position, returning their ids.
var deleteThreadItemChunkSQL = `DELETE FROM items
		  WHERE rowid IN (SELECT rowid FROM items
		                   WHERE thread_id = ? AND (turn_index, item_index) >= (?, ?) LIMIT ` +
	strconv.Itoa(deleteThreadItemChunk) + `) RETURNING id`

// ChunkPause runs between the bounded write chunks of a long delete so a
// background caller can hand the write lock back to user writes between
// transactions. It is called only between chunks, never while one is
// open, and never after the last one.
type ChunkPause func()

// DeleteThread removes a thread row and everything that cascades from
// it, as fast as the database allows. Interactive callers use this.
func (s *Store) DeleteThread(id string) error {
	return s.DeleteThreadPaced(id, nil)
}

// DeleteThreadPaced is DeleteThread with a caller-supplied pause between
// item chunks.
//
// The first write marks the row deleting (BeginThreadDelete). From its
// commit the thread is gone to every read through owned_threads and a
// fork of it is refused (ErrForkSourceDeleted). The thread's items are
// drained next in bounded chunks, each its own transaction, so no single
// write transaction ever spans a large thread's whole item set; pause is
// where a background sweep yields so user writes interleave. A nil pause
// is DeleteThread.
//
// A thread its pointer forks read keeps what they read: the drain stops at
// the last reader's cut, and the last transaction makes the thread a
// holder (retireToHolderTx) instead of deleting its row, which the forks
// read from as before. Otherwise the row goes in the last transaction. A
// crash or an error in between leaves a marked row, which
// ListPendingThreadDeletes returns and a repeated DeleteThreadPaced
// completes.
func (s *Store) DeleteThreadPaced(id string, pause ChunkPause) error {
	if err := s.BeginThreadDelete(id); err != nil {
		return err
	}
	for {
		for {
			n, err := s.deleteThreadItemsChunk(id)
			if err != nil {
				return err
			}
			if n < deleteThreadItemChunk {
				break
			}
			if pause != nil {
				pause()
			}
		}
		done, err := s.finishThreadDelete(id)
		if err != nil || done {
			return err
		}
		// The last reader left during the drain: what it read goes too.
		if pause != nil {
			pause()
		}
	}
}

// BeginThreadDelete is a delete's first write: it marks id deleting. A
// fork of id admitted before it has committed by the time this write gets
// the writer connection, and one after it reads the mark and is refused,
// so what the forks read is settled before the delete releases anything
// (ReleasableAttachments). Marking a marked row again is how a repeated
// delete resumes.
func (s *Store) BeginThreadDelete(id string) error {
	result, err := s.db.Exec(`UPDATE threads SET deleting = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: mark thread %s deleting: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete thread %s", id))
}

// ListPendingThreadDeletes returns the threads whose delete began and did
// not finish (DeleteThreadPaced), and the holders no fork reads any more
// (ListReleasedHolders), for the app to complete at boot.
func (s *Store) ListPendingThreadDeletes() ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM threads WHERE deleting = 1`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending thread deletes: %w", err)
	}
	return ids, nil
}

// ListReleasedHolders returns the holders no fork reads any more
// (trg_thread_fork_lineage_release), for the app to delete.
func (s *Store) ListReleasedHolders() ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM threads WHERE deleting = 1 AND mode = ?`, threadmode.ModeHolder)
	if err != nil {
		return nil, fmt.Errorf("store: list released holders: %w", err)
	}
	return ids, nil
}

// OnHoldersReleased sets the function the store calls after it commits a
// write that may have released a holder: one that removed lineage rows.
// The app deletes the released holders (ListReleasedHolders); a crash
// before it does leaves them to ListPendingThreadDeletes. fn must not
// block. nil clears it.
func (s *Store) OnHoldersReleased(fn func()) {
	if fn == nil {
		s.holdersReleased.Store(nil)
		return
	}
	s.holdersReleased.Store(&fn)
}

// holdersMayBeReleased reports a committed write that removed lineage rows.
func (s *Store) holdersMayBeReleased() {
	if fn := s.holdersReleased.Load(); fn != nil {
		(*fn)()
	}
}

// readsThroughLineageTx reports whether id reads other threads' rows: its
// delete removes lineage rows (holdersMayBeReleased).
func readsThroughLineageTx(tx *sql.Tx, id string) (bool, error) {
	var reads bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_fork_lineage WHERE thread_id = ?)`, id).Scan(&reads); err != nil {
		return false, fmt.Errorf("store: read the lineage of %s: %w", id, err)
	}
	return reads, nil
}

// finishThreadDelete is DeleteThreadPaced's last transaction. It reports
// false when rows remain that no fork reads any more, for the drain to
// take first.
func (s *Store) finishThreadDelete(id string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin delete thread %s: %w", id, err)
	}
	defer tx.Rollback()
	reads, err := readsThroughLineageTx(tx, id)
	if err != nil {
		return false, err
	}
	retired, err := retireToHolderTx(tx, id)
	if err != nil {
		return false, err
	}
	if !retired {
		var remaining bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM items WHERE thread_id = ?)`, id).Scan(&remaining); err != nil {
			return false, fmt.Errorf("store: probe the rows of %s: %w", id, err)
		}
		if remaining {
			return false, nil
		}
		// What the chunk loop could not name: the thread's title row and
		// the index rows of its imported history. The mapping table
		// cascades with the thread, but the contentless FTS rows it names
		// do not, so they come off here rather than being left behind.
		if err := deleteThreadSearchThreadTx(tx, id); err != nil {
			return false, err
		}
		result, err := tx.Exec(`DELETE FROM threads WHERE id = ?`, id)
		if err != nil {
			return false, fmt.Errorf("store: delete thread %s: %w", id, err)
		}
		if err := requireRowsAffected(result, fmt.Sprintf("store: delete thread %s", id)); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit delete thread %s: %w", id, err)
	}
	if reads {
		s.holdersMayBeReleased()
	}
	return true, nil
}

// maxReaderCutSQL reads the last entry of ?'s range of
// idx_thread_fork_lineage_ancestor.
const maxReaderCutSQL = `SELECT cut_turn_index, cut_item_index FROM thread_fork_lineage
		 WHERE ancestor_id = ? ORDER BY cut_turn_index DESC, cut_item_index DESC LIMIT 1`

// maxReaderCutTx is the latest cut of a lineage row that reads id: the
// forks read nothing of id from it on. found is false when no fork reads
// id.
func maxReaderCutTx(q sqlQueryer, id string) (timelineRow, bool, error) {
	var cut timelineRow
	err := q.QueryRow(maxReaderCutSQL, id).Scan(&cut.turn, &cut.item)
	if errors.Is(err, sql.ErrNoRows) {
		return timelineRow{}, false, nil
	}
	if err != nil {
		return timelineRow{}, false, fmt.Errorf("store: read the forks' cut of %s: %w", id, err)
	}
	return cut, true, nil
}

// deleteThreadItemsChunk removes one bounded slice of the rows no fork
// reads, those at or after the last reader's cut, while aggregating the
// history stamps the per-item delete trigger would otherwise write one at a
// time. The cut is read in the chunk's transaction, so a reader that
// leaves between chunks releases its rows to the next one. The flag,
// deletes, exact rev/epoch advance, and flag reset share one
// transaction: readers either see the prior chunk or the shortened history
// with its new stamp, and any failure rolls the flag back with the rows.
func (s *Store) deleteThreadItemsChunk(id string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin delete thread %s item chunk: %w", id, err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE threads SET history_bulk_load = 1
		  WHERE id = ? AND history_bulk_load = 0`, id,
	)
	if err != nil {
		return 0, fmt.Errorf("store: begin delete thread %s item history: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: begin delete thread %s item history", id)); err != nil {
		return 0, err
	}
	cut, read, err := maxReaderCutTx(tx, id)
	if err != nil {
		return 0, err
	}
	if !read {
		cut = nothingKept
	}

	// The search index is paced with the rows it describes: a 38k-item
	// thread would otherwise pay for its whole index in one statement,
	// which is the stall this chunking exists to avoid.
	n, err := deleteItemsAndSearchRowsTx(tx, id, deleteThreadItemChunkSQL,
		[]any{id, cut.turn, cut.item},
		fmt.Sprintf("store: delete thread %s items", id),
	)
	if err != nil {
		return 0, err
	}

	result, err = tx.Exec(
		`UPDATE threads
		    SET history_rev = history_rev + ?,
		        history_epoch = history_epoch + ?,
		        history_bulk_load = 0
		  WHERE id = ? AND history_bulk_load = 1`,
		n, n, id,
	)
	if err != nil {
		return 0, fmt.Errorf("store: finish delete thread %s item history: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: finish delete thread %s item history", id)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit delete thread %s item chunk: %w", id, err)
	}
	return n, nil
}
