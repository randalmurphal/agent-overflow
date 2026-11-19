package harnessrpc

import "fmt"

// HarnessUpdateWorkReason answers what a service update or restart would
// wait for right now, through the production admission check, or "" when
// the host is idle. It only reads: admission stays open and no update
// starts, so a spec can assert what holds an update without an updater.
func (h *Harness) HarnessUpdateWorkReason() (string, error) {
	if h.config.Host == nil {
		return "", fmt.Errorf("harness host unavailable")
	}
	return h.config.Host.UpdateWorkReason()
}
