//go:build !noremote

package deviceclient

import (
	"errors"
	"testing"
)

// The client's lock holders report a failed release the same way the
// profile functions do; they live behind the remote build tag with the
// backend fixture they need.
func TestClientLockHoldersReportAFailedRelease(t *testing.T) {
	t.Run("sessionTransaction", func(t *testing.T) {
		client, _ := openAgainst(t, newBackend(t), nil)
		failProfileLockRelease(t, "backend-a")
		err := client.sessionTransaction(t.Context(), func(string, *Session) error { return nil })
		if !errors.Is(err, errReleaseRefused) {
			t.Fatalf("sessionTransaction = %v", err)
		}
	})
	t.Run("rotate legacy renewal lock", func(t *testing.T) {
		// openAgainst's session reports no renewal recovery, so a rotation
		// serializes on the legacy-renewal lock; only that lock fails here,
		// so the error is rotate's own release, not a transaction's.
		backend := newBackend(t)
		client, _ := openAgainst(t, backend, nil)
		failProfileLockRelease(t, "legacy-renewal")
		err := client.renew(t.Context())
		if !errors.Is(err, errReleaseRefused) {
			t.Fatalf("renew = %v", err)
		}
		if backend.rotations.Load() != 1 {
			t.Fatalf("rotations = %d, want the exchange to have run", backend.rotations.Load())
		}
	})
}
