package harnessrpc

import (
	"context"
	"fmt"
)

// HarnessPrimeTransferTemplate builds the process's migrated database image
// now. A received conversation transfer clones that image while it validates
// the history, and the first build in a process runs every migration; a spec
// that primes the receiving backend before it starts a transfer keeps that
// build out of the transfer's own wait. Calling it again is cheap. The build
// ends with the call's context, so a stopped backend does not wait on it.
func (h *Harness) HarnessPrimeTransferTemplate(ctx context.Context) error {
	if err := h.primeTemplate(ctx, h.config.DataDir); err != nil {
		return fmt.Errorf("build the migrated database template: %w", err)
	}
	return nil
}
