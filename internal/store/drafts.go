package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ThreadDraft is the composer draft state for a single thread. Attachments
// and TerminalChips are opaque JSON blobs produced and consumed by the
// frontend. The store does not interpret them.
//
// PendingPlanImplementation is a JSON-encoded ProposedPlanSourceRef
// pointing at the proposed plan a draft is preparing to implement, or
// empty if the draft is not preparing an implementation. Column is
// nullable; "" round-trips as SQL NULL so the partial index in migration
// v31 stays selective and the sidebar visibility carve-out remains
// scoped to actual plan-implementation drafts.
type ThreadDraft struct {
	ThreadID                  string `json:"threadId"`
	Content                   string `json:"content"`
	Attachments               string `json:"attachments"`
	TerminalChips             string `json:"terminalChips"`
	PendingPlanImplementation string `json:"pendingPlanImplementation,omitempty"`
	UpdatedAt                 int64  `json:"updatedAt"`
}

// stored is the draft as UpsertThreadDraft writes it: an empty attachment or
// chip list is "[]".
func (d ThreadDraft) stored() ThreadDraft {
	if d.Attachments == "" {
		d.Attachments = "[]"
	}
	if d.TerminalChips == "" {
		d.TerminalChips = "[]"
	}
	return d
}

// SameDraft reports whether two drafts hold the same text, attachments,
// terminal snippets and plan link, the comparison a consuming delete makes
// (DeleteThreadDraft). A draft no row holds is the zero ThreadDraft.
func SameDraft(a, b ThreadDraft) bool {
	a, b = a.stored(), b.stored()
	return a.Content == b.Content && a.Attachments == b.Attachments &&
		a.TerminalChips == b.TerminalChips && a.PendingPlanImplementation == b.PendingPlanImplementation
}

// ErrDraftThreadMissing is a draft write for a thread row that no longer
// exists, such as an empty draft thread another request cleaned up. It wraps
// sql.ErrNoRows, which the transport answers as not_found.
var ErrDraftThreadMissing = fmt.Errorf("store: the draft's thread no longer exists: %w", sql.ErrNoRows)

// GetThreadDraft returns the draft for a thread, or (empty, false, nil) if no
// draft row exists yet.
func (s *Store) GetThreadDraft(threadID string) (ThreadDraft, bool, error) {
	return getThreadDraft(s.reader(), threadID)
}

func getThreadDraft(q sqlQueryer, threadID string) (ThreadDraft, bool, error) {
	row := q.QueryRow(
		`SELECT thread_id, content, attachments, terminal_chips, pending_plan_implementation, updated_at
		 FROM thread_drafts WHERE thread_id = ?`,
		threadID,
	)
	var d ThreadDraft
	var pendingPlan sql.NullString
	err := row.Scan(&d.ThreadID, &d.Content, &d.Attachments, &d.TerminalChips, &pendingPlan, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return ThreadDraft{ThreadID: threadID, Attachments: "[]", TerminalChips: "[]"}, false, nil
	}
	if err != nil {
		return ThreadDraft{}, false, fmt.Errorf("store: get thread draft %s: %w", threadID, err)
	}
	if pendingPlan.Valid {
		d.PendingPlanImplementation = pendingPlan.String
	}
	return d, true, nil
}

// DraftWrite is what a draft write moved.
type DraftWrite struct {
	// Changed reports that the stored draft differs from before.
	Changed bool
	// Listing reports whether the write moved the thread across the sidebar
	// listing rule: a thread with nothing else to list it is listed while its
	// draft has content. A screen whose catalog read came before the write
	// learns of the change only from the announcement this calls for.
	Listing ListingChange
}

// UpsertThreadDraft writes the draft for a thread, replacing any existing row,
// and reports whether the write actually moved it and whether it listed the
// thread. Callers should pre-encode Attachments and TerminalChips as JSON
// arrays.
//
// The changed flag exists because every persisted draft write is broadcast on
// `draft:updated`, and the composer AUTOSAVES: a save fired on a buffer nobody
// touched must not wake every attached client. `updated_at` is deliberately
// left out of the change test — it is a fresh timestamp on every call, so
// including it would make every write look like a change and defeat the point.
// The consequence is that a no-op save leaves the stored timestamp where it
// was, which is the honest answer (nothing was edited) and which nothing
// reads: the draft's updated_at is not rendered anywhere.
func (s *Store) UpsertThreadDraft(d ThreadDraft) (DraftWrite, error) {
	tx, err := s.beginListingWrite()
	if err != nil {
		return DraftWrite{}, fmt.Errorf("store: upsert thread draft %s: %w", d.ThreadID, err)
	}
	defer func() { _ = tx.Rollback() }()
	write, err := upsertThreadDraft(tx, d)
	if err != nil {
		return DraftWrite{}, err
	}
	return write, tx.Commit()
}

