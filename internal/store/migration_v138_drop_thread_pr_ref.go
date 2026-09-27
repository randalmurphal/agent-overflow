package store

// Migration v138 drops threads.pr_ref. Only threads seeded from a pull
// request URL wrote it, and that entry point is gone: a review pane finds a
// thread's pull request through its workspace's git status.
const dropThreadPRRefMigrationVersion = 138

const dropThreadPRRefV138SQL = `ALTER TABLE threads DROP COLUMN pr_ref`
