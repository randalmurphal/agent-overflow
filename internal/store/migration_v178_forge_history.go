package store

// Migration v178 adds the pull request history of the forge repositories
// the tracker connections' projects clone, which a ticket's Work lists
// (ApplyForgeHistory, ForgeHistoryByKey):
//
//   - forge_history_sources: one row per read of a repository. keep_head
//     is empty for a project's origin, whose every pull request is kept,
//     and names the fork for a fork's parent, read for the pull requests
//     whose head repository is that fork. It holds the reader's walk
//     (walk, JSON only the reader reads), how far back every update is
//     read (reach, the forge's Unix ms; 0 is back to the window's start;
//     NULL until a read lands), whether the first full read is done
//     (walked), the repository's own head identity in the forge's terms
//     (own_head), the parent the forge names when it is a fork, the last
//     read's wall time (read_at) and its failure (failure_kind, failure,
//     failed_at), which a read that lands clears.
//   - forge_history_prs: one row per pull request updated inside the
//     window: its number, address, title, source branch and head
//     repository, state, author and times. A parent's pull request of
//     another head is kept with its head and times only (kept 0), so a
//     later event naming it is known and not fetched again; no read
//     returns it.
//   - forge_history_acts: one row per pull request, kind (1 approved, 2
//     commented) and login, with the person's name and the earliest time.
//   - forge_history_keys: the ticket keys each kept pull request names,
//     by its title and source branch (trackerapp.TicketKeys).
//
// Acts and keys belong to their pull request and pull requests to their
// source (ON DELETE CASCADE). Every row is cache content: a lost one costs
// a forge read.
const forgeHistoryV178SQL = `
CREATE TABLE forge_history_sources (
 id INTEGER PRIMARY KEY,
 repo_key TEXT NOT NULL CHECK(repo_key <> ''),
 keep_head TEXT NOT NULL DEFAULT '',
 forge TEXT NOT NULL CHECK(forge IN ('github', 'gitlab')),
 host TEXT NOT NULL CHECK(host <> ''),
 namespace TEXT NOT NULL CHECK(namespace <> ''),
 repo TEXT NOT NULL CHECK(repo <> ''),
 own_head TEXT NOT NULL DEFAULT '',
 parent_namespace TEXT NOT NULL DEFAULT '',
 parent_repo TEXT NOT NULL DEFAULT '',
 walk TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(walk)),
 reach INTEGER CHECK(reach >= 0),
 walked INTEGER NOT NULL DEFAULT 0 CHECK(walked IN (0, 1)),
 read_at INTEGER NOT NULL DEFAULT 0 CHECK(read_at >= 0),
 failure_kind TEXT NOT NULL DEFAULT '',
 failure TEXT NOT NULL DEFAULT '',
 failed_at INTEGER NOT NULL DEFAULT 0 CHECK(failed_at >= 0),
 CHECK((failure_kind = '') = (failure = '') AND (failure = '') = (failed_at = 0)),
 CHECK((parent_namespace = '') = (parent_repo = '')),
 CHECK(walked = 0 OR reach IS NOT NULL),
 UNIQUE(repo_key, keep_head)
);
CREATE TABLE forge_history_prs (
 source_id INTEGER NOT NULL,
 number INTEGER NOT NULL CHECK(number > 0),
 kept INTEGER NOT NULL CHECK(kept IN (0, 1)),
 url TEXT NOT NULL,
 title TEXT NOT NULL,
 head_ref TEXT NOT NULL,
 head_repo TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('open', 'draft', 'merged', 'closed')),
 author TEXT NOT NULL,
 author_name TEXT NOT NULL,
 created_at INTEGER NOT NULL CHECK(created_at >= 0),
 updated_at INTEGER NOT NULL CHECK(updated_at > 0),
 merged_at INTEGER NOT NULL CHECK(merged_at >= 0),
 closed_at INTEGER NOT NULL CHECK(closed_at >= 0),
 CHECK(kept = 1 AND url <> '' OR kept = 0 AND url = '' AND title = '' AND author = '' AND author_name = ''),
 PRIMARY KEY (source_id, number),
 FOREIGN KEY (source_id) REFERENCES forge_history_sources(id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX forge_history_prs_updated ON forge_history_prs(source_id, updated_at);
CREATE TABLE forge_history_acts (
 source_id INTEGER NOT NULL,
 number INTEGER NOT NULL,
 kind INTEGER NOT NULL CHECK(kind IN (1, 2)),
 login TEXT NOT NULL CHECK(login <> ''),
 name TEXT NOT NULL,
 at INTEGER NOT NULL CHECK(at > 0),
 PRIMARY KEY (source_id, number, kind, login),
 FOREIGN KEY (source_id, number) REFERENCES forge_history_prs(source_id, number) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE forge_history_keys (
 key TEXT NOT NULL CHECK(key <> ''),
 source_id INTEGER NOT NULL,
 number INTEGER NOT NULL,
 PRIMARY KEY (key, source_id, number),
 FOREIGN KEY (source_id, number) REFERENCES forge_history_prs(source_id, number) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX forge_history_keys_pr ON forge_history_keys(source_id, number);
`

const forgeHistoryMigrationVersion = 178
