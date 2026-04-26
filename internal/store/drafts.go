package store

import (
	"database/sql"
	"fmt"
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
	row := s.db.QueryRow(
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

// UpsertThreadDraft writes the draft for a thread, replacing any existing row.
// Callers should pre-encode Attachments and TerminalChips as JSON arrays.
func (s *Store) UpsertThreadDraft(d ThreadDraft) error {
	if d.ThreadID == "" {
		return fmt.Errorf("store: upsert draft: thread id is required")
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
	_, err := s.db.Exec(
		`INSERT INTO thread_drafts (thread_id, content, attachments, terminal_chips, pending_plan_implementation, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
			content = excluded.content,
			attachments = excluded.attachments,
			terminal_chips = excluded.terminal_chips,
			pending_plan_implementation = excluded.pending_plan_implementation,
			updated_at = excluded.updated_at`,
		d.ThreadID, d.Content, d.Attachments, d.TerminalChips, pendingPlan, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: upsert thread draft %s: %w", d.ThreadID, err)
	}
	return nil
}

// DeleteThreadDraft removes the draft for a thread. Missing rows are not an
// error — clearing something that was never there is a no-op.
func (s *Store) DeleteThreadDraft(threadID string) error {
	_, err := s.db.Exec(`DELETE FROM thread_drafts WHERE thread_id = ?`, threadID)
	if err != nil {
		return fmt.Errorf("store: delete thread draft %s: %w", threadID, err)
	}
	return nil
}
