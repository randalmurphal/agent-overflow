package store

// Migration v148 lets a review draft span a range of lines and be a held
// reply to a PR thread (docs/specs/pull-requests.md):
//
//   - start_old_line and start_new_line are the first line of a range, in
//     the numbers old_line and new_line give its last; both 0 for one
//     line.
//   - reply_thread_id names the forge thread a held reply answers, and
//     reply_root_id that thread's first comment (GitHub posts a reply under
//     it). A reply is side 'reply', with no line, in pr scope; at most one
//     draft reply per thread for each owner and source.
//
// The side CHECK cannot widen in place, so the table is rebuilt with every
// column, index and the claim trigger as v144 and v145 left them. The
// trigger on items names this table, so it is dropped before the rebuild
// and created again after it.
const reviewRangesRepliesMigrationVersion = 148

const reviewRangesRepliesV148SQL = `DROP TRIGGER items_settle_diff_review_claims;

CREATE TABLE diff_review_comments_new (
	id              TEXT    PRIMARY KEY,
	thread_id       TEXT    REFERENCES threads(id) ON DELETE CASCADE,
	scope           TEXT    NOT NULL CHECK(scope IN ('turn', 'session', 'workspace', 'branch', 'pr')),
	source_key      TEXT    NOT NULL,
	commit_sha      TEXT    NOT NULL DEFAULT '',
	file_path       TEXT    NOT NULL,
	status          TEXT    NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'sent', 'resolved')),
	old_line        INTEGER NOT NULL DEFAULT 0 CHECK(old_line >= 0),
	new_line        INTEGER NOT NULL DEFAULT 0 CHECK(new_line >= 0),
	side            TEXT    NOT NULL CHECK(side IN ('file', 'old', 'new', 'context', 'reply')),
	selected_text   TEXT    NOT NULL DEFAULT '',
	body            TEXT    NOT NULL,
	sent_at         INTEGER NOT NULL DEFAULT 0,
	sent_turn_id    TEXT    NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	pr_key          TEXT,
	claim_thread_id TEXT    NOT NULL DEFAULT '',
	claim_item_id   TEXT    NOT NULL DEFAULT '',
	claim_settles   TEXT    NOT NULL DEFAULT '' CHECK (claim_settles IN ('', 'row', 'confirm')),
	start_old_line  INTEGER NOT NULL DEFAULT 0 CHECK(start_old_line >= 0),
	start_new_line  INTEGER NOT NULL DEFAULT 0 CHECK(start_new_line >= 0),
	reply_thread_id TEXT    NOT NULL DEFAULT '',
	reply_root_id   INTEGER NOT NULL DEFAULT 0 CHECK(reply_root_id >= 0),
	CHECK((thread_id IS NULL) <> (pr_key IS NULL)),
	CHECK(pr_key IS NULL OR (pr_key <> '' AND scope = 'pr')),
	CHECK((side = 'file' AND old_line = 0 AND new_line = 0)
	   OR (side = 'old' AND old_line > 0)
	   OR (side = 'new' AND new_line > 0)
	   OR (side = 'context' AND old_line > 0 AND new_line > 0)
	   OR (side = 'reply' AND old_line = 0 AND new_line = 0 AND scope = 'pr')),
	CHECK((side = 'reply') = (reply_thread_id <> '')),
	CHECK((start_old_line = 0 AND start_new_line = 0) OR side IN ('old', 'new', 'context'))
);

INSERT INTO diff_review_comments_new (
	id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at, pr_key,
	claim_thread_id, claim_item_id, claim_settles
)
SELECT
	id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at, pr_key,
	claim_thread_id, claim_item_id, claim_settles
FROM diff_review_comments;

DROP TABLE diff_review_comments;

ALTER TABLE diff_review_comments_new RENAME TO diff_review_comments;

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);

CREATE INDEX idx_diff_review_comments_pr
	ON diff_review_comments(pr_key, scope, source_key, status, file_path, old_line, new_line, created_at)
	WHERE pr_key IS NOT NULL;

CREATE INDEX idx_diff_review_comments_claim ON diff_review_comments(claim_thread_id, claim_item_id) WHERE claim_item_id <> '';

CREATE UNIQUE INDEX idx_diff_review_comments_held_reply
	ON diff_review_comments(COALESCE(thread_id, ''), COALESCE(pr_key, ''), scope, source_key, reply_thread_id)
	WHERE reply_thread_id <> '' AND status = 'draft';

CREATE TRIGGER items_settle_diff_review_claims
AFTER INSERT ON items
WHEN NEW.kind = 'user_text'
BEGIN
  UPDATE diff_review_comments
     SET claim_settles = ''
   WHERE claim_item_id <> '' AND claim_thread_id = NEW.thread_id AND claim_item_id = NEW.id AND claim_settles = 'row';
END;`
