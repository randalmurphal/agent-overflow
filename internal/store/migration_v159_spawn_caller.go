package store

// Migration v159 indexes the local spawn links by caller
// (idx_thread_requests_spawn_caller), over the rows of
// idx_thread_requests_spawn_link: a read of rest walks from a thread to the
// threads its spawns reached, at any depth, settled or not (ReadSubtreeRest),
// and the owned walks of an archive and a delete step the same way
// (ownedStepSQL), each probing a thread's spawn links without reading its
// sends, asks and reminders.
const spawnCallerMigrationVersion = 159

const spawnCallerV159SQL = `
CREATE INDEX idx_thread_requests_spawn_caller
    ON thread_requests(caller_thread_id)
 WHERE kind = 'spawn' AND target_computer_id = '' AND superseded = 0 AND target_thread_id <> '';
`
