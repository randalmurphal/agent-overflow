package store

// Migration v146 changes what the Pull requests page keeps:
//
//   - pr_views goes: the page no longer records which PRs the app opened.
//     A PR's attention comes from the forge's own record of the viewer's
//     activity (docs/decisions.md).
//   - pr_list_projects keeps each repository's last project read, the
//     ProjectPRList the list pump published (JSON, its rows' attention
//     computed), and when the read landed (unix milliseconds), so the
//     page and the sidebar badge answer from disk before any forge read.
//     It is a cache the next read replaces.
const prListSnapshotMigrationVersion = 146

const prListSnapshotV146SQL = `DROP TABLE pr_views;

CREATE TABLE pr_list_projects (
 repo_key TEXT PRIMARY KEY CHECK(repo_key <> ''),
 project_json BLOB NOT NULL,
 read_at INTEGER NOT NULL
);`
