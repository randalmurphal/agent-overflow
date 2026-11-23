package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The open tickets (tracker_cache_open_tickets, v164): while a person has
// an issue open as a ticket, its pump holds a row that makes the mirror
// keep the issue (mirrorIssueKept), so the ticket's body, card and
// history are stored as any kept issue's are. A pump is named by the
// process's session and its number within it; each pump writes its own
// row and deletes it, so two pumps of one issue, one ending as the other
// starts, never delete each other's row. The row is the ticket's one
// record of its lifetime: opened_at, when the pump bound it on the root's
// read clock, admits a body from any read that began no earlier
// (StoreMirrorTickets), so a read made before a close and a reopen does
// not start the new lifetime's body. An issue whose last keep goes is
// absent to reads at once and FinishMirrorCleanup removes it.

// KeepMirrorTicket records that pump of session has issueID open in
// rootID's mirror, bound at opened (a read begun on the root's clock for
// the bind), which keeps the issue until DropMirrorTicket. A pump has one
// row: keeping again moves it to rootID and issueID with the new bind,
// and the same keep again writes nothing and keeps its first bind. One
// statement, and one more when it wrote nothing, which tells the same
// keep from a root that is gone or detached (ErrTrackerCacheGone).
func (s *Store) KeepMirrorTicket(rootID int64, issueID string, session, pump int64, opened MirrorRead) error {
	if issueID == "" || pump <= 0 {
		return fmt.Errorf("%w: an open ticket names its issue and its pump", ErrInvalidTrackerCache)
	}
	bind, err := opened.in(rootID)
	if err != nil {
		return err
	}
	kept, err := execChangedTx(s.db, `INSERT INTO tracker_cache_open_tickets(root_id, issue_id, session, pump, opened_at)
		SELECT id, ?2, ?3, ?4, ?5 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NOT NULL
		ON CONFLICT(session, pump) DO UPDATE SET root_id = excluded.root_id, issue_id = excluded.issue_id, opened_at = excluded.opened_at
		 WHERE root_id IS NOT excluded.root_id OR issue_id IS NOT excluded.issue_id
		RETURNING pump`, rootID, issueID, session, pump, bind.at)
	if err != nil {
		return fmt.Errorf("store: keep the open ticket %s of tracker mirror %d: %w", issueID, rootID, err)
	}
	if kept {
		return nil
	}
	var same bool
	err = s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM tracker_cache_open_tickets o JOIN tracker_cache_roots r ON r.id = o.root_id
		WHERE o.session = ?3 AND o.pump = ?4 AND o.root_id = ?1 AND o.issue_id = ?2 AND r.connection_id IS NOT NULL)`,
		rootID, issueID, session, pump).Scan(&same)
	switch {
	case err != nil:
		return fmt.Errorf("store: keep the open ticket %s of tracker mirror %d: %w", issueID, rootID, err)
	case !same:
		return fmt.Errorf("store: keep the open ticket %s: root %d: %w", issueID, rootID, ErrTrackerCacheGone)
	}
	return nil
}

// DropMirrorTicket deletes the row of pump of session, if any, and reports
// whether the mirror keeps its issue no longer (unkept), as the same
// transaction leaves it: a ticket that shows the issue as another's parent
// or link then shows it without its card. Two statements when a row was
// deleted, one when none was.
func (s *Store) DropMirrorTicket(session, pump int64) (unkept bool, err error) {
	action := fmt.Sprintf("store: drop the open ticket of pump %d", pump)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var root int64
	var issue string
	err = tx.QueryRow(`DELETE FROM tracker_cache_open_tickets WHERE session = ? AND pump = ? RETURNING root_id, issue_id`,
		session, pump).Scan(&root, &issue)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
	case err != nil:
		return false, fmt.Errorf("%s: %w", action, err)
	default:
		var kept bool
		if err = tx.QueryRow(`SELECT `+mirrorIssueKept("?1", "?2"), root, issue).Scan(&kept); err != nil {
			return false, fmt.Errorf("%s: read the keep of issue %s: %w", action, issue, err)
		}
		unkept = !kept
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return unkept, nil
}

// DropMirrorTicketsOfOtherSessions deletes the rows no pump of session
// wrote: those a process that ended without deleting them left, at most
// a process's cap of open tickets each. It returns how many it deleted.
// One statement, which reads every row: the table holds the open tickets
// of the live process and what earlier ones left.
func (s *Store) DropMirrorTicketsOfOtherSessions(session int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM tracker_cache_open_tickets WHERE session <> ?`, session)
	if err != nil {
		return 0, fmt.Errorf("store: drop the open tickets of earlier sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: drop the open tickets of earlier sessions: %w", err)
	}
	return n, nil
}
