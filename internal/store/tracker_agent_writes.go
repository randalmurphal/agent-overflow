package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ErrInvalidAgentWrite is a tracker_agent_writes row the table refuses.
var ErrInvalidAgentWrite = errors.New("store: invalid agent write")

// The states of an agent's write (tracker_agent_writes.state): sent before
// the tracker answered (a row left in it was cut off and is unknown),
// applied, and unknown (sent, and not answered in full).
const (
	AgentWriteSent    = "sent"
	AgentWriteApplied = "applied"
	AgentWriteUnknown = "unknown"
)

// AgentWrite is one part of a tracker write an agent made (migration
// v171): the ticket it touched as a TicketRef keys one (the issue empty
// for a create the tracker did not confirm, Project then naming where it
// was made), the key a create made (Key, empty for any other step), the
// account the tracker records as its author, the thread and
// the call, the step, its state, the digest of what a comment or a create
// adds, the comment an applied comment made, the card's tracker update
// time before the write and after its refresh (0 when not read), when this
// computer sent it (Sent) and when it recorded it (At), Unix milliseconds.
type AgentWrite struct {
	ID       int64
	Ticket   TicketRef
	Key      string
	Project  string
	Account  string
	ThreadID string
	CallID   string
	Step     string
	State    string
	Digest   string
	Comment  string
	Before   int64
	After    int64
	Sent     int64
	At       int64
}

func (w AgentWrite) check() error {
	switch {
	case w.Ticket.Kind != "jira" && w.Ticket.Kind != "linear":
		return fmt.Errorf("%w: kind %q", ErrInvalidAgentWrite, w.Ticket.Kind)
	case w.Ticket.Site == "" || w.ThreadID == "" || w.CallID == "" || w.Sent <= 0 || w.At <= 0:
		return fmt.Errorf("%w: a write needs its site, thread, call and times", ErrInvalidAgentWrite)
	case w.Ticket.IssueID == "" && w.Step != "create":
		return fmt.Errorf("%w: a %s needs its issue", ErrInvalidAgentWrite, w.Step)
	case w.Key != "" && w.Step != "create":
		return fmt.Errorf("%w: only a create names the key it made", ErrInvalidAgentWrite)
	}
	switch w.State {
	case AgentWriteSent, AgentWriteApplied, AgentWriteUnknown:
	default:
		return fmt.Errorf("%w: state %q", ErrInvalidAgentWrite, w.State)
	}
	return nil
}

const agentWriteColumns = `id, kind, site, issue_id, issue_key, project, account, thread_id, call_id, step, state, digest, comment_id, before_ms, after_ms, sent_ms, at_ms`

func scanAgentWrite(row interface{ Scan(...any) error }) (AgentWrite, error) {
	var w AgentWrite
	err := row.Scan(&w.ID, &w.Ticket.Kind, &w.Ticket.Site, &w.Ticket.IssueID, &w.Key, &w.Project, &w.Account, &w.ThreadID, &w.CallID,
		&w.Step, &w.State, &w.Digest, &w.Comment, &w.Before, &w.After, &w.Sent, &w.At)
	return w, err
}

