package threadtransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transferwire"
)

func (s *Source) sourceArchive(ctx context.Context, row store.ThreadTransfer) (store.ThreadTransfer, transferwire.Upload, error) {
	var initial transferwire.Upload
	if row.ArchiveSize != 0 {
		bound := transferwire.Upload{SHA256: row.ManifestHash, Size: row.ArchiveSize}
		if !bound.Valid() {
			return row, bound, errors.New("transfer: source archive no longer matches its sealed identity")
		}
		return row, bound, nil
	}
	if row.Phase != "preparing" {
		return row, initial, errors.New("transfer: source is missing its sealed snapshot")
	}
	if s.snapshot == nil {
		return row, initial, errors.New("transfer: source snapshot preparation is unavailable")
	}
	initial, err := s.snapshot(ctx, row, filepath.Join(s.root, row.ID))
	if err != nil {
		return row, initial, err
	}
	if !initial.Valid() {
		return row, initial, errors.New("transfer: snapshotter returned an invalid archive identity")
	}
	info, err := os.Lstat(filepath.Join(s.root, row.ID, "archive.tar"))
	if err != nil {
		return row, initial, err
	}
	if !info.Mode().IsRegular() || info.Size() != initial.Size {
		return row, initial, errors.New("transfer: source archive does not match its snapshot receipt")
	}
	row, err = s.store.BindThreadTransferArchive(row.ID, initial)
	return row, initial, err
}
