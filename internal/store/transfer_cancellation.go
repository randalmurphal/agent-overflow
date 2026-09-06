package store

import (
	"context"
	"errors"
	"time"
)

// ThreadTransferCancellationRequested is the upload loop's small per-chunk
// check. Do not reload megabytes of private recovery metadata for one bit.
func (s *Store) ThreadTransferCancellationRequested(id string) (bool, error) {
	var requested bool
	err := s.reader().QueryRow(`SELECT cancel_requested FROM thread_transfers WHERE id = ?`, id).Scan(&requested)
	return requested, err
}

// RequestThreadTransferCancellation records intent BEFORE contacting the peer.
// An interrupted cancellation must resume cancellation after restart, not fall
// back into the ordinary move path. The thread stays fenced until the peer
// acknowledges that it cannot activate (or no offer was ever bound).
func (s *Store) RequestThreadTransferCancellation(id string) (ThreadTransfer, error) {
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return ThreadTransfer{}, err
	}
	if row.Direction != "outgoing" || row.Phase == "committed" || row.Phase == "complete" {
		return ThreadTransfer{}, errors.New("transfer: this operation can no longer be canceled at the source")
	}
	if row.CancelRequested || row.Phase == "canceled" {
		return row, nil
	}
	row.CancelRequested, row.UpdatedAt = true, time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE thread_transfers SET cancel_requested = 1,updated_at = ? WHERE id = ?`, row.UpdatedAt, id); err != nil {
		return ThreadTransfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadTransfer{}, err
	}
	return row, nil
}

// CheckIncomingTransferCancellation allows cleanup before acknowledging
// cancellation. The source has durably committed to canceling before revealing
// this secret; only its failed cancellation retries can now resume this path.
func (s *Store) CheckIncomingTransferCancellation(id string, secret []byte) (ThreadTransfer, error) {
	row, err := s.GetThreadTransfer(id)
	if err != nil {
		return row, err
	}
	if row.Direction != "incoming" || !matchesTransferSecret(row, secret) || (row.Phase != "preparing" && row.Phase != "prepared" && row.Phase != "canceled") {
		return row, errors.New("transfer: cancellation is not authorized")
	}
	return row, nil
}

// DiscardUnpreparedIncomingTransfer releases an offer whose source could never
// have retired: retirement requires the prepared acknowledgment. The owning
// coordinator serializes this with preparation and cleans inert reservations
// before this transaction. Once prepared, cancellation needs the source proof.
func (s *Store) DiscardUnpreparedIncomingTransfer(id string) error {
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id=?`, id))
	if err != nil {
		return err
	}
	if row.Direction != "incoming" || (row.Phase != "preparing" && row.Phase != "canceled") {
		return errors.New("Cancel this prepared transfer from its source computer.")
	}
	if _, err := tx.Exec(`UPDATE thread_transfers SET phase='canceled',error='',updated_at=? WHERE id=?`, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelIncomingThreadTransfer requires the source's secret, just as activation
// does. A frontend holding only the destination offer cannot race retirement by
// canceling the destination independently. The source durably records its cancel
// intent before releasing this proof, and cannot subsequently retire/activate.
func (s *Store) CancelIncomingThreadTransfer(id string, secret []byte) (ThreadTransfer, error) {
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return ThreadTransfer{}, err
	}
	if row.Direction != "incoming" || !matchesTransferSecret(row, secret) || (row.Phase != "preparing" && row.Phase != "prepared" && row.Phase != "canceled") {
		return ThreadTransfer{}, errors.New("transfer: cancellation is not authorized")
	}
	if row.Phase == "canceled" {
		return row, nil
	}
	row.Phase, row.Error, row.UpdatedAt = "canceled", "", time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE thread_transfers SET phase = 'canceled',error = '',updated_at = ? WHERE id = ?`, row.UpdatedAt, id); err != nil {
		return ThreadTransfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadTransfer{}, err
	}
	return row, nil
}
