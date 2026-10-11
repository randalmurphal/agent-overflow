package store

// MigrationStatus is where an open database stands against its build: the
// applied migration version beside the build's latest, and the deferred
// phases' watermark beside the last version that carries a deferred phase.
// Pending deferred work is Watermark below Latest.
type MigrationStatus struct {
	Applied           int
	Build             int
	DeferredWatermark int
	DeferredLatest    int
}

// MigrationStatus reads the open database's versions. An open store has
// applied every migration of its build, so Applied equals Build unless the
// open skipped migration (RefusePendingMigrations), and the deferred
// watermark moves as RunDeferredMigrations finishes each phase.
func (s *Store) MigrationStatus() (MigrationStatus, error) {
	applied, err := schemaVersion(s.db)
	if err != nil {
		return MigrationStatus{}, err
	}
	watermark, err := readDeferredWatermark(s.db)
	if err != nil {
		return MigrationStatus{}, err
	}
	return MigrationStatus{
		Applied:           applied,
		Build:             migrations[len(migrations)-1].Version,
		DeferredWatermark: watermark,
		DeferredLatest:    latestDeferredVersion,
	}, nil
}
