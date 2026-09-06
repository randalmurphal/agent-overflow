package store

import (
	"context"
	"errors"
	"time"

	"agent-overflow/internal/transferwire"
)

// BindThreadTransferArchive seals the source's finished snapshot, or the first
// upload identity accepted by a provisional destination offer. It never means
// the destination has validated the content: that requires phase=prepared.
// Keeping the receipt in SQLite lets file cleanup preserve status and recovery.
func (s *Store) BindThreadTransferArchive(id string, archive transferwire.Upload) (ThreadTransfer, error) {
	if !archive.Valid() {
		return ThreadTransfer{}, errors.New("transfer: invalid archive identity")
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return row, err
	}
	if row.ArchiveSize != 0 {
		if row.ArchiveSize != archive.Size || row.ManifestHash != archive.SHA256 {
			return row, errors.New("transfer: archive identity is already bound")
		}
		return row, nil
	}
	if row.Phase != "preparing" || (row.ManifestHash != "" && row.ManifestHash != archive.SHA256) {
		return row, errors.New("transfer: archive cannot change after preparation")
	}
	row.ManifestHash, row.ArchiveSize, row.UpdatedAt = archive.SHA256, archive.Size, time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE thread_transfers SET manifest_hash = ?,archive_size = ?,updated_at = ? WHERE id = ?`, row.ManifestHash, row.ArchiveSize, row.UpdatedAt, id); err != nil {
		return row, err
	}
	if err := tx.Commit(); err != nil {
		return row, err
	}
	return row, nil
}
