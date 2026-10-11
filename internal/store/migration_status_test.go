package store

import (
	"path/filepath"
	"testing"
)

// TestMigrationStatusReportsAMigratedDatabaseAtItsBuild: a store this build
// opened and migrated reports the chain's latest version applied and no
// deferred phase pending, and the deferred figures agree with
// DeferredMigrationsPending.
func TestMigrationStatusReportsAMigratedDatabaseAtItsBuild(t *testing.T) {
	t.Parallel()
	s, err := New(filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	status, err := s.MigrationStatus()
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	want := migrations[len(migrations)-1].Version
	if status.Applied != want || status.Build != want {
		t.Fatalf("status = %+v, want applied and build %d", status, want)
	}
	if status.DeferredLatest != latestDeferredVersion {
		t.Fatalf("DeferredLatest = %d, want %d", status.DeferredLatest, latestDeferredVersion)
	}
	pending, err := s.DeferredMigrationsPending()
	if err != nil {
		t.Fatalf("DeferredMigrationsPending: %v", err)
	}
	if got := status.DeferredWatermark < status.DeferredLatest; got != pending {
		t.Fatalf("watermark %d against latest %d says pending=%v, DeferredMigrationsPending says %v",
			status.DeferredWatermark, status.DeferredLatest, got, pending)
	}
}
