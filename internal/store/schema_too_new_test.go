package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// stampAhead writes a database this build creates, then records a
// migration version and deferred watermark as a newer build would. A zero
// leaves that value as this build wrote it.
func stampAhead(t *testing.T, path string, migration, watermark int) {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if migration > 0 {
		mustExec(t, raw, `INSERT INTO migration_versions(version, name) VALUES(?, 'from a newer build')`, migration)
	}
	if watermark > 0 {
		mustExec(t, raw, fmt.Sprintf(`PRAGMA user_version = %d`, watermark))
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

func latestMigrationVersionForTest() int { return migrations[len(migrations)-1].Version }

// An older build refuses a database a newer build migrated, with the
// sentence the boot failure shows, before it applies or writes anything.
func TestNewRefusesADatabaseANewerBuildMigrated(t *testing.T) {
	known := latestMigrationVersionForTest()
	for _, tc := range []struct {
		name                 string
		migration, watermark int
		want                 int
	}{
		{name: "migration record", migration: known + 2, want: known + 2},
		{name: "deferred watermark", watermark: known + 1, want: known + 1},
		{name: "both", migration: known + 1, watermark: known + 3, want: known + 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ahead.db")
			stampAhead(t, path, tc.migration, tc.watermark)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			s, err := New(path)
			if err == nil {
				s.Close()
				t.Fatal("a database ahead of this build opened")
			}
			want := fmt.Sprintf("database is at schema v%d; this build knows v%d; install the newer version", tc.want, known)
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
			if tooNew, ok := err.(*SchemaTooNewError); !ok || tooNew.Database != tc.want || tooNew.Build != known {
				t.Fatalf("error = %#v, want an unwrapped SchemaTooNewError", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				var diff []int
				for i := range min(len(before), len(after)) {
					if before[i] != after[i] && len(diff) < 20 {
						diff = append(diff, i)
					}
				}
				t.Fatalf("the refused open changed the database file: %d -> %d bytes, first differing offsets %v", len(before), len(after), diff)
			}
		})
	}
}

// A database at this build's version opens, whatever its watermark below
// that is.
func TestNewOpensADatabaseAtThisBuildsVersion(t *testing.T) {
	known := latestMigrationVersionForTest()
	path := filepath.Join(t.TempDir(), "current.db")
	stampAhead(t, path, 0, known)
	s, err := New(path)
	if err != nil {
		t.Fatalf("open a database at v%d: %v", known, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// RestoreFrom migrates its snapshot through New, so a snapshot a newer
// build wrote is refused and the live rows stay.
func TestRestoreRefusesASnapshotFromANewerBuild(t *testing.T) {
	live := openStoreAt(t)
	t.Cleanup(func() { _ = live.Close() })
	if err := live.CreateThread(makeThread("kept", "claude")); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	stampAhead(t, snapshot, latestMigrationVersionForTest()+1, 0)

	_, err := live.RestoreFrom(snapshot)
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("restore of a newer snapshot = %v, want a SchemaTooNewError", err)
	}
	if _, err := live.GetThread("kept"); err != nil {
		t.Fatalf("the live thread after the refused restore: %v", err)
	}
}
