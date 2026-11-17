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

// BeginThreadDelete marks id deleting, the store's own commit of a delete
// of a row nothing claimed (DeleteThreadPaced): a fork of id admitted
// before it has committed by the time this write gets the writer
// connection, and one after it reads the mark and is refused, so what the
// forks read is settled before the delete releases anything
// (ReleasableAttachments). Marking a marked row again is how a repeated
// delete resumes. A family delete commits its rows itself
// (CommitDeleteClaim) before it drops any. A missing thread is
// sql.ErrNoRows.
func (s *Store) BeginThreadDelete(id string) error {
	result, err := s.db.Exec(`UPDATE threads SET deleting = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: mark thread %s deleting: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: mark thread %s deleting: %w", id, err)
	} else if n == 0 {
		return fmt.Errorf("store: delete thread %s: %w", id, sql.ErrNoRows)
	}
	return nil
}

// A delete of threads with what lives under them (the app's familyDelete)
// is one operation with an id, in four phases, each phase's write here:
//
//  1. Claim (ClaimDeleteFamily): one transaction reads the family
//     (ListDeleteFamily) and writes the operation's id into each row's
//     delete_claim. A claimed row is still listed everywhere, takes what
//     the app queues for it and starts nothing: a queued message starts
//     nothing by itself, so the app's queue writes and withdrawals pass a
//     claim (CheckThreadDeliverable), and every door that starts or moves
//     something refuses it (CheckThreadNotDeleting,
//     CheckThreadExecutionAccess): no session start, dispatch of its queue,
//     send, spawn, fork, handoff, transfer, archive, placement or group
//     change.
//  2. Verify, in the app, writes nothing; a refusal releases every claim
//     in one transaction (ReleaseDeleteClaim).
//  3. Commit (CommitDeleteClaim): one transaction sets deleting on every
//     row the operation claimed. From here the rows are gone to every
//     listing and the delete only moves forward.
//  4. Destroy, in the app, drops each committed row (DeleteThreadPaced),
//     the threads under it first.
//
// The boot releases the claims no operation committed
// (ReleaseUndecidedDeleteClaims) and resumes every committed row
// (ListPendingThreadDeletes).

// ErrThreadDeleting is what a thread a delete claimed or committed refuses:
// execution (CheckThreadExecutionAccess), a placement or group change, a
// handoff and an archive.
var ErrThreadDeleting = errors.New("store: this thread is being deleted")

// ErrThreadDeleteClaimed is ErrThreadDeleting for a thread a delete claimed
// and has not committed: the delete may still be refused, so a start it
// turns away that nobody asked for in the moment is "not now", never a
// failure, and waits for the decision.
var ErrThreadDeleteClaimed error = deleteClaimedRefusal{}

type deleteClaimedRefusal struct{}

func (deleteClaimedRefusal) Error() string        { return ErrThreadDeleting.Error() }
func (deleteClaimedRefusal) Is(target error) bool { return target == ErrThreadDeleting }

// ThreadDeleteState is how far a delete has gone with a thread
// (Thread.Delete).
type ThreadDeleteState int

const (
	// ThreadNotDeleting is a thread no delete holds.
	ThreadNotDeleting ThreadDeleteState = iota
	// ThreadDeleteClaimed is a thread a delete claimed and has not
	// committed (threads.delete_claim).
	ThreadDeleteClaimed
	// ThreadDeleteCommitted is a thread a delete committed
	// (threads.deleting): it is gone for good.
	ThreadDeleteCommitted
)

// deleteRefusal is the fence's refusal of threadID in state, nil for none.
func deleteRefusal(threadID string, state ThreadDeleteState) error {
	switch state {
	case ThreadDeleteClaimed:
		return fmt.Errorf("store: thread %s: %w", threadID, ErrThreadDeleteClaimed)
	case ThreadDeleteCommitted:
		return fmt.Errorf("store: thread %s: %w", threadID, ErrThreadDeleting)
	}
	return nil
}

// CheckThreadDeliverable refuses a thread that will never take a message
// again: one gone to every operation (CheckThreadDeleteAccess: a holder, a
// thread given to another computer) or one whose delete committed. A claim
// passes: a claimed thread takes what the app queues for it, which starts
// nothing until the claim is released.
func (s *Store) CheckThreadDeliverable(thread Thread) error {
	if err := s.CheckThreadDeleteAccess(thread); err != nil {
		return err
	}
	if thread.Delete == ThreadDeleteCommitted {
		return deleteRefusal(thread.ID, thread.Delete)
	}
	return nil
}

// DeleteClaimedError is a delete refused, with nothing changed, because a
// thread it takes is claimed by another delete still deciding.
type DeleteClaimedError struct {
	ThreadID string
}

func (e *DeleteClaimedError) Error() string {
	return fmt.Sprintf("thread %s is already being deleted; nothing was deleted, try again once that delete ends", e.ThreadID)
}

func (e *DeleteClaimedError) Is(target error) bool { return target == ErrThreadDeleting }

// CheckThreadNotDeleting refuses, with ErrThreadDeleting, a thread a delete
// claimed or committed: the check of a door that starts something. A
// missing thread passes: each caller has its own answer for one. Read
// under the thread's action lock it holds until the lock is released,
// because a claim is written before the delete takes the thread's lock to
// verify it.
func (s *Store) CheckThreadNotDeleting(threadID string) error {
	return checkThreadNotDeleting(s.reader(), threadID)
}

func checkThreadNotDeleting(q transferQuerier, threadID string) error {
	var state ThreadDeleteState
	err := q.QueryRow(`SELECT CASE WHEN deleting = 1 THEN 2 WHEN delete_claim <> '' THEN 1 ELSE 0 END
		FROM threads WHERE id = ?`, threadID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("store: read the delete of thread %s: %w", threadID, err)
	}
	return deleteRefusal(threadID, state)
}

// ThreadDeleteClaim reads how far a delete has gone with threadID: the
// delete that claimed it (threads.delete_claim), empty for none, and
// whether a delete committed it (threads.deleting), which a delete that
// never claimed it may have (BeginThreadDelete). A missing thread is
// neither.
func (s *Store) ThreadDeleteClaim(threadID string) (claim string, committed bool, err error) {
	err = s.reader().QueryRow(`SELECT delete_claim, deleting FROM threads WHERE id = ?`, threadID).Scan(&claim, &committed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("store: read the delete of thread %s: %w", threadID, err)
	}
	return claim, committed, nil
}

// ClaimDeleteFamily claims for the delete op, in one transaction, the
// threads ids names and what deleting them takes (ListDeleteFamily). It
// refuses, changing nothing, when one of them is claimed by another delete
// (*DeleteClaimedError). A thread already committed is left as it is: its
// delete only goes forward, and this one joins it. A thread op already
// claimed stays claimed, so a delete reads its family again with the same
// call, which claims the threads a drained spawn added. It returns the
// family as claimed.
func (s *Store) ClaimDeleteFamily(op string, ids []string) (members []DeleteMember, err error) {
	if op == "" {
		return nil, errors.New("store: a delete claim needs its operation's id")
	}
	action := "store: claim a delete"
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	members, err = deleteFamily(tx, ids)
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		if !member.Committed && member.Claim != "" && member.Claim != op {
			return nil, &DeleteClaimedError{ThreadID: member.ThreadID}
		}
	}
	for i, member := range members {
		if member.Committed || member.Claim == op {
			continue
		}
		owner := ""
		if member.Kind == DeleteWorker {
			owner = member.Under
		}
		if err := claimThreadDeleteTx(tx, op, member.ThreadID, owner); err != nil {
			return nil, err
		}
		members[i].Claim = op
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	return members, nil
}

// claimThreadDeleteTx claims id for the delete op. For a worker its owner's
// delete takes, owner names that owner and fences the claim: it holds only
// while owner still owns the thread (ErrWorkerNotOwned), so a delete
// admitted from elsewhere claims a worker only through its owner here. A
// thread committed or claimed by another delete is not claimed
// (ErrThreadDeleting).
func claimThreadDeleteTx(tx *sql.Tx, op, id, owner string) error {
	query := `UPDATE threads SET delete_claim = ?1 WHERE id = ?2 AND deleting = 0 AND delete_claim IN ('', ?1)`
	args := []any{op, id}
	if owner != "" {
		query += ` AND ` + ownedBySQL("?3")
		args = append(args, owner)
	}
	result, err := tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: claim the delete of thread %s: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: claim the delete of thread %s: %w", id, err)
	} else if n > 0 {
		return nil
	}
	if err := checkThreadNotDeleting(tx, id); err != nil {
		return err
	}
	if owner != "" {
		return ErrWorkerNotOwned
	}
	return fmt.Errorf("store: claim the delete of thread %s: %w", id, sql.ErrNoRows)
}

// ReleaseDeleteClaim takes back every claim of the delete op that did not
// commit, in one transaction: a refused delete changes nothing.
func (s *Store) ReleaseDeleteClaim(op string) error {
	if _, err := s.db.Exec(`UPDATE threads SET delete_claim = '' WHERE delete_claim = ?1 AND delete_claim <> '' AND deleting = 0`, op); err != nil {
		return fmt.Errorf("store: release the claims of delete %s: %w", op, err)
	}
	return nil
}

// CommitDeleteClaim commits every thread the delete op claimed, in one
// transaction: each is marked deleting, and is gone to every listing from
// here (owned_threads). It returns the threads it marked.
func (s *Store) CommitDeleteClaim(op string) (_ []string, err error) {
	action := fmt.Sprintf("store: commit delete %s", op)
	ids, err := queryIDs(s.db, `UPDATE threads SET deleting = 1
		 WHERE delete_claim = ?1 AND delete_claim <> '' AND deleting = 0 RETURNING id`, op)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return ids, nil
}

// ReleaseUndecidedDeleteClaims takes back, at boot, every claim no delete
// committed: the process that held it is gone, and nothing of a claimed
// thread was stopped or removed. It returns how many it released.
func (s *Store) ReleaseUndecidedDeleteClaims() (int64, error) {
	result, err := s.db.Exec(`UPDATE threads SET delete_claim = '' WHERE delete_claim <> '' AND deleting = 0`)
	if err != nil {
		return 0, fmt.Errorf("store: release undecided delete claims: %w", err)
	}
	return result.RowsAffected()
}

// ListPendingThreadDeletes returns the threads whose delete committed and
// did not finish (CommitDeleteClaim, DeleteThreadPaced), and the holders no
// fork reads any more (ListReleasedHolders), for the app to complete at
// boot.
func (s *Store) ListPendingThreadDeletes() ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM threads WHERE deleting = 1`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending thread deletes: %w", err)
	}
	return ids, nil
}

// ListTerminalThreads returns every terminal-mode thread this backend owns
// that no delete has begun on, archived or not. A terminal thread lives
// only as long as its shells, so at boot each one belongs to a process
// that is gone.
func (s *Store) ListTerminalThreads() ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM owned_threads WHERE mode = ?`, threadmode.ModeTerminal)
	if err != nil {
		return nil, fmt.Errorf("store: list terminal threads: %w", err)
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
	// Retired or deleted, the thread is no assistant's any more.
	deletedHome, err := deleteAgentRecordTx(tx, id)
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
	s.noteAgentRecordDeleted(deletedHome)
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
