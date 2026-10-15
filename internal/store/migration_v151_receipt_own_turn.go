package store

// Migration v151 records whether the turn a request's message was delivered
// into opened for that message (docs/specs/agent-thread-tools.md, "Cancel").
// A cancel interrupts the turn only when it did: a message drained into a
// turn already running joined a turn someone else started.
//
// A receipt written before it reads as delivered into a turn it did not
// open, so a cancel leaves that turn running.
const receiptOwnTurnMigrationVersion = 151

const receiptOwnTurnV151SQL = `
ALTER TABLE thread_request_receipts ADD COLUMN own_turn INTEGER NOT NULL DEFAULT 0 CHECK(own_turn IN (0,1));
`
