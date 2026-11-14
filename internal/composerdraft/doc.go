// Package composerdraft builds the composer-draft row a thread should
// be restored to when text that already exists elsewhere is put back
// into the composer: fork-from-message (app_thread_fork.go), the
// flush-queue restore paths (app_flush_queue_restore.go), and the
// edit-and-resend saga's crash copy (app_revert_and_resend.go), which
// merges the edited text ahead of the composer's own work-in-progress.
// Pure helpers — App-bound glue stays in `app_draft.go`.
package composerdraft
