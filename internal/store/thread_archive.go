package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The archive flag has one writer, setThreadArchivedTx. An archive is
// durable at its flip, and what the thread owns follows through a walk the
// app runs after the commit (finishArchive): its owned workers, and at the
// walk's end the archives owed for its workers on paired computers
// (OweRemoteArchives).

// workerFence is what an archive or delete of a worker holds through: its
// owner here, or the link a paired computer's owner reaches it by. The zero
// fence holds for any thread.
type workerFence struct {
	owner string
	link  *LinkFence
}

// condition is the fence over a threads row, with its arguments; empty for
// the zero fence.
func (f workerFence) condition() (string, []any) {
	switch {
	case f.link != nil:
		return linkedBySQL, []any{f.link.Token, f.link.DeviceID}
	case f.owner != "":
		return ownedBySQL("?"), []any{f.owner}
	}
	return "", nil
}

// setThreadArchivedTx writes the archive flag of id in tx and reports
// whether the flag moved, with the row it wrote when it did.
//
// A fence makes an archive a worker's, carried out for its owner: a local
// owner's walk flips the worker only while that owner is archived and owns
// it; a link's archive flips the worker only while the link owns it.
//
// An unarchive ends the archive and withdraws what it owed the paired
// computers (supersedeOwedArchivesTx).
func setThreadArchivedTx(tx *sql.Tx, id string, archived bool, fence workerFence) (Thread, bool, error) {
	write := threadArchivedWrite(id, archived)
	if !archived {
		row, changed, err := applyThreadRowWriteTx(tx, write)
		if err != nil || !changed {
			return row, changed, err
		}
		if err := supersedeOwedArchivesTx(tx, id, nowMillis()); err != nil {
			return Thread{}, false, fmt.Errorf("%s: %w", write.Action, err)
		}
		return row, true, nil
	}
	fenced, args := fence.condition()
	if fenced != "" {
		write.Change += ` AND ` + fenced
		write.ChangeArgs = args
	}
	if fence.owner != "" {
		write.Change += ` AND EXISTS (SELECT 1 FROM threads AS owner WHERE owner.id = ? AND owner.archived = 1)`
		write.ChangeArgs = append(write.ChangeArgs, fence.owner)
	}
	return applyThreadRowWriteTx(tx, write)
}

