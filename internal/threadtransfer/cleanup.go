package threadtransfer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

// Called with the operation lock held, after completion or acknowledged
// cancellation. Delete only private operation scratch; the installer's native
// files and published checkout live elsewhere. The compact SQL ownership and
// receipt survive indefinitely, so status/retry never needs the removed bytes.
func cleanupTransfer(ctx context.Context, st *store.Store, directory string, row store.ThreadTransfer) error {
	if (row.Phase != "complete" && row.Phase != "canceled") || !entityid.Valid(row.ID) || !filepath.IsAbs(directory) {
		return errors.New("transfer: cannot clean an unfinished operation")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return st.FinishThreadTransferCleanup(row.ID)
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(row.ID); err != nil {
		return err
	}
	if err := atomicfile.SyncRootDir(root, "."); err != nil {
		return err
	}
	return st.FinishThreadTransferCleanup(row.ID)
}
