package store

// Rebuild DDL for the diff_review_comments table (accessors in
// diff_review_comments.go).
//
// The chain driver, the derivation helpers, and the migrations slice are in
// migrate.go.

const rebuildDiffReviewCommentsV16SQL = `
CREATE TABLE diff_review_comments_new (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	scope         TEXT    NOT NULL CHECK(scope IN ('turn', 'session', 'workspace', 'branch')),
	source_key    TEXT    NOT NULL,
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
	CHECK((side = 'file' AND old_line = 0 AND new_line = 0)
	   OR (side = 'old' AND old_line > 0)
	   OR (side = 'new' AND new_line > 0)
	   OR (side = 'context' AND old_line > 0 AND new_line > 0))
);

INSERT INTO diff_review_comments_new (
	id, thread_id, scope, source_key, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
)
SELECT
	id, thread_id, scope, source_key, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
FROM diff_review_comments;

DROP TABLE diff_review_comments;

ALTER TABLE diff_review_comments_new RENAME TO diff_review_comments;

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);
`

const rebuildDiffReviewCommentsV18SQL = `
CREATE TABLE diff_review_comments_new (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
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
	CHECK((side = 'file' AND old_line = 0 AND new_line = 0)
	   OR (side = 'old' AND old_line > 0)
	   OR (side = 'new' AND new_line > 0)
	   OR (side = 'context' AND old_line > 0 AND new_line > 0))
);

INSERT INTO diff_review_comments_new (
	id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
)
SELECT
	id, thread_id, scope, source_key, '', file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
FROM diff_review_comments;

DROP TABLE diff_review_comments;

ALTER TABLE diff_review_comments_new RENAME TO diff_review_comments;

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);
`
