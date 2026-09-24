package store

import (
	"fmt"
	"slices"
)

// FoldUserTextRows rewrites one user_text row to the JOIN of itself and a set
// of earlier rows, and deletes those earlier rows, in ONE transaction.
//
// It exists for the Claude CLI's queue-boundary merge: several messages AO
// dispatched at separate drains can reach the provider as a single transcript
// entry carrying only the last message's uuid. AO folds its rows to match, so
// one AO message stays one transcript entry and a revert slices SQLite and the
// session file at the same place (internal/triage/claude_merge_fold.go).
//
// Atomic because a partial fold is a visibly wrong conversation: a summary
// rewritten without the deletes shows every folded message twice, and deletes
// without the rewrite lose the text outright. Message anchors for the deleted
// rows follow them through the items FK cascade.
//
// The survivor keeps its own (turn_index, item_index): the provider consumed
// the merged message where the LAST member was written, which is where that
// row already sits. Gaps left in item_index are ordering-irrelevant — nothing
// reads the values, only their order — so no renumber runs.
//
// Returns the survivor row as stored.
func (s *Store) FoldUserTextRows(threadID, survivorID string, foldedIDs []string, summary, meta string, updatedAt int64) (Item, error) {
	if threadID == "" || survivorID == "" {
		return Item{}, fmt.Errorf("store: fold user text rows: thread id and survivor id are required")
	}
	if slices.Contains(foldedIDs, survivorID) {
		return Item{}, fmt.Errorf("store: fold user text rows %s/%s: the survivor cannot also be folded away", threadID, survivorID)
	}
	if len(foldedIDs) == 0 {
		return Item{}, fmt.Errorf("store: fold user text rows %s/%s: no rows to fold", threadID, survivorID)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("store: begin fold user text rows %s/%s: %w", threadID, survivorID, err)
	}
	defer tx.Rollback()
	// The fold carries no card: it recomputes the chains of the rows it
	// rewrites and deletes before it commits.
	w := s.bulkItemWrites(tx, threadID, false)

	// Localize any imported row this touches BEFORE mutating, so an overlay
	// thread folds its own copies rather than the shared base.
	old, err := readMutableSubagentRowTx(tx, threadID, survivorID, "store: fold user text rows")
	if err != nil {
		return Item{}, err
	}
	for _, id := range foldedIDs {
		if err := requireMutableItemTx(tx, threadID, id, "store: fold user text rows"); err != nil {
			return Item{}, err
		}
	}

	row := old
	row.summary = summary
	row.setMeta(meta)
	if err := w.updated(old, row); err != nil {
		return Item{}, err
	}
	if _, err := tx.Exec(
		`UPDATE items SET summary = ?, meta = ?, updated_at = ? WHERE thread_id = ? AND id = ?`,
		summary, meta, updatedAt, threadID, survivorID,
	); err != nil {
		return Item{}, fmt.Errorf("store: fold survivor %s/%s: %w", threadID, survivorID, err)
	}
	for _, id := range foldedIDs {
		deleted, err := scanSubagentRow(tx.QueryRow(
			`DELETE FROM items WHERE thread_id = ? AND id = ? RETURNING `+subagentRowColumns(""),
			threadID, id,
		))
		if err != nil {
			return Item{}, fmt.Errorf("store: fold delete %s/%s: %w", threadID, id, err)
		}
		w.deleted(deleted)
	}
	if err := w.finish(); err != nil {
		return Item{}, err
	}

	survivor, err := readBackItemTx(tx, threadID, survivorID)
	if err != nil {
		return Item{}, fmt.Errorf("store: fold re-read %s/%s: %w", threadID, survivorID, err)
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("store: commit fold user text rows %s/%s: %w", threadID, survivorID, err)
	}
	return survivor, nil
}
