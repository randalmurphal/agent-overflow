package store

// Migration v137 orders a thread's import chunk references newest first.
// An ordered imported arm (timelineArms) sorts its rows, and once its
// sorter holds the limit it reads each later chunk only up to its first
// selected row; that holds only when the arm visits the chunks in the
// read's order. SQLite walks an index backwards only for an ORDER BY the
// index itself satisfies, so a limited newest-first read walks this index
// (chunkRefsIndex):
// through idx_thread_import_chunks_turns it would visit the chunks oldest
// first and sort every imported row of the thread.
const newestChunkRefsIndexMigrationVersion = 137

const newestChunkRefsIndexV137SQL = `CREATE INDEX idx_thread_import_chunks_newest
    ON thread_import_chunks(thread_id, max_turn_index DESC, min_turn_index, chunk_id)`
