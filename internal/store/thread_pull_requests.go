package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Outcomes of a tracked pull request once the app has acted on it
// (thread_pull_requests.outcome). Empty means not handled yet.
const (
	// PROutcomeRemoved: it merged and its worktree was removed.
	PROutcomeRemoved = "removed"
	// PROutcomeKept: it merged and its worktree was kept for uncommitted
	// changes.
	PROutcomeKept = "kept"
	// PROutcomeGone: it merged and its worktree was already gone.
	PROutcomeGone = "gone"
	// PROutcomeClosed: it closed without merging, or the forge no longer
	// knows it. A closed pull request seen open again is tracked again.
	PROutcomeClosed = "closed"
	// PROutcomeSkipped: it merged and the worktree stays because the thread
	// left it, the user kept working after the merge, or the branch has a
	// new open pull request.
	PROutcomeSkipped = "skipped"
)

// ThreadPullRequest is the pull request a thread was last seen with OPEN
// (migration v142). MergedAt is unix milliseconds once the forge reported
// the merge, HandledAt once the app acted on it; both are zero until then.
// RemovalFailedAt is when the thread was told that removing its worktree
// failed, while the removal is retried; zero otherwise.
type ThreadPullRequest struct {
	ThreadID        string
	Forge           string
	Host            string
	Project         string
	Number          int
	URL             string
	NodeID          string
	Branch          string
	WorktreePath    string
	SeenOpenAt      int64
	MergedAt        int64
	HandledAt       int64
	RemovalFailedAt int64
	Outcome         string
}

const threadPullRequestColumns = `thread_id, forge, host, project, number, url, node_id, branch, worktree_path, seen_open_at, merged_at, handled_at, removal_failed_at, outcome`

func scanThreadPullRequest(scan func(...any) error) (ThreadPullRequest, error) {
	var pr ThreadPullRequest
	err := scan(&pr.ThreadID, &pr.Forge, &pr.Host, &pr.Project, &pr.Number, &pr.URL, &pr.NodeID,
		&pr.Branch, &pr.WorktreePath, &pr.SeenOpenAt, &pr.MergedAt, &pr.HandledAt, &pr.RemovalFailedAt, &pr.Outcome)
	return pr, err
}

// RecordOpenThreadPullRequest records pr as the pull request its thread was
// seen with OPEN and reports whether the row changed. A different pull
// request replaces the row and is tracked from the start. The same pull
// request seen again changes nothing once it is known merged, since a
// merged pull request cannot reopen and a lagging forge answer must not
// undo the merge; before that, its branch and worktree follow the latest
// sighting. One handled as closed is tracked again: it was reopened.
func (s *Store) RecordOpenThreadPullRequest(pr ThreadPullRequest) (bool, error) {
	if pr.ThreadID == "" || pr.Number <= 0 || pr.URL == "" || pr.Branch == "" || pr.WorktreePath == "" {
		return false, fmt.Errorf("store: record thread pull request: thread, number, url, branch and worktree are required")
	}
	result, err := s.db.Exec(`INSERT INTO thread_pull_requests(`+threadPullRequestColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, '')
		ON CONFLICT(thread_id) DO UPDATE SET
		  forge = excluded.forge, host = excluded.host, project = excluded.project,
		  number = excluded.number, url = excluded.url, node_id = excluded.node_id,
		  branch = excluded.branch, worktree_path = excluded.worktree_path,
		  seen_open_at = excluded.seen_open_at, merged_at = 0, handled_at = 0, removal_failed_at = 0, outcome = ''
		WHERE forge <> excluded.forge OR host <> excluded.host OR project <> excluded.project
		   OR number <> excluded.number OR outcome = 'closed'
		   OR (merged_at = 0 AND (url <> excluded.url OR node_id <> excluded.node_id
		       OR branch <> excluded.branch OR worktree_path <> excluded.worktree_path))`,
		pr.ThreadID, pr.Forge, pr.Host, pr.Project, pr.Number, pr.URL, pr.NodeID, pr.Branch, pr.WorktreePath, pr.SeenOpenAt)
	if err != nil {
		return false, fmt.Errorf("store: record thread %s pull request: %w", pr.ThreadID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: record thread %s pull request: %w", pr.ThreadID, err)
	}
	return n > 0, nil
}

// GetThreadPullRequest reads a thread's tracked pull request; false when it
// has none.
func (s *Store) GetThreadPullRequest(threadID string) (ThreadPullRequest, bool, error) {
	row := s.reader().QueryRow(`SELECT `+threadPullRequestColumns+` FROM thread_pull_requests WHERE thread_id = ?`, threadID)
	pr, err := scanThreadPullRequest(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadPullRequest{}, false, nil
	}
	if err != nil {
		return ThreadPullRequest{}, false, fmt.Errorf("store: read thread %s pull request: %w", threadID, err)
	}
	return pr, true, nil
}

// ListPendingThreadPullRequests lists every tracked pull request the app has
// not acted on yet, merged or not, in thread order.
func (s *Store) ListPendingThreadPullRequests() ([]ThreadPullRequest, error) {
	rows, err := s.reader().QueryContext(context.Background(), `SELECT `+threadPullRequestColumns+`
		FROM thread_pull_requests WHERE handled_at = 0 ORDER BY thread_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending thread pull requests: %w", err)
	}
	defer rows.Close()
	var out []ThreadPullRequest
	for rows.Next() {
		pr, err := scanThreadPullRequest(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan pending thread pull request: %w", err)
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pending thread pull requests: %w", err)
	}
	return out, nil
}

