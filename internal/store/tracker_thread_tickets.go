package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/threadmode"
)

// How a thread came to be linked to its ticket (thread_tickets.origin).
const (
	// TicketOriginStartWork: Start work on the ticket created the thread.
	TicketOriginStartWork = "start_work"
	// TicketOriginAsk: Ask about the ticket created the thread.
	TicketOriginAsk = "ask"
	// TicketOriginManual: the user linked an existing thread.
	TicketOriginManual = "manual"
	// TicketOriginAgent: the thread's agent linked it through a tool.
	TicketOriginAgent = "agent"
)

// ErrInvalidThreadTicket is returned by SetThreadTicket for a value the
// table refuses, and for a thread that is a fork holder.
var ErrInvalidThreadTicket = errors.New("store: invalid thread ticket")

// TicketRef names one issue: the tracker kind (jira or linear), the site
// as tracker_connections.site holds it for that kind, and the tracker's own
// issue ID.
type TicketRef struct {
	Kind    string
	Site    string
	IssueID string
}

// ThreadTicket is the one ticket a thread is linked to (migration v159).
// IssueKey and IssueTitle are as last seen; LinkedAt is unix milliseconds.
type ThreadTicket struct {
	ThreadID   string
	Ticket     TicketRef
	IssueKey   string
	IssueTitle string
	Origin     string
	LinkedAt   int64
}

const threadTicketColumns = `thread_id, kind, site, issue_id, issue_key, issue_title, origin, linked_at`

func scanThreadTicket(scan func(...any) error) (ThreadTicket, error) {
	var t ThreadTicket
	err := scan(&t.ThreadID, &t.Ticket.Kind, &t.Ticket.Site, &t.Ticket.IssueID, &t.IssueKey, &t.IssueTitle, &t.Origin, &t.LinkedAt)
	return t, err
}

func (r TicketRef) validate() error {
	switch {
	case r.Kind != TrackerKindJira && r.Kind != TrackerKindLinear:
		return fmt.Errorf("%w: unknown ticket kind %q", ErrInvalidThreadTicket, r.Kind)
	case r.Site == "" || r.IssueID == "":
		return fmt.Errorf("%w: a ticket needs its site and issue ID", ErrInvalidThreadTicket)
	}
	return nil
}

func (t ThreadTicket) validate() error {
	if err := t.Ticket.validate(); err != nil {
		return err
	}
	switch t.Origin {
	case TicketOriginStartWork, TicketOriginAsk, TicketOriginManual, TicketOriginAgent:
	default:
		return fmt.Errorf("%w: unknown origin %q", ErrInvalidThreadTicket, t.Origin)
	}
	switch {
	case t.ThreadID == "":
		return fmt.Errorf("%w: a ticket link needs its thread", ErrInvalidThreadTicket)
	case t.IssueKey == "":
		return fmt.Errorf("%w: a ticket link needs the issue key", ErrInvalidThreadTicket)
	case t.LinkedAt <= 0:
		return fmt.Errorf("%w: a ticket link needs its time", ErrInvalidThreadTicket)
	}
	return nil
}

