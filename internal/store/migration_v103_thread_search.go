package store

// threadSearchV103SQL adds the full-text index the agent `thread_search` tool
// reads (docs/architecture/agent-thread-tools-plan.md, "v103: search index").
//
// The FTS5 table is contentless, so no message text is stored twice; the
// mapping table beside it says which thread, item and arm each FTS rowid
// stands for, and Go produces snippets from the source text because a
// contentless table cannot return one. `contentless_delete=1` is what lets a
// row be deleted and re-indexed by rowid without replaying its old text.
//
// `source` is `item` or `import`; `kind` is `user`, `assistant`, `tool` or
// `title`, and a title row has an empty `item_id`. Hidden-mode and scratch
// filtering happens at query time by joining threads, so promoting a scratch
// thread needs no reindex.
//
// The progress row is inserted here so a fresh install and an upgrade take the
// same path: the background build walks the existing corpus and deletes the
// row when it is done, and `thread_search` reports `indexing: true` for as
// long as it exists.
const threadSearchV103SQL = `
CREATE TABLE thread_search_rows (
  rowid INTEGER PRIMARY KEY,
  thread_id TEXT NOT NULL, item_id TEXT NOT NULL, source TEXT NOT NULL,
  kind TEXT NOT NULL,
  UNIQUE (thread_id, item_id, source),
  FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE);

CREATE VIRTUAL TABLE thread_search USING fts5(
  text, content='', contentless_delete=1,
  tokenize='unicode61 remove_diacritics 2');

CREATE TABLE thread_search_build (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  cursor_thread_id TEXT NOT NULL, cursor_item_id TEXT NOT NULL,
  imports_done INTEGER NOT NULL, titles_done INTEGER NOT NULL,
  started_at INTEGER NOT NULL);

INSERT INTO thread_search_build (id, cursor_thread_id, cursor_item_id, imports_done, titles_done, started_at)
VALUES (1, '', '', 0, 0, CAST(strftime('%s','now') AS INTEGER) * 1000);
`

// threadSearchResetSQL rebuilds the index from empty. RestoreFrom runs it
// because a restored history is a different corpus: the rows the live index
// described are gone, and re-deriving them costs one background pass.
const threadSearchResetSQL = `
DROP TABLE IF EXISTS thread_search_rows;
DROP TABLE IF EXISTS thread_search;
DROP TABLE IF EXISTS thread_search_build;
` + threadSearchV103SQL
