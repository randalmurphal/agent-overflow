package store

import (
	"database/sql"
	"errors"
)

// Epochs cross a JavaScript number boundary. Refuse a counter that a frontend
// could not compare exactly, rather than rounding two owners into a tie.
const maxOwnershipEpoch int64 = 1<<53 - 1

// Move epochs order two computers' catalogs even when one of them is offline.
// The journal is authoritative; timestamps and arrival order cannot decide who
// owns a conversation. A copy has a new identity and starts at epoch zero.
func assignTransferEpoch(tx *sql.Tx, request *ThreadTransfer) error {
	if request.OwnershipEpoch < 0 || request.OwnershipEpoch > maxOwnershipEpoch {
		return errors.New("transfer: invalid ownership epoch")
	}
	if request.Kind == "copy" {
		if request.OwnershipEpoch != 0 {
			return errors.New("transfer: a copy starts a new ownership history")
		}
		return nil
	}
	if request.Direction == "outgoing" {
		var current int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(ownership_epoch),0) FROM thread_transfers WHERE thread_id = ? AND direction = 'incoming' AND phase = 'complete'`, request.ThreadID).Scan(&current); err != nil {
			return err
		}
		if current >= maxOwnershipEpoch {
			return errors.New("transfer: ownership counter exhausted")
		}
		next := current + 1
		if request.OwnershipEpoch != 0 && request.OwnershipEpoch != next {
			return errors.New("transfer: source ownership changed before preparation")
		}
		request.OwnershipEpoch = next
		return nil
	}
	var seen int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(ownership_epoch),0) FROM thread_transfers WHERE thread_id = ? AND phase <> 'canceled'`, request.ThreadID).Scan(&seen); err != nil {
		return err
	}
	if request.OwnershipEpoch <= seen {
		return errors.New("transfer: this computer has already seen a newer owner of the conversation")
	}
	return nil
}