// SetThreadTicket links t.ThreadID to t's ticket and reports whether the
// row changed. Linking another ticket replaces the link. Setting the
// ticket the thread already has records its key and title and keeps the
// link's origin and time. A missing thread fails with sql.ErrNoRows, a
// thread a delete claimed or committed with ErrThreadDeleting, a thread
// with an outgoing move or an unsealed transfer with *ThreadTransferError,
// and a holder or an invalid value with ErrInvalidThreadTicket.
func (s *Store) SetThreadTicket(t ThreadTicket) (changed bool, err error) {
	if err := t.validate(); err != nil {
		return false, err
	}
	action := fmt.Sprintf("store: set thread %s ticket", t.ThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if err := checkTicketThreadWritableTx(tx, t.ThreadID); err != nil {
		return false, err
	}
	var mode string
	if err := tx.QueryRow(`SELECT mode FROM threads WHERE id = ?`, t.ThreadID).Scan(&mode); err != nil {
		return false, fmt.Errorf("%s: read thread: %w", action, err)
	}
	if mode == threadmode.ModeHolder {
		return false, fmt.Errorf("%w: thread %s is a fork holder", ErrInvalidThreadTicket, t.ThreadID)
	}
	written, err := queryIDs(tx, `INSERT INTO thread_tickets(`+threadTicketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(thread_id) DO UPDATE SET
		  kind = excluded.kind, site = excluded.site, issue_id = excluded.issue_id,
		  issue_key = excluded.issue_key, issue_title = excluded.issue_title,
		  origin = CASE WHEN `+sameTicketSQL+` THEN origin ELSE excluded.origin END,
		  linked_at = CASE WHEN `+sameTicketSQL+` THEN linked_at ELSE excluded.linked_at END
		WHERE NOT (`+sameTicketSQL+`) OR issue_key IS NOT excluded.issue_key OR issue_title IS NOT excluded.issue_title
		RETURNING thread_id`,
		t.ThreadID, t.Ticket.Kind, t.Ticket.Site, t.Ticket.IssueID, t.IssueKey, t.IssueTitle, t.Origin, t.LinkedAt)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return len(written) > 0, nil
}

// sameTicketSQL is true in SetThreadTicket's upsert when the stored row
// names the ticket being set.
const sameTicketSQL = `(kind = excluded.kind AND site = excluded.site AND issue_id = excluded.issue_id)`

// checkTicketThreadWritableTx refuses a link write to a thread this
// computer may not change: one with an outgoing move or an unsealed
// transfer (*ThreadTransferError), and one a delete claimed or committed
// (ErrThreadDeleting). Both are read in the write's transaction, which
// serializes the write with a transfer reservation and a delete claim.
func checkTicketThreadWritableTx(tx *sql.Tx, threadID string) error {
	if err := checkThreadTransferAccess(tx, threadID); err != nil {
		return err
	}
	return checkThreadNotDeleting(tx, threadID)
}

// ticketThreadWritableSQL is checkTicketThreadWritableTx and the holder
// refusal as a predicate on thread_tickets.thread_id, for a write to many
// links at once.
var ticketThreadWritableSQL = `EXISTS (SELECT 1 FROM threads
	WHERE threads.id = thread_tickets.thread_id AND threads.mode <> ` + sqlTextLiteral(threadmode.ModeHolder) + `
	  AND threads.deleting = 0 AND threads.delete_claim = '' AND ` + threadTransferReadableExpr("threads.") + `)`

// ticketThreadOwnedSQL keeps the links of the threads this computer owns
// (owned_threads), as the sidebar and the public thread read do: a thread
// moved away whose cache stays here reads as having no link.
const ticketThreadOwnedSQL = `EXISTS (SELECT 1 FROM owned_threads WHERE owned_threads.id = thread_tickets.thread_id)`

// ClearThreadTicket removes threadID's ticket link and reports whether it
// had one. It refuses a thread SetThreadTicket refuses for its transfer or
// its delete; a missing thread or a holder has no link to clear.
func (s *Store) ClearThreadTicket(threadID string) (cleared bool, err error) {
	action := fmt.Sprintf("store: clear thread %s ticket", threadID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if err := checkTicketThreadWritableTx(tx, threadID); err != nil {
		return false, err
	}
	result, err := tx.Exec(`DELETE FROM thread_tickets WHERE thread_id = ?`, threadID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return n > 0, nil
}

// GetThreadTicket reads threadID's ticket link; false when it has none or
// this computer does not own the thread (owned_threads).
func (s *Store) GetThreadTicket(threadID string) (ThreadTicket, bool, error) {
	t, err := scanThreadTicket(s.reader().QueryRow(`SELECT `+threadTicketColumns+` FROM thread_tickets
		WHERE thread_id = ? AND `+ticketThreadOwnedSQL, threadID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadTicket{}, false, nil
	}
	if err != nil {
		return ThreadTicket{}, false, fmt.Errorf("store: read thread %s ticket: %w", threadID, err)
	}
	return t, true, nil
}

// ListThreadTickets reads the ticket links of the threads threadIDs names,
// keyed by thread; a thread with no link, or one this computer does not
// own, has no entry.
func (s *Store) ListThreadTickets(threadIDs []string) (map[string]ThreadTicket, error) {
	encoded, err := json.Marshal(uniqueNonEmptyStrings(threadIDs))
	if err != nil {
		return nil, fmt.Errorf("store: list thread tickets: encode: %w", err)
	}
	links, err := s.queryThreadTickets(`SELECT `+threadTicketColumns+` FROM thread_tickets
		WHERE thread_id IN (SELECT value FROM json_each(?)) AND `+ticketThreadOwnedSQL, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("store: list thread tickets: %w", err)
	}
	out := make(map[string]ThreadTicket, len(links))
	for _, link := range links {
		out[link.ThreadID] = link
	}
	return out, nil
}

// ListThreadsForTicket lists the links of every thread this computer owns
// that is linked to ticket, in thread order.
func (s *Store) ListThreadsForTicket(ticket TicketRef) ([]ThreadTicket, error) {
	links, err := s.queryThreadTickets(`SELECT `+threadTicketColumns+` FROM thread_tickets
		WHERE kind = ? AND site = ? AND issue_id = ? AND `+ticketThreadOwnedSQL+` ORDER BY thread_id`, ticket.Kind, ticket.Site, ticket.IssueID)
	if err != nil {
		return nil, fmt.Errorf("store: list threads of %s ticket %s: %w", ticket.Kind, ticket.IssueID, err)
	}
	return links, nil
}

// RecordTicketSeen records key and title as the last seen of ticket on
// every thread linked to it that SetThreadTicket could write, and returns
// the threads whose link changed, in thread order.
func (s *Store) RecordTicketSeen(ticket TicketRef, key, title string) ([]string, error) {
	if err := ticket.validate(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: a ticket needs its key", ErrInvalidThreadTicket)
	}
	ids, err := queryIDs(s.db, `UPDATE thread_tickets SET issue_key = ?1, issue_title = ?2
		WHERE kind = ?3 AND site = ?4 AND issue_id = ?5 AND (issue_key IS NOT ?1 OR issue_title IS NOT ?2)
		  AND `+ticketThreadWritableSQL+`
		RETURNING thread_id`, key, title, ticket.Kind, ticket.Site, ticket.IssueID)
	if err != nil {
		return nil, fmt.Errorf("store: record %s ticket %s as seen: %w", ticket.Kind, ticket.IssueID, err)
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *Store) queryThreadTickets(query string, args ...any) ([]ThreadTicket, error) {
	rows, err := s.reader().QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	var out []ThreadTicket
	for rows.Next() {
		link, err := scanThreadTicket(rows.Scan)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, link)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
