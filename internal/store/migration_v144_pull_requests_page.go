package store

// Migration v144 backs the Pull requests page (docs/specs/pull-requests.md):
//
//   - pr_views records, per PR key (git.PRReference.Key), when the app last
//     opened the PR and the head, comment count and commit count it showed
//     then, so the list can say what changed since. The counts are null
//     when the list had not read them.
//   - diff_review_comments lets a draft belong to a PR instead of a thread:
//     the page reviews a PR with no thread. thread_id becomes nullable and
//     pr_key is added, with exactly one of the two set. SQLite cannot relax
//     a NOT NULL in place, so the table is rebuilt with every column and
//     its scope index as v18 left them. No table references it and no
//     trigger names it, so the rebuild has nothing else to restate.
//   - thread_pull_requests gains an index on the PR it tracks, for the
//     page's "threads on this PR" lookup.
const pullRequestsPageMigrationVersion = 144

const pullRequestsPageV144SQL = `CREATE TABLE pr_views (
 pr_key TEXT PRIMARY KEY CHECK(pr_key <> ''),
 opened_at INTEGER NOT NULL,
 head_sha TEXT NOT NULL,
 comments INTEGER CHECK(comments IS NULL OR comments >= 0),
 commits INTEGER CHECK(commits IS NULL OR commits >= 0)
);

CREATE TABLE diff_review_comments_new (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    REFERENCES threads(id) ON DELETE CASCADE,
	scope         TEXT    NOT NULL CHECK(scope IN ('turn', 'session', 'workspace', 'branch', 'pr')),
	source_key    TEXT    NOT NULL,
	commit_sha    TEXT    NOT NULL DEFAULT '',
	file_path     TEXT    NOT NULL,
	status        TEXT    NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'sent', 'resolved')),
	old_line      INTEGER NOT NULL DEFAULT 0 CHECK(old_line >= 0),
	new_line      INTEGER NOT NULL DEFAULT 0 CHECK(new_line >= 0),
	side          TEXT    NOT NULL CHECK(side IN ('file', 'old', 'new', 'context')),
	selected_text TEXT    NOT NULL DEFAULT '',
	body          TEXT    NOT NULL,
	sent_at       INTEGER NOT NULL DEFAULT 0,
	sent_turn_id  TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	pr_key        TEXT,
	CHECK((thread_id IS NULL) <> (pr_key IS NULL)),
	CHECK(pr_key IS NULL OR (pr_key <> '' AND scope = 'pr')),
	CHECK((side = 'file' AND old_line = 0 AND new_line = 0)
	   OR (side = 'old' AND old_line > 0)
	   OR (side = 'new' AND new_line > 0)
	   OR (side = 'context' AND old_line > 0 AND new_line > 0))
);

INSERT INTO diff_review_comments_new (
	id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at, pr_key
)
SELECT
	id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at, NULL
FROM diff_review_comments;

DROP TABLE diff_review_comments;

ALTER TABLE diff_review_comments_new RENAME TO diff_review_comments;

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);

CREATE INDEX idx_diff_review_comments_pr
	ON diff_review_comments(pr_key, scope, source_key, status, file_path, old_line, new_line, created_at)
	WHERE pr_key IS NOT NULL;

CREATE INDEX idx_thread_pull_requests_pr ON thread_pull_requests(forge, host, project, number);`
