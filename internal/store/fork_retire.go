package store

import (
	"database/sql"
	"fmt"

	"agent-overflow/internal/threadmode"
)

// A deleted thread its pointer forks still read becomes a holder in place
// (docs/architecture/sqlite-store.md#pointer-forks): the rows the forks read
// stay where they are, so the delete costs the forks nothing whatever their
// number, and everything else the thread had goes as its delete would have
// removed it.

// holderKeptTables are the tables with a foreign key to threads whose rows
// a holder keeps: the history its readers read, the hides they read it
// through, the index rows search expands to them, and the attachments
// their rows show (ReleasableAttachments).
var holderKeptTables = []string{
	"attachment_owners",
	"items",
	"payloads",
	"thread_fork_hidden",
	"thread_import_chunks",
	"thread_import_item_overrides",
	"thread_search_rows",
	"turns",
}

// holderClearedRows are the other rows with a foreign key to threads. A
// retired thread loses them as its delete would have. thread_fork_lineage
// is the thread's own reads: its readers hold levels of their own for
// every thread it read.
var holderClearedRows = []struct{ table, column string }{
	{"agent_end_backfill", "thread_id"},
	{"async_questions", "thread_id"},
	{"channels", "thread_id"},
	{"diff_review_comments", "thread_id"},
	{"flush_queue_items", "thread_id"},
	{"message_anchors", "thread_id"},
	{"pending_background_task_terminals", "thread_id"},
	{"proposed_plan_comments", "thread_id"},
	{"proposed_plans", "thread_id"},
	{"provider_thread_cost", "thread_id"},
	{"scratch_threads", "thread_id"},
	{"subagent_aggregate_backfill", "thread_id"},
	{"thread_draft_recoveries", "thread_id"},
	{"thread_drafts", "thread_id"},
	{"thread_fork_lineage", "thread_id"},
	{"thread_import_state", "thread_id"},
	{"thread_request_receipts", "target_thread_id"},
	{"thread_requests", "caller_thread_id"},
}

// retireToHolderTx makes id a holder when a fork reads it, in place of
// deleting its row, and reports whether it did. The caller has removed the
// rows past the forks' last cut; the turn rows past it go here. The
// holder keeps its title and history and nothing else: no project, so no
// project delete waits for it, no workspace, session, pin, group or
// parent, and the links other threads had to it are cleared as the
// delete would have cleared them (ON DELETE SET NULL). It is hidden from
// every listing and read (threadmode.ModeHolder, owned_threads). When its
// last reader goes, trg_thread_fork_lineage_release marks it deleting.
func retireToHolderTx(tx *sql.Tx, id string) (bool, error) {
	cut, read, err := maxReaderCutTx(tx, id)
	if err != nil || !read {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM turns WHERE thread_id = ? AND turn_index >= ?`, id, cut.turn); err != nil {
		return false, fmt.Errorf("store: drop the turns no fork of %s reads: %w", id, err)
	}
	for _, rows := range holderClearedRows {
		if _, err := tx.Exec(`DELETE FROM `+rows.table+` WHERE `+rows.column+` = ?`, id); err != nil {
			return false, fmt.Errorf("store: clear %s of retired thread %s: %w", rows.table, id, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, id); err != nil {
		return false, fmt.Errorf("store: clear the cards of retired thread %s: %w", id, err)
	}
	if err := deleteThreadSearchWhereTx(tx, "thread_id = ? AND item_id = ''", []any{id}); err != nil {
		return false, err
	}
	for _, column := range []string{"forked_from_thread_id", "parent_thread_id"} {
		if _, err := tx.Exec(`UPDATE threads SET `+column+` = NULL WHERE `+column+` = ?`, id); err != nil {
			return false, fmt.Errorf("store: clear the %s links to retired thread %s: %w", column, id, err)
		}
	}
	result, err := tx.Exec(`UPDATE threads
		    SET mode = ?, deleting = 0, project_id = NULL, workspace_path = '', worktree_path = NULL,
		        branch = NULL, pr_ref = '', session_ref = NULL, pending_fork_session_ref = NULL,
		        pending_fork_resume_at = '', fork_preparing = 0, discussion_id = NULL,
		        parent_thread_id = NULL, forked_from_thread_id = NULL, pinned_at = NULL,
		        pin_group = NULL, group_id = NULL, archived = 0, live_todo = '',
		        worktree_setup_state = ''
		  WHERE id = ?`, threadmode.ModeHolder, id)
	if err != nil {
		return false, fmt.Errorf("store: retire thread %s to a holder: %w", id, err)
	}
	if err := requireRowsAffected(result, "store: retire thread "+id+" to a holder"); err != nil {
		return false, err
	}
	return true, nil
}
