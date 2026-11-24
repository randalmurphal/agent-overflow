package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// The open tickets (tracker_cache_open_tickets, v164): while a person has
// an issue open as a ticket, its pump holds a row that makes the mirror
// keep the issue (mirrorIssueKept), so the ticket's body, card and
// history are stored as any kept issue's are. A pump is named by the
// process's session and its number within it, which grows with every
// pump; each pump writes its own row and deletes it. The row only keeps
// the issue: an issue may have more than one (a closed ticket's drop owed
// beside the reopened ticket's keep), and what bounds them is their
// cleanup (the owed drop retried, the next process's delete of what this
// one left), not the cap of open tickets. An issue whose last keep goes is
// absent to reads at once and FinishMirrorCleanup removes it.
//
// Which reads of an open ticket write its body is the bind's rule, kept in
// this process's memory (mirrorBinds): a keep issues a pending bind
// (KeepMirrorTicket); the caller makes it the issue's current bind once
// the keep committed and the ticket is still open (ActivateMirrorBind),
// and ends it as the ticket closes (EndMirrorBind). A read a pump makes
// carries its bind (MirrorTicket.Bind), and a ticket write admits its
// body and its opening only while that bind is the issue's current one,
// looked up once per ticket in the write's transaction: a write that
// passed the lookup commits though the ticket closed after it, and every
// write whose lookup follows the end is refused. A read no open ticket
// made carries no bind, and a person's opening confirmed on the cached
// body (ConfirmMirrorBody) records that read's own opening: neither ever
// records a bind's.

// MirrorBind is one bind of an open ticket, which the store issues
// (KeepMirrorTicket): the root and the issue it names, a number no other
// bind of the process has, and its opening, the order time of the read
// that kept the issue, which a check by card under it records on the body
// (StoreMirrorTickets). Its value is opaque; the zero value is no bind, a
// read no open ticket made. A bind names its root by ID, which no other
// root ever has: the roots' sequence survives a detach, a drain and a
// restore (TestARestoreNeverReusesARootID), and a write naming a root that
// ended fails (ErrTrackerCacheGone), so a bind of an ended root admits
// nothing, current or not.
type MirrorBind struct {
	n      uint64
	root   int64
	issue  string
	opened int64
}

// IsZero reports that b is no bind.
func (b MirrorBind) IsZero() bool { return b == MirrorBind{} }

// mirrorBinds is the current bind of each issue an open ticket of this
// process keeps. Its mutex is a leaf: no SQL runs under it and nothing
// under it takes another lock. A ticket write takes it in its transaction,
// holding the writer connection, for its admission lookup.
type mirrorBinds struct {
	mu sync.Mutex
	// last is the number of the last bind issued.
	last    uint64
	current map[mirrorBindKey]MirrorBind
}

type mirrorBindKey struct {
	root  int64
	issue string
}

// issue is a new bind of issueID in rootID opened at opened.
func (m *mirrorBinds) issue(rootID int64, issueID string, opened int64) MirrorBind {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last++
	return MirrorBind{n: m.last, root: rootID, issue: issueID, opened: opened}
}

// mirrorAdmission is what a ticket write may record of a ticket: whether
// it was read under the issue's current bind or under none (admitted), and
// the opening of that bind (zero for none).
type mirrorAdmission struct {
	admitted, unbound bool
	opened            int64
}

// admit looks up, once per ticket, whether each of tickets written to
// rootID is admitted: read under no bind, or under the issue's current one
// in rootID. The caller is the write's transaction.
func (m *mirrorBinds) admit(rootID int64, tickets []MirrorTicket) []mirrorAdmission {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mirrorAdmission, len(tickets))
	for i, t := range tickets {
		switch {
		case t.Bind.IsZero():
			out[i] = mirrorAdmission{admitted: true, unbound: true}
		case m.current[mirrorBindKey{rootID, t.Card.IssueID}] == t.Bind:
			out[i] = mirrorAdmission{admitted: true, opened: t.Bind.opened}
		}
	}
	return out
}

// ActivateMirrorBind makes b, a bind a keep that committed issued, the
// current bind of its issue, in place of any other: the caller has just
// found the ticket it was issued for still open. No bind is no change.
func (s *Store) ActivateMirrorBind(b MirrorBind) {
	if b.IsZero() {
		return
	}
	m := &s.mirrorBinds
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		m.current = map[mirrorBindKey]MirrorBind{}
	}
	m.current[mirrorBindKey{b.root, b.issue}] = b
}

