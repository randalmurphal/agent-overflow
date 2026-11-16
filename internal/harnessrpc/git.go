// git.go is the harness surface of the app's own git work.
package harnessrpc

import (
	"context"
	"fmt"
)

// HarnessBackgroundFetchPass runs one pass of the unattended fetch cadence
// over every project, as its timer would, and returns when the pass ends.
// An isolated boot never starts the cadence, so this is how a spec fetches
// in the background. The pass honors the background fetch setting and the
// per-repository freshness window and failure backoff; HarnessReset forgets
// both.
func (h *Harness) HarnessBackgroundFetchPass(ctx context.Context) error {
	if h.config.Host == nil {
		return fmt.Errorf("harness host unavailable")
	}
	h.config.Host.RunBackgroundFetchPass(ctx)
	return nil
}
