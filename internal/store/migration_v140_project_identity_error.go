package store

// Migration v140 adds projects.identity_error: why the last read of the
// checkout's repository identity failed, "" after a successful read. A failed
// read was previously stored as an empty identity, indistinguishable from a
// folder that is not a repository. A plain ADD COLUMN, so the FK-parent
// `projects` table is not rebuilt.
const projectIdentityErrorMigrationVersion = 140

const projectIdentityErrorV140SQL = `ALTER TABLE projects ADD COLUMN identity_error TEXT NOT NULL DEFAULT ''`
