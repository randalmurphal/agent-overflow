package harnessrpc

import "testing"

// HarnessPrimeTransferTemplate builds the process's migrated image and may be
// called again.
func TestHarnessPrimeTransferTemplateIsIdempotent(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	h.config.DataDir = t.TempDir()
	for range 2 {
		if err := h.HarnessPrimeTransferTemplate(); err != nil {
			t.Fatal(err)
		}
	}
}