// EndMirrorBind ends b: no write admits it from then on. Ending a bind
// that is not its issue's current one (replaced, ended, or never
// activated) does nothing.
func (s *Store) EndMirrorBind(b MirrorBind) {
	m := &s.mirrorBinds
	m.mu.Lock()
	defer m.mu.Unlock()
	if k := (mirrorBindKey{b.root, b.issue}); !b.IsZero() && m.current[k] == b {
		delete(m.current, k)
	}
}

// KeepMirrorTicket records that pump of session has issueID open in
// rootID's mirror, kept at opened (a read begun on the root's clock for
// the keep), which keeps the issue until DropMirrorTicket. It returns a
// pending bind, which no write admits until the caller activates it
// (ActivateMirrorBind), and reports whether the keep made the issue's
// record shown again (shown): the mirror holds a record of it that nothing
// kept, so no read served it (a ticket closed and opened again before the
// cleanup removed the record). A pump keeps one issue of one root: the
// same keep again writes nothing and returns a bind of its first opening;
// a keep of another issue or root by a pump that has a row fails
// (ErrInvalidTrackerCache). One transaction: the probe of the issue's
// keep and the insert, with one more statement when the insert wrote
// nothing, which tells the same keep from another and from a root that is
// gone or detached (ErrTrackerCacheGone).
func (s *Store) KeepMirrorTicket(rootID int64, issueID string, session, pump int64, opened MirrorRead) (bind MirrorBind, shown bool, err error) {
	if issueID == "" || pump <= 0 {
		return MirrorBind{}, false, fmt.Errorf("%w: an open ticket names its issue and its pump", ErrInvalidTrackerCache)
	}
	at, err := opened.in(rootID)
	if err != nil {
		return MirrorBind{}, false, err
	}
	if at.at <= 0 {
		return MirrorBind{}, false, fmt.Errorf("%w: an open ticket is kept at a read", ErrInvalidTrackerCache)
	}
	action := fmt.Sprintf("store: keep the open ticket %s of tracker mirror %d", issueID, rootID)
	tx, err := s.db.Begin()
	if err != nil {
		return MirrorBind{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var hidden bool
	if err = tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM tracker_cache_issues WHERE root_id = ?1 AND issue_id = ?2) AND NOT `+
		mirrorIssueKept("?1", "?2"), rootID, issueID).Scan(&hidden); err != nil {
		return MirrorBind{}, false, fmt.Errorf("%s: read the keep of the issue: %w", action, err)
	}
	var openedAt int64
	err = tx.QueryRow(`INSERT INTO tracker_cache_open_tickets(root_id, issue_id, session, pump, opened_at)
		SELECT id, ?2, ?3, ?4, ?5 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NOT NULL
		ON CONFLICT DO NOTHING
		RETURNING opened_at`, rootID, issueID, session, pump, at.at).Scan(&openedAt)
	kept := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var other bool
		err = tx.QueryRow(`SELECT coalesce((SELECT o.opened_at FROM tracker_cache_open_tickets o JOIN tracker_cache_roots r ON r.id = o.root_id
			  WHERE o.session = ?3 AND o.pump = ?4 AND o.root_id = ?1 AND o.issue_id = ?2 AND r.connection_id IS NOT NULL), 0),
			EXISTS (SELECT 1 FROM tracker_cache_open_tickets WHERE session = ?3 AND pump = ?4 AND (root_id <> ?1 OR issue_id <> ?2))`,
			rootID, issueID, session, pump).Scan(&openedAt, &other)
		switch {
		case err != nil:
			return MirrorBind{}, false, fmt.Errorf("%s: %w", action, err)
		case other:
			return MirrorBind{}, false, fmt.Errorf("%w: pump %d of session %d keeps another open ticket", ErrInvalidTrackerCache, pump, session)
		case openedAt == 0:
			return MirrorBind{}, false, fmt.Errorf("store: keep the open ticket %s: root %d: %w", issueID, rootID, ErrTrackerCacheGone)
		}
	case err != nil:
		return MirrorBind{}, false, fmt.Errorf("%s: %w", action, err)
	}
	bind = s.mirrorBinds.issue(rootID, issueID, openedAt)
	if err = tx.Commit(); err != nil {
		return MirrorBind{}, false, fmt.Errorf("%s: commit: %w", action, err)
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
