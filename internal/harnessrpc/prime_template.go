package harnessrpc

import (
	"context"
	"fmt"

	"agent-overflow/internal/store"
)

// HarnessPrimeTransferTemplate builds the process's migrated database image
// now. A received conversation transfer clones that image while it validates
// the history, and the first build in a process runs every migration; a spec
// that primes the receiving backend before it starts a transfer keeps that
// build out of the transfer's own wait. Calling it again is cheap.
func (h *Harness) HarnessPrimeTransferTemplate() error {
	if err := store.PrimeMigratedTemplate(context.Background(), h.config.DataDir); err != nil {
		return fmt.Errorf("build the migrated database template: %w", err)
	}
	return nil
}
