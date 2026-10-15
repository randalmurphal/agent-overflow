package store

import (
	"database/sql"
	"errors"
)

// Migration v152 records whether the turn a request's message was delivered
// into opened for that message (docs/specs/agent-thread-tools.md, "Cancel").
// A cancel interrupts the turn only when it did: a message drained into a
// turn already running joined a turn someone else started.
//
// A receipt written before it reads as delivered into a turn it did not
// open, so a cancel leaves that turn running.
const receiptOwnTurnMigrationVersion = 152

const receiptOwnTurnV152SQL = `
ALTER TABLE thread_request_receipts ADD COLUMN own_turn INTEGER NOT NULL DEFAULT 0 CHECK(own_turn IN (0,1));
`

// errPreSyncReceiptOwnTurn names why v152 cannot apply to a database that
// already has own_turn: a personal-assistant build from before main's
// thread_pull_requests took v142 recorded this migration as v151, so the
// database holds it and lacks thread_pull_requests.
var errPreSyncReceiptOwnTurn = errors.New("thread_request_receipts.own_turn already exists: this database was migrated by a pre-sync personal-assistant build; see docs/architecture/development.md#a-database-from-a-pre-sync-personal-assistant-build")

// refusePreSyncReceiptOwnTurn is v152's Check.
func refusePreSyncReceiptOwnTurn(tx *sql.Tx) error {
	columns, err := tableColumns(tx, "thread_request_receipts")
	if err != nil {
		return err
	}
	if columns["own_turn"] {
		return errPreSyncReceiptOwnTurn
	}
	return nil
}
