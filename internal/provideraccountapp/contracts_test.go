package provideraccountapp

import (
	"sync/atomic"
	"testing"
)

func TestSelectionLeaseReleaseIsIdempotent(t *testing.T) {
	var releases atomic.Int32
	lease := NewSelectionLease(Selection{Generation: 7, AccountID: "acct"}, func() {
		releases.Add(1)
	})
	lease.Release()
	lease.Release()
	if got := releases.Load(); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
}

func TestNilSelectionLeaseReleaseIsSafe(t *testing.T) {
	var lease *SelectionLease
	lease.Release()
}
