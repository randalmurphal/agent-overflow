package store

// Migration v142 records the pull request each thread was last seen with
// OPEN, so the app can remove the thread's worktree once that pull request
// merges, across restarts. One row per thread: a later open pull request
// replaces it. v138 dropped threads.pr_ref because a review pane finds a
// thread's pull request through git status; this row is the durable fact
// that the pull request was seen open from this thread, which the status
// cache does not keep.
const threadPullRequestsMigrationVersion = 142

const threadPullRequestsV142SQL = `CREATE TABLE thread_pull_requests (
 thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
 forge TEXT NOT NULL CHECK(forge IN ('github', 'gitlab')),
 host TEXT NOT NULL,
 project TEXT NOT NULL,
 number INTEGER NOT NULL CHECK(number > 0),
 url TEXT NOT NULL,
 node_id TEXT NOT NULL DEFAULT '',
 branch TEXT NOT NULL,
 worktree_path TEXT NOT NULL,
 seen_open_at INTEGER NOT NULL,
 merged_at INTEGER NOT NULL DEFAULT 0,
 handled_at INTEGER NOT NULL DEFAULT 0,
 removal_failed_at INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('', 'removed', 'kept', 'gone', 'closed', 'skipped'))
);
CREATE INDEX idx_thread_pull_requests_pending ON thread_pull_requests(forge, host, project) WHERE handled_at = 0;`
