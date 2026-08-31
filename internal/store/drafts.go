package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// GetThreadDraft returns the draft for a thread, or (empty, false, nil) if no
// draft row exists yet.
func (s *Store) GetThreadDraft(threadID string) (ThreadDraft, bool, error) {
	row := s.reader().QueryRow(
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

// UpsertThreadDraft writes the draft for a thread, replacing any existing row,
// and reports whether the write actually moved it. Callers should pre-encode
// Attachments and TerminalChips as JSON arrays.
//
// The changed flag exists because every persisted draft write is broadcast on
// `draft:updated`, and the composer AUTOSAVES: a save fired on a buffer nobody
// touched must not wake every attached client. `updated_at` is deliberately
// left out of the change test — it is a fresh timestamp on every call, so
// including it would make every write look like a change and defeat the point.
// The consequence is that a no-op save leaves the stored timestamp where it
// was, which is the honest answer (nothing was edited) and which nothing
// reads: the draft's updated_at is not rendered anywhere.
func (s *Store) UpsertThreadDraft(d ThreadDraft) (bool, error) {
	if d.ThreadID == "" {
		return false, fmt.Errorf("store: upsert draft: thread id is required")
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
	// `IS NOT` rather than `<>` on every term: pending_plan_implementation is
	// nullable and `NULL <> 'x'` is NULL, which SQLite reads as false — a `<>`
	// predicate would report a plan link appearing or disappearing as a no-op.
	var written string
	err := s.db.QueryRow(
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
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: upsert thread draft %s: %w", d.ThreadID, err)
	}
	return true, nil
}

// DeleteThreadDraft removes the draft for a thread and reports whether a row
// was there to remove. Missing rows are not an error — clearing something that
// was never there is a no-op, and it is one nothing needs to hear about.
func (s *Store) DeleteThreadDraft(threadID string) (bool, error) {
	var deleted string
	err := s.db.QueryRow(
		`DELETE FROM thread_drafts WHERE thread_id = ? RETURNING thread_id`, threadID,
	).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: delete thread draft %s: %w", threadID, err)
	}
	return true, nil
}
