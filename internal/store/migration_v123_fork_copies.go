package store

// threadForkCopiedV123SQL records the rows a pointer fork copied from its
// ancestors in a materialization that has not finished
// (MaterializeForkHistory), each with source_id, the ancestor that held
// the row. Each batch commits its copies with their rows here; the
// transaction that drops the fork's lineage removes them. A delete of an
// ancestor rolls back the copies of the rows its detach stops the fork
// reading (DeleteThreadPaced), so a fork never keeps part of a deleted
// thread's history, whether its export is running, was stopped or was
// interrupted by a crash. A write of the fork to a copy or under one
// removes the record (settleForkCopiesTx), so the delete keeps what the
// fork wrote. A row leaves with its item.
const threadForkCopiedV123SQL = `
CREATE TABLE thread_fork_copied (
    thread_id TEXT NOT NULL,
    item_id   TEXT NOT NULL,
    source_id TEXT NOT NULL,
    PRIMARY KEY (thread_id, item_id),
    FOREIGN KEY (thread_id, item_id) REFERENCES items(thread_id, id) ON DELETE CASCADE ON UPDATE CASCADE
) WITHOUT ROWID;
CREATE INDEX idx_thread_fork_copied_source ON thread_fork_copied(thread_id, source_id);
`
