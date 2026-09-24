package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// TestRefusePendingMigrationsLeavesTheDatabaseAsItWas: an open that must
// not migrate refuses an older database and changes none of its bytes; the
// same open migrates it once the option is off.
func TestRefusePendingMigrationsLeavesTheDatabaseAsItWas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-overflow.db")
	writeHistoryRepairFixture(t, path)
	before := fileDigest(t, path)

	_, err := NewWithOptions(path, Options{RefusePendingMigrations: true})
	var pending *MigrationsPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("open = %v, want a MigrationsPendingError", err)
	}
	// The count is the chain's, not the version gap: versions need not be
	// contiguous while lanes land.
	after118 := 0
	for _, m := range migrations {
		if m.Version > 118 {
			after118++
		}
	}
	if pending.Database != 118 || pending.Build != latestMigrationVersionForTest() || pending.Pending != after118 {
		t.Fatalf("refusal = %+v, want %d pending after v118", pending, after118)
	}
	if after := fileDigest(t, path); after != before {
		t.Fatal("the refused open changed the database")
	}

	s, err := NewWithOptions(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewWithOptions(path, Options{RefusePendingMigrations: true})
	if err != nil {
		t.Fatalf("a migrated database was refused: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRefusePendingMigrationsCreatesANewDatabase: nothing is pending in a
// database that does not exist yet, or whose first migration never
// committed.
func TestRefusePendingMigrationsCreatesANewDatabase(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"no file", func(*testing.T, string) {}},
		{"no applied migration", func(t *testing.T, path string) {
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(createMigrationVersionsTableSQL); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-overflow.db")
			c.setup(t, path)
			s, err := NewWithOptions(path, Options{RefusePendingMigrations: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRefusePendingMigrationsLeavesANewerSchemaToItsOwnRefusal: a database
// a newer build migrated has nothing pending here; the open refuses it as
// too new, so the launcher shows that failure instead of running a trial.
func TestRefusePendingMigrationsLeavesANewerSchemaToItsOwnRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-overflow.db")
	stampAhead(t, path, latestMigrationVersionForTest()+2, 0)
	_, err := NewWithOptions(path, Options{RefusePendingMigrations: true})
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("open = %v, want a SchemaTooNewError", err)
	}
}

// TestPendingMigrationsIsTheRefusalsRule: the answer the desktop boot asks
// for a schema version it read is the refusal an open with the option gives
// for a database at that version.
func TestPendingMigrationsIsTheRefusalsRule(t *testing.T) {
	latest := latestMigrationVersionForTest()
	for _, applied := range []int{0, latest} {
		if err := PendingMigrations(applied); err != nil {
			t.Fatalf("PendingMigrations(%d) = %v; want nothing pending", applied, err)
		}
	}
	if err := PendingMigrations(latest + 2); err != nil {
		t.Fatalf("PendingMigrations(a newer schema) = %v; that is SchemaTooNewError's", err)
	}

	path := filepath.Join(t.TempDir(), "agent-overflow.db")
	writeHistoryRepairFixture(t, path)
	applied, err := ReadSchemaVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	answer := PendingMigrations(applied)
	_, refusal := NewWithOptions(path, Options{RefusePendingMigrations: true})
	var fromAnswer, fromOpen *MigrationsPendingError
	if !errors.As(answer, &fromAnswer) || !errors.As(refusal, &fromOpen) || *fromAnswer != *fromOpen {
		t.Fatalf("PendingMigrations(%d) = %v; the open refused with %v", applied, answer, refusal)
	}
}

// databaseFileSet is each file of the database at path that exists, by
// digest.
func databaseFileSet(t *testing.T, path string) map[string][32]byte {
	t.Helper()
	files := map[string][32]byte{}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			files[suffix] = fileDigest(t, path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return files
}

// TestReadSchemaVersionReadsWithoutChangingTheDatabase: the version is the
// one the refusal reports, including committed WAL content a checkpoint has
// not reached. A database closed cleanly keeps its bytes and its file set,
// which is what lets the update's restore put back the database as the
// backend left it. Reading a missing database creates nothing.
func TestReadSchemaVersionReadsWithoutChangingTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-overflow.db")
	writeHistoryRepairFixture(t, path)
	before := databaseFileSet(t, path)
	if got, err := ReadSchemaVersion(path); err != nil || got != 118 {
		t.Fatalf("ReadSchemaVersion(the v118 fixture) = %d, %v", got, err)
	}
	if after := databaseFileSet(t, path); len(after) != len(before) || after[""] != before[""] {
		t.Fatalf("reading the schema version changed the database's files: %d files before, %d after", len(before), len(after))
	}

	// A writer that keeps its WAL: the row it commits is only in the WAL.
	writer, err := sql.Open("sqlite", "file:"+path+"?_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO migration_versions (version, name) VALUES (119, 'in the WAL')`); err != nil {
		t.Fatal(err)
	}
	// The files as a backend that died leaves them.
	stopped := filepath.Join(t.TempDir(), "agent-overflow.db")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stopped+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if got, err := ReadSchemaVersion(stopped); err != nil || got != 119 {
			t.Fatalf("ReadSchemaVersion with the row in the WAL = %d, %v; want 119", got, err)
		}
	}

	missing := filepath.Join(t.TempDir(), "agent-overflow.db")
	if _, err := ReadSchemaVersion(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadSchemaVersion(a missing database) = %v, want not-exist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading a missing database created it: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "agent-overflow.db")
	db, err := sql.Open("sqlite", "file:"+empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSchemaVersion(empty); err != nil || got != 0 {
		t.Fatalf("ReadSchemaVersion(no migration table) = %d, %v; want 0", got, err)
	}
}
