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
// process's session and its number within it, which grows with every
// pump; each pump writes its own row and deletes it. The row is the
// ticket's one record of its lifetime, its bind: opened_at, the order
// time the root's read clock gave the read that bound it, names the bind
// (MirrorBind). An issue has at most one row, the current bind
// (idx_tracker_cache_open_tickets_issue is unique): a bind deletes the
// issue's earlier rows, whose pumps ended and whose drops are owed or
// deferred, and an earlier pump of the session cannot bind the issue
// again after a later one, so once a later bind existed an earlier one
// is never current again, in whatever order the rows are dropped. A read
// a pump makes carries its bind (MirrorTicket.Bind), and the write admits
// its body only while that bind is the issue's row (StoreMirrorTickets).
// An issue whose last keep goes is absent to reads at once and
// FinishMirrorCleanup removes it.

// MirrorBind names one bind of an open ticket: the order time of the read
// that bound it, unique to the bind within the root.
type MirrorBind int64

// KeepMirrorTicket records that pump of session has issueID open in
// rootID's mirror, bound at opened (a read begun on the root's clock for
// the bind), which keeps the issue until DropMirrorTicket. It returns the
// bind, which every read the pump makes carries, and reports whether the
// keep made the issue's record shown again (shown): the mirror holds a
// record of it that nothing kept, so no read served it (a ticket closed
// and opened again before the cleanup removed the record). The bind
// deletes the issue's other rows: their pumps ended. A pump keeps one
// issue of one root: the same keep again writes nothing and returns its
// first bind; a keep of another issue or root by a pump that has a row,
// and a keep by a pump of the session earlier than one that has the
// issue's row, fail (ErrInvalidTrackerCache). One transaction: the probe
// of the issue's keep and its rows, the delete of the others when there
// are any, and the insert, with one more statement when the insert wrote
// nothing, which tells the same keep from another and from a root that is
// gone or detached (ErrTrackerCacheGone).
func (s *Store) KeepMirrorTicket(rootID int64, issueID string, session, pump int64, opened MirrorRead) (bind MirrorBind, shown bool, err error) {
	if issueID == "" || pump <= 0 {
		return 0, false, fmt.Errorf("%w: an open ticket names its issue and its pump", ErrInvalidTrackerCache)
	}
	at, err := opened.in(rootID)
	if err != nil {
		return 0, false, err
	}
	if at.at <= 0 {
		return 0, false, fmt.Errorf("%w: an open ticket is bound at a read", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: keep the open ticket %s of tracker mirror %d", issueID, rootID)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var hidden, later, others bool
	if err = tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM tracker_cache_issues WHERE root_id = ?1 AND issue_id = ?2) AND NOT `+
		mirrorIssueKept("?1", "?2")+`,
		EXISTS (SELECT 1 FROM tracker_cache_open_tickets WHERE root_id = ?1 AND issue_id = ?2 AND session = ?3 AND pump > ?4),
		EXISTS (SELECT 1 FROM tracker_cache_open_tickets WHERE root_id = ?1 AND issue_id = ?2 AND NOT (session = ?3 AND pump = ?4))`,
		rootID, issueID, session, pump).Scan(&hidden, &later, &others); err != nil {
		return 0, false, fmt.Errorf("%s: read the keep of the issue: %w", action, err)
	}
	if later {
		return 0, false, fmt.Errorf("%w: pump %d of session %d binds issue %s after a later pump did", ErrInvalidTrackerCache, pump, session, issueID)
	}
	if others {
		if _, err = tx.Exec(`DELETE FROM tracker_cache_open_tickets WHERE root_id = ?1 AND issue_id = ?2 AND NOT (session = ?3 AND pump = ?4)`,
			rootID, issueID, session, pump); err != nil {
			return 0, false, fmt.Errorf("%s: delete the earlier binds: %w", action, err)
		}
	}
	err = tx.QueryRow(`INSERT INTO tracker_cache_open_tickets(root_id, issue_id, session, pump, opened_at)
		SELECT id, ?2, ?3, ?4, ?5 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NOT NULL
		ON CONFLICT DO NOTHING
		RETURNING opened_at`, rootID, issueID, session, pump, at.at).Scan(&bind)
	kept := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var other bool
		err = tx.QueryRow(`SELECT coalesce((SELECT o.opened_at FROM tracker_cache_open_tickets o JOIN tracker_cache_roots r ON r.id = o.root_id
			  WHERE o.session = ?3 AND o.pump = ?4 AND o.root_id = ?1 AND o.issue_id = ?2 AND r.connection_id IS NOT NULL), 0),
			EXISTS (SELECT 1 FROM tracker_cache_open_tickets WHERE session = ?3 AND pump = ?4 AND (root_id <> ?1 OR issue_id <> ?2))`,
			rootID, issueID, session, pump).Scan(&bind, &other)
		switch {
		case err != nil:
			return 0, false, fmt.Errorf("%s: %w", action, err)
		case other:
			return 0, false, fmt.Errorf("%w: pump %d of session %d keeps another open ticket", ErrInvalidTrackerCache, pump, session)
		case bind == 0:
			return 0, false, fmt.Errorf("store: keep the open ticket %s: root %d: %w", issueID, rootID, ErrTrackerCacheGone)
		}
	case err != nil:
		return 0, false, fmt.Errorf("%s: %w", action, err)
	}
	if err = tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return bind, kept && hidden, nil
}

// DropMirrorTicket deletes the row of pump of session, if any, and reports
// whether the mirror keeps its issue no longer (unkept), as the same
// transaction leaves it: a ticket that shows the issue as another's parent
// or link then shows it without its card, until a keep shows it again
// (KeepMirrorTicket). Two statements when a row was deleted, one when none
// was.
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