// upsertThreadDraft runs inside the caller's transaction, so the listing
// read before the write and the one after it see no other write between.
func upsertThreadDraft(q sqlQueryer, d ThreadDraft) (DraftWrite, error) {
	if d.ThreadID == "" {
		return DraftWrite{}, fmt.Errorf("store: upsert draft: thread id is required")
	}
	// Normalise nullable JSON fields so SELECTs always return valid JSON.
	if d.Attachments == "" {
		d.Attachments = "[]"
	}
	if d.TerminalChips == "" {
		d.TerminalChips = "[]"
	}
	var pendingPlan any
	if d.PendingPlanImplementation != "" {
		pendingPlan = d.PendingPlanImplementation
	}
	hasContent := 0
	if strings.TrimSpace(d.Content) != "" ||
		(d.Attachments != "" && d.Attachments != "[]" && d.Attachments != "null") ||
		(d.TerminalChips != "" && d.TerminalChips != "[]" && d.TerminalChips != "null") ||
		d.PendingPlanImplementation != "" {
		hasContent = 1
	}
	listedBefore, err := threadListed(q, d.ThreadID)
	if err != nil {
		return DraftWrite{}, err
	}
	// `IS NOT` rather than `<>` on every term: pending_plan_implementation is
	// nullable and `NULL <> 'x'` is NULL, which SQLite reads as false — a `<>`
	// predicate would report a plan link appearing or disappearing as a no-op.
	var written string
	err = q.QueryRow(
		`INSERT INTO thread_drafts (thread_id, content, attachments, terminal_chips, pending_plan_implementation, updated_at, has_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
			content = excluded.content,
			attachments = excluded.attachments,
			terminal_chips = excluded.terminal_chips,
			pending_plan_implementation = excluded.pending_plan_implementation,
			updated_at = excluded.updated_at,
			has_content = excluded.has_content
		 WHERE thread_drafts.content IS NOT excluded.content
		    OR thread_drafts.attachments IS NOT excluded.attachments
		    OR thread_drafts.terminal_chips IS NOT excluded.terminal_chips
		    OR thread_drafts.pending_plan_implementation IS NOT excluded.pending_plan_implementation
		 RETURNING thread_id`,
		d.ThreadID, d.Content, d.Attachments, d.TerminalChips, pendingPlan, d.UpdatedAt, hasContent,
	).Scan(&written)
	if errors.Is(err, sql.ErrNoRows) {
		// The row already held exactly this draft. A normal outcome, not an
		// error: the composer autosaves whether or not anything was typed.
		return DraftWrite{}, nil
	}
	var engine *sqlite.Error
	if errors.As(err, &engine) && engine.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		// thread_id is the table's only foreign key.
		return DraftWrite{}, fmt.Errorf("store: upsert thread draft %s: %w", d.ThreadID, ErrDraftThreadMissing)
	}
	if err != nil {
		return DraftWrite{}, fmt.Errorf("store: upsert thread draft %s: %w", d.ThreadID, err)
	}
	listedAfter, err := threadListed(q, d.ThreadID)
	if err != nil {
		return DraftWrite{}, err
	}
	return DraftWrite{Changed: true, Listing: listingChange(listedBefore, listedAfter)}, nil
}

// DeleteThreadDraft removes the draft for a thread and reports whether a row
// was there to remove (Changed) and whether that unlisted the thread. Missing rows are not an error — clearing something that
// was never there is a no-op, and it is one nothing needs to hear about.
// A non-nil expected snapshot is compared atomically with deletion, so another
// client saving while a send is in flight cannot lose its newer draft.
func (s *Store) DeleteThreadDraft(threadID string, expected *ThreadDraft) (DraftWrite, error) {
	query := `DELETE FROM thread_drafts WHERE thread_id = ?`
	args := []any{threadID}
	if expected != nil {
		e := expected.stored()
		var plan any
		if e.PendingPlanImplementation != "" {
			plan = e.PendingPlanImplementation
		}
		query += ` AND content = ? AND attachments = ? AND terminal_chips = ? AND pending_plan_implementation IS ?`
		args = append(args, e.Content, e.Attachments, e.TerminalChips, plan)
	}

	tx, err := s.beginListingWrite()
	if err != nil {
		return DraftWrite{}, fmt.Errorf("store: delete thread draft %s: %w", threadID, err)
	}
	defer func() { _ = tx.Rollback() }()
	listedBefore, err := threadListed(tx, threadID)
	if err != nil {
		return DraftWrite{}, err
	}
	var deleted string
	err = tx.QueryRow(
		query+` RETURNING thread_id`, args...,
	).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return DraftWrite{}, tx.Commit()
	}
	if err != nil {
		return DraftWrite{}, fmt.Errorf("store: delete thread draft %s: %w", threadID, err)
	}
	listedAfter, err := threadListed(tx, threadID)
	if err != nil {
		return DraftWrite{}, err
	}
	write := DraftWrite{Changed: true, Listing: listingChange(listedBefore, listedAfter)}
	if err := tx.Commit(); err != nil {
		return DraftWrite{}, fmt.Errorf("store: commit delete thread draft %s: %w", threadID, err)
	}
	return write, nil
}
