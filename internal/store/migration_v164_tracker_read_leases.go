package store

// Migration v164 adds tracker_cache_read_leases: per mirror root, a time
// later than every read the root's clock began (BeginMirrorRead), so the
// first read a process begins of the root, after a restart or a crash,
// begins after every read the root stored. The table is of the mirror: it
// cascades from its root and a restore empties it.
//
// The table is new: no row is rewritten.
const trackerReadLeasesMigrationVersion = 164

const trackerReadLeasesV164SQL = `
CREATE TABLE tracker_cache_read_leases (
 root_id INTEGER PRIMARY KEY NOT NULL REFERENCES tracker_cache_roots(id) ON DELETE CASCADE,
 lease INTEGER NOT NULL CHECK(lease > 0)
);
`