func insertAgentWriteTx(tx *sql.Tx, w AgentWrite) (int64, error) {
	if err := w.check(); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO tracker_agent_writes(kind, site, issue_id, issue_key, project, account, thread_id, call_id, step, state, digest, comment_id, before_ms, after_ms, sent_ms, at_ms)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16)`,
		w.Ticket.Kind, w.Ticket.Site, w.Ticket.IssueID, w.Key, w.Project, w.Account, w.ThreadID, w.CallID, w.Step, w.State, w.Digest, w.Comment, w.Before, w.After, w.Sent, w.At)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ClaimAgentWrite is the claim of w, a comment or a create about to be
// sent: in one transaction, the newest row of w's thread and digest sent at
// since or later when there is one (claimed false), and otherwise w, inserted
// in state sent (claimed true), which RecordAgentWrites replaces with the
// outcome. The caller decides what an earlier row means
// (trackerapp.Service.claimAgentWrite).
func (s *Store) ClaimAgentWrite(w AgentWrite, since int64) (AgentWrite, bool, error) {
	w.State = AgentWriteSent
	if err := w.check(); err != nil {
		return AgentWrite{}, false, err
	}
	if w.Digest == "" {
		return AgentWrite{}, false, fmt.Errorf("%w: a claim needs its digest", ErrInvalidAgentWrite)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return AgentWrite{}, false, fmt.Errorf("store: claim agent write: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	earlier, err := scanAgentWrite(tx.QueryRow(agentWriteRepeatSQL, w.ThreadID, w.Digest))
	switch {
	case err == nil && earlier.Sent >= since:
		return earlier, false, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return AgentWrite{}, false, fmt.Errorf("store: claim agent write: %w", err)
	}
	if w.ID, err = insertAgentWriteTx(tx, w); err != nil {
		return AgentWrite{}, false, fmt.Errorf("store: claim agent write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AgentWrite{}, false, fmt.Errorf("store: claim agent write: commit: %w", err)
	}
	return w, true, nil
}

// ResendAgentWrite records that the write of row id, found not made, is
// sent again at sent: state sent, measured from the new time.
func (s *Store) ResendAgentWrite(id, sent int64) error {
	if sent <= 0 {
		return fmt.Errorf("%w: a resend needs its time", ErrInvalidAgentWrite)
	}
	if _, err := s.db.Exec(`UPDATE tracker_agent_writes SET state = 'sent', sent_ms = ?2, at_ms = ?2 WHERE id = `+intParam("?1"), id, sent); err != nil {
		return fmt.Errorf("store: resend agent write %d: %w", id, err)
	}
	return nil
}

// RecordAgentWrites replaces the row sent (0 for none) with the parts ws
// of the same write, in one transaction: a sent row whose part was refused
// or not sent goes with no row in its place.
func (s *Store) RecordAgentWrites(sent int64, ws []AgentWrite) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: record agent writes: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if sent != 0 {
		if _, err := tx.Exec(`DELETE FROM tracker_agent_writes WHERE id = `+intParam("?1"), sent); err != nil {
			return fmt.Errorf("store: record agent writes: %w", err)
		}
	}
	for _, w := range ws {
		if _, err := insertAgentWriteTx(tx, w); err != nil {
			return fmt.Errorf("store: record agent writes: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: record agent writes: commit: %w", err)
	}
	return nil
}

// SettleAgentWrite records that the write of row id, sent or of unknown
// outcome, was found made on the tracker, as comment when it made one, or
// as issue and its key when it was a create.
func (s *Store) SettleAgentWrite(id int64, issue, key, comment string) error {
	_, err := s.db.Exec(`UPDATE tracker_agent_writes SET state = 'applied', issue_id = iif(?2 <> '', ?2, issue_id),
		issue_key = iif(?3 <> '', ?3, issue_key), comment_id = iif(?4 <> '', ?4, comment_id) WHERE id = `+intParam("?1"), id, issue, key, comment)
	if err != nil {
		return fmt.Errorf("store: settle agent write %d: %w", id, err)
	}
	return nil
}

// agentWritesShown bounds the writes one ticket's marks read: its newest.
const agentWritesShown = 200

// AgentWriteMark is an applied or unknown write of an agent on a ticket,
// with the title of its thread, empty when the thread is gone.
type AgentWriteMark struct {
	AgentWrite
	ThreadTitle string
}

var agentWritesOfIssueSQL = `SELECT w.id, w.kind, w.site, w.issue_id, w.issue_key, w.project, w.account, w.thread_id, w.call_id, w.step, w.state,
	w.digest, w.comment_id, w.before_ms, w.after_ms, w.sent_ms, w.at_ms, COALESCE(t.title, '')
	FROM tracker_agent_writes w LEFT JOIN threads t ON t.id = w.thread_id
	WHERE w.kind = ?1 AND w.site = ?2 AND w.issue_id = ?3 AND w.state <> 'sent'
	ORDER BY w.id DESC LIMIT ` + strconv.Itoa(agentWritesShown)

// AgentWritesOf is the newest writes agents made on ticket, at most
// agentWritesShown, newest first, those still sent left out.
func (s *Store) AgentWritesOf(ticket TicketRef) ([]AgentWriteMark, error) {
	rows, err := s.reader().Query(agentWritesOfIssueSQL, ticket.Kind, ticket.Site, ticket.IssueID)
	if err != nil {
		return nil, fmt.Errorf("store: agent writes of %s: %w", ticket.IssueID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []AgentWriteMark
	for rows.Next() {
		var m AgentWriteMark
		if err := rows.Scan(&m.ID, &m.Ticket.Kind, &m.Ticket.Site, &m.Ticket.IssueID, &m.Key, &m.Project, &m.Account, &m.ThreadID, &m.CallID,
			&m.Step, &m.State, &m.Digest, &m.Comment, &m.Before, &m.After, &m.Sent, &m.At, &m.ThreadTitle); err != nil {
			return nil, fmt.Errorf("store: agent writes of %s: %w", ticket.IssueID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: agent writes of %s: %w", ticket.IssueID, err)
	}
	return out, nil
}

// agentWriteRepeatSQL is a thread's newest write with a digest.
const agentWriteRepeatSQL = `SELECT ` + agentWriteColumns + ` FROM tracker_agent_writes
	WHERE thread_id = ?1 AND digest = ?2 ORDER BY id DESC LIMIT 1`
