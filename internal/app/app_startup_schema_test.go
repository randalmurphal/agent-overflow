package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/store"
)

// A database a newer build migrated fails boot with the store's sentence,
// unwrapped, so the startup failure surface shows it as written.
func TestStartRefusesADatabaseFromANewerBuild(t *testing.T) {
	dataRoot := t.TempDir()
	dbPath := filepath.Join(dataRoot, "agent-overflow", databaseFileName)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const ahead = 1_000_000
	if _, err := raw.Exec(`INSERT INTO migration_versions(version, name) VALUES(?, 'from a newer build')`, ahead); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	app.dataDirOverride = dataRoot
	err = app.Start(context.Background())
	t.Cleanup(func() {
		if app.appCancel != nil {
			app.appCancel()
		}
	})
	tooNew, ok := err.(*store.SchemaTooNewError)
	if !ok || tooNew.Database != ahead {
		t.Fatalf("Start = %#v, want the store's SchemaTooNewError unwrapped", err)
	}
	want := fmt.Sprintf("database is at schema v%d; this build knows v%d; install the newer version", ahead, tooNew.Build)
	if err.Error() != want {
		t.Fatalf("Start error = %q, want %q", err, want)
	}
	if app.store != nil {
		t.Fatal("a refused database was installed as the app's store")
	}
}

// A boot that must not migrate live refuses a database with pending
// migrations before anything writes it, and installs no store.
func TestStartRefusesPendingMigrationsWhenAsked(t *testing.T) {
	dataRoot := t.TempDir()
	dbPath := filepath.Join(dataRoot, "agent-overflow", databaseFileName)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var latest int
	if err := raw.QueryRow(`SELECT max(version) FROM migration_versions`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM migration_versions WHERE version = ?`, latest); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	app := NewApp()
	app.dataDirOverride = dataRoot
	RefusePendingMigrations(app)
	err = app.Start(context.Background())
	t.Cleanup(func() {
		if app.appCancel != nil {
			app.appCancel()
		}
	})
	var pending *store.MigrationsPendingError
	if !errors.As(err, &pending) || pending.Database != latest-1 || pending.Build != latest || pending.Pending != 1 {
		t.Fatalf("Start = %v, want a MigrationsPendingError from v%d", err, latest-1)
	}
	if app.store != nil {
		t.Fatal("a refused database was installed as the app's store")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the refused boot changed the database")
	}
}
