package store

// Migration v179 lets credit count the forge pull request history (v178)
// as review actions (tracker_cache_credit_forge.go):
//
//   - forge_history_sources gains acts_owed, whether the last read left
//     acts of a pull request it read unread (the walk's follow-up pages),
//     good_at, the wall time of the last read that landed without a
//     failure (0 before one), and viewer, the login of the account whose
//     token read it (empty until a read says).
//   - forge_history_uses: the repositories a tracker connection's credit
//     reads, one row per connection and source that a project listing a
//     board of the connection clones. A fork's parent is reached through
//     its origin's row (forge_history_sources.parent_key), so it has none
//     of its own. SetForgeHistorySources writes it with the sources.
//   - tracker_cache_forge_people: who a forge account is in a connection's
//     tracker, one row per mirror root, forge host and login: the name the
//     forge gave when the match was made, the tracker person (empty while
//     nobody matches) and when it was checked. Its one writer is the
//     linked tick's people unit (SetMirrorForgePeople).
//   - trg_tracker_cache_credit_key: credit finds a ticket's pull requests
//     by its key and tests what a source covers against its creation, so
//     a card write that changes either advances the totals revision of
//     the boards holding the issue, as every other input of the totals
//     does (the pace of a board, tracker_cache_pace.go).
//
// Every row is cache content: a lost use is written again by the next
// round, a lost match by the next tick.
const forgeCreditV179SQL = `
ALTER TABLE forge_history_sources ADD COLUMN acts_owed INTEGER NOT NULL DEFAULT 0 CHECK(acts_owed IN (0, 1));
ALTER TABLE forge_history_sources ADD COLUMN good_at INTEGER NOT NULL DEFAULT 0 CHECK(good_at >= 0);
ALTER TABLE forge_history_sources ADD COLUMN viewer TEXT NOT NULL DEFAULT '';
CREATE TABLE forge_history_uses (
 connection_id TEXT NOT NULL REFERENCES tracker_connections(id) ON DELETE CASCADE,
 source_id INTEGER NOT NULL REFERENCES forge_history_sources(id) ON DELETE CASCADE,
 PRIMARY KEY (connection_id, source_id)
) WITHOUT ROWID;
CREATE INDEX forge_history_uses_source ON forge_history_uses(source_id);
CREATE TABLE tracker_cache_forge_people (
 root_id INTEGER NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 host TEXT NOT NULL CHECK(host <> ''),
 login TEXT NOT NULL CHECK(login <> ''),
 name TEXT NOT NULL DEFAULT '',
 person TEXT NOT NULL DEFAULT '',
 checked_at INTEGER NOT NULL CHECK(checked_at > 0),
 PRIMARY KEY (root_id, host, login)
) WITHOUT ROWID;
CREATE TRIGGER trg_tracker_cache_credit_key AFTER UPDATE OF issue_key, created_at ON tracker_cache_issues
 WHEN OLD.issue_key IS NOT NEW.issue_key OR OLD.created_at IS NOT NEW.created_at BEGIN
 UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1 WHERE root_id = NEW.root_id
  AND board_id IN (SELECT board_id FROM tracker_cache_board_issues WHERE root_id = NEW.root_id AND issue_id = NEW.issue_id);
END;
`

const forgeCreditMigrationVersion = 179
