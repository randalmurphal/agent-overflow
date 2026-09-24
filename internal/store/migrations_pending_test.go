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
	if pending.Database != 118 || pending.Build != latestMigrationVersionForTest() || pending.Pending != latestMigrationVersionForTest()-118 {
		t.Fatalf("refusal = %+v", pending)
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