// setThreadArchived is setThreadArchivedTx in a transaction of its own.
func (s *Store) setThreadArchived(id string, archived bool, fence workerFence) (row Thread, changed bool, err error) {
	action := fmt.Sprintf("store: set thread %s archived %v", id, archived)
	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if row, changed, err = setThreadArchivedTx(tx, id, archived, fence); err != nil {
		return Thread{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return row, changed, nil
}

// threadArchivedWrite is the archive flip as a row write.
func threadArchivedWrite(id string, archived bool) rowWrite {
	flag, action := "1", "archive"
	if !archived {
		flag, action = "0", "unarchive"
	}
	return rowWrite{
		Action:  fmt.Sprintf("store: %s thread %s", action, id),
		ID:      id,
		Set:     "archived = " + flag + ", updated_at = ?",
		SetArgs: []any{nowMillis()},
		Change:  "archived IS NOT " + flag,
	}
}

// supersedeOwedArchivesTx withdraws, in tx and as of now, the archives
// owed for the remote spawns threadID made, its own reopen having undone
// them: one not yet sent is dropped, and one sent becomes a supersede
// under the spawn's next sequence, so the archive in flight arrives as
// already applied. An owed delete or release stays. The archives owed for
// the spawns of the archived workers beneath it stay owed: their own
// owners are still archived.
func supersedeOwedArchivesTx(tx *sql.Tx, threadID string, now int64) error {
	if _, err := tx.Exec(`DELETE FROM thread_owner_ops WHERE op = 'archive' AND sent = 0 AND token IN (
		SELECT token FROM thread_requests WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id <> '')`,
		threadID); err != nil {
		return fmt.Errorf("drop the unsent archives of %s: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE thread_requests SET owner_op_seq = owner_op_seq + 1
		 WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id <> ''
		   AND EXISTS (SELECT 1 FROM thread_owner_ops AS owed
		                WHERE owed.token = thread_requests.token AND owed.op = 'archive')`, threadID); err != nil {
		return fmt.Errorf("supersede the sent archives of %s: %w", threadID, err)
	}
	if _, err := tx.Exec(`UPDATE thread_owner_ops SET op = 'supersede',
		     seq = (SELECT owner_op_seq FROM thread_requests WHERE thread_requests.token = thread_owner_ops.token),
		     sent = 0, owed_at = ?1, next_at = ?1, attempts = 0, last_error = ''
		 WHERE op = 'archive' AND token IN (
		   SELECT token FROM thread_requests WHERE caller_thread_id = ?2 AND kind = 'spawn' AND target_computer_id <> '')`,
		now, threadID); err != nil {
		return fmt.Errorf("supersede the sent archives of %s: %w", threadID, err)
	}
	return nil
}

// ArchiveLinkedWorker archives threadID for its owner on a paired computer:
// the owner operation seq that owner sent through the link token, from
// deviceID. In one transaction it refuses a thread the link does not own
// (ErrWorkerNotOwned), takes seq once, and archives the thread
// (setThreadArchivedTx with the link's fence). The seq the link took last,
// sent again, walks again while the thread is still archived: its
// owner keeps it owed until every worker below is archived, and the walk is
// what archives them. applied is false for an older seq, or the same one
// once the thread was reopened, which changes nothing; changed is whether
// the flag moved.
func (s *Store) ArchiveLinkedWorker(threadID, token, deviceID string, seq int64) (row Thread, changed, applied bool, err error) {
	action := fmt.Sprintf("store: archive linked worker %s for spawn %s", threadID, token)
	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, false, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var taken int64
	switch err := tx.QueryRow(`SELECT owner_op_seq FROM thread_request_receipts
		 WHERE token = ? AND owner_device_id = ? AND target_thread_id = ?`, token, deviceID, threadID).Scan(&taken); {
	case errors.Is(err, sql.ErrNoRows):
		return Thread{}, false, false, ErrWorkerNotOwned
	case err != nil:
		return Thread{}, false, false, fmt.Errorf("%s: read the link: %w", action, err)
	}
	if seq < taken {
		return Thread{}, false, false, nil
	}
	fence := workerFence{link: &LinkFence{Token: token, DeviceID: deviceID}}
	condition, args := fence.condition()
	if seq == taken {
		var again bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM threads WHERE id = ? AND archived = 1 AND `+condition+`)`,
			append([]any{threadID}, args...)...).Scan(&again); err != nil {
			return Thread{}, false, false, fmt.Errorf("%s: walk seq %d again: %w", action, seq, err)
		}
		if !again {
			return Thread{}, false, false, nil
		}
		if row, err = getThreadTx(tx, threadID); err != nil {
			return Thread{}, false, false, fmt.Errorf("%s: %w", action, err)
		}
		return row, false, true, nil
	}
	var owned bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM threads WHERE id = ? AND `+condition+`)`,
		append([]any{threadID}, args...)...).Scan(&owned); err != nil {
		return Thread{}, false, false, fmt.Errorf("%s: read the link: %w", action, err)
	}
	if !owned {
		return Thread{}, false, false, ErrWorkerNotOwned
	}
	if _, err := tx.Exec(`UPDATE thread_request_receipts SET owner_op_seq = ? WHERE token = ?`, seq, token); err != nil {
		return Thread{}, false, false, fmt.Errorf("%s: take seq %d: %w", action, seq, err)
	}
	if row, changed, err = setThreadArchivedTx(tx, threadID, true, fence); err != nil {
		return Thread{}, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, false, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return row, changed, true, nil
}