// MarkThreadPullRequestsMerged records mergedAt on every unhandled row that
// tracks the pull request (forge, host, project, number) and does not know
// the merge yet, and returns how many rows changed.
func (s *Store) MarkThreadPullRequestsMerged(forge, host, project string, number int, mergedAt int64) (int64, error) {
	if mergedAt <= 0 {
		return 0, fmt.Errorf("store: mark pull request merged: merge time is required")
	}
	result, err := s.db.Exec(`UPDATE thread_pull_requests SET merged_at = ?
		WHERE forge = ? AND host = ? AND project = ? AND number = ? AND handled_at = 0 AND merged_at = 0`,
		mergedAt, forge, host, project, number)
	if err != nil {
		return 0, fmt.Errorf("store: mark %s %s#%d merged: %w", forge, project, number, err)
	}
	return result.RowsAffected()
}

// MarkThreadPullRequestsRemovalFailed records, on every unhandled row that
// tracks the pull request (forge, host, project, number), that its threads
// were told removing the worktree failed, and reports whether this is the
// first time, so the notice is posted once however often the removal is
// retried.
func (s *Store) MarkThreadPullRequestsRemovalFailed(forge, host, project string, number int, at int64) (bool, error) {
	if at <= 0 {
		return false, fmt.Errorf("store: mark pull request removal failed: time is required")
	}
	result, err := s.db.Exec(`UPDATE thread_pull_requests SET removal_failed_at = ?
		WHERE forge = ? AND host = ? AND project = ? AND number = ? AND handled_at = 0 AND removal_failed_at = 0`,
		at, forge, host, project, number)
	if err != nil {
		return false, fmt.Errorf("store: mark %s %s#%d removal failed: %w", forge, project, number, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: mark %s %s#%d removal failed: %w", forge, project, number, err)
	}
	return n > 0, nil
}

// MarkThreadPullRequestsHandled records outcome on every unhandled row that
// tracks the pull request (forge, host, project, number), so the app acts
// on it once whichever of its threads it acted through, and returns how many
// rows changed.
func (s *Store) MarkThreadPullRequestsHandled(forge, host, project string, number int, outcome string, handledAt int64) (int64, error) {
	switch outcome {
	case PROutcomeRemoved, PROutcomeKept, PROutcomeGone, PROutcomeClosed, PROutcomeSkipped:
	default:
		return 0, fmt.Errorf("store: mark pull request handled: unknown outcome %q", outcome)
	}
	if handledAt <= 0 {
		return 0, fmt.Errorf("store: mark pull request handled: handled time is required")
	}
	result, err := s.db.Exec(`UPDATE thread_pull_requests SET handled_at = ?, outcome = ?
		WHERE forge = ? AND host = ? AND project = ? AND number = ? AND handled_at = 0`,
		handledAt, outcome, forge, host, project, number)
	if err != nil {
		return 0, fmt.Errorf("store: mark %s %s#%d %s: %w", forge, project, number, outcome, err)
	}
	return result.RowsAffected()
}
