package harnessrpc

import (
	"fmt"
	"strings"
)

// HarnessStallProviderEvents holds the thread's provider event worker the
// way a handler stuck on a slow write would: every event read from the
// thread's provider afterwards queues behind it, unhandled, until
// HarnessReleaseProviderEvents. A session stop's drain gives up on such a
// worker at its stall bound (the `drain-stall` timing entry), which is how
// a spec runs a replacement start with the dead session's frames still
// queued. One stall per thread at a time.
func (h *Harness) HarnessStallProviderEvents(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("thread id is required")
	}
	return h.config.Host.StallProviderEvents(threadID)
}

// HarnessReleaseProviderEvents lets the thread's stalled worker go on
// handling its queue in order.
func (h *Harness) HarnessReleaseProviderEvents(threadID string) error {
	return h.config.Host.ReleaseProviderEvents(strings.TrimSpace(threadID))
}

// HarnessPendingProviderEvents answers how many events queued for the
// thread are not handled yet, a stall included, so a spec can see a
// provider's frames reach the queue before it acts.
func (h *Harness) HarnessPendingProviderEvents(threadID string) int {
	return h.config.Host.PendingProviderEvents(strings.TrimSpace(threadID))
}
