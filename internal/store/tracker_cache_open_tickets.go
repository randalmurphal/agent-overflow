package store

import (
	"fmt"
)

// The open tickets (tracker_cache_open_tickets, v163): while a person has
// an issue open as a ticket, its pump holds a row that makes the mirror
// keep the issue (mirrorIssueKept), so the ticket's document, card and
// history are stored as any kept issue's are. A pump is named by the
// process's session and its number within it; each pump writes its own
// row and deletes it, so two pumps of one issue, one ending as the other
// starts, never delete each other's row. An issue whose last keep goes is
// absent to reads at once and FinishMirrorCleanup removes it.

// KeepMirrorTicket records that pump of session has issueID open in
// rootID's mirror, which keeps the issue until DropMirrorTicket. A pump
// has one row: keeping again moves it to rootID and issueID, and the same
// keep again writes nothing. One statement, and one more when it wrote
// nothing, which tells the same keep from a root that is gone or
// detached (ErrTrackerCacheGone).
func (s *Store) KeepMirrorTicket(rootID int64, issueID string, session, pump int64) error {
	if issueID == "" || pump <= 0 {
		return fmt.Errorf("%w: an open ticket names its issue and its pump", ErrInvalidTrackerCache)
	}
	kept, err := execChangedTx(s.db, `INSERT INTO tracker_cache_open_tickets(root_id, issue_id, session, pump)
		SELECT id, ?2, ?3, ?4 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NOT NULL
		ON CONFLICT(session, pump) DO UPDATE SET root_id = excluded.root_id, issue_id = excluded.issue_id
		 WHERE root_id IS NOT excluded.root_id OR issue_id IS NOT excluded.issue_id
		RETURNING pump`, rootID, issueID, session, pump)
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

// DropMirrorTicket deletes the row of pump of session, if any: its issue
// is kept no longer unless something else keeps it. One statement.
func (s *Store) DropMirrorTicket(session, pump int64) error {
	if _, err := s.db.Exec(`DELETE FROM tracker_cache_open_tickets WHERE session = ? AND pump = ?`, session, pump); err != nil {
		return fmt.Errorf("store: drop the open ticket of pump %d: %w", pump, err)
	}
	return nil
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
