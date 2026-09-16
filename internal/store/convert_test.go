package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The conversion swap replaces the database file under both pools while
// the app is running. These tests pin what makes that safe: it happens
// only when nothing committed during the snapshot, it leaves no
// connection on the old file, it blocks callers for a bounded moment
// rather than failing them, and a crash at any step leaves a database
// the next open can use.

// mustSeedLegacyDatabase creates a database file that already has a
// table, so the auto_vacuum=INCREMENTAL that runMigrations sets is the
// silent no-op SQLite makes it on a non-empty file. That is exactly the
// shape of a database created before incremental auto-vacuum was
// adopted.
func mustSeedLegacyDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatalf("seed legacy database: %v", err)
	}
	defer db.Close()
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		t.Fatalf("seed legacy database journal mode: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE legacy_seed (x INTEGER)`); err != nil {
		t.Fatalf("seed legacy database table: %v", err)
	}
}

// newLegacyStore opens a migrated store whose file is auto_vacuum=none.
func newLegacyStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	mustSeedLegacyDatabase(t, path)
	s, err := New(path)
	if err != nil {
		t.Fatalf("new legacy store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close legacy store: %v", err)
		}
	})
	if mode, err := s.AutoVacuumMode(); err != nil || mode != AutoVacuumNone {
		t.Fatalf("seeded store auto_vacuum = %v (%v), want none", mode, err)
	}
	if _, err := s.CreateProject(Project{
		ID: defaultTestProjectID, Path: "/tmp/test", Name: "Default Test Project",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return s
}

func TestConvertToIncrementalVacuumSwapsFileWithoutFailingCallers(t *testing.T) {
	s := newLegacyStore(t)
	if err := s.SetUIState("client:test", map[string]string{"k": "before"}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	fileBefore, err := os.Stat(s.path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	// Callers started inside the blocked window are the ones the swap
	// has to keep correct: they block on a connection the swap is about
	// to retire and must come back on the new file.
	var (
		wg           sync.WaitGroup
		writeErr     error
		readErr      error
		readValue    string
		writeLatency time.Duration
		readLatency  time.Duration
	)
	const hookSettle = 20 * time.Millisecond
	started := make(chan struct{})
	s.convertHooks.insideWindow = func() {
		wg.Add(2)
		go func() {
			defer wg.Done()
			start := time.Now()
			writeErr = s.SetUIState("client:test", map[string]string{"k": "during"})
			writeLatency = time.Since(start)
		}()
		go func() {
			defer wg.Done()
			start := time.Now()
			state, err := s.GetUIState("client:test")
			readLatency = time.Since(start)
			readErr = err
			readValue = state["k"]
		}()
		// Give both goroutines time to reach the pool before the swap
		// retires the connection they are waiting on. This sleep is
		// inside the measured window, so it is subtracted below.
		time.Sleep(hookSettle)
		close(started)
	}
	defer func() { s.convertHooks.insideWindow = nil }()

	result, err := s.ConvertToIncrementalVacuum(context.Background())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	<-started
	wg.Wait()

	if result.Outcome != ConvertConverted {
		t.Fatalf("outcome = %v, want converted", result.Outcome)
	}
	swapWindow := result.BlockedWindow - hookSettle
	t.Logf("swap blocked callers for %s (%s excluding the test hook), %d -> %d bytes",
		result.BlockedWindow, swapWindow, result.SizeBefore, result.SizeAfter)
	if swapWindow > 100*time.Millisecond {
		t.Fatalf("swap blocked callers for %s, want well under 100ms", swapWindow)
	}
	if writeErr != nil {
		t.Fatalf("write started during the swap failed: %v", writeErr)
	}
	if readErr != nil {
		t.Fatalf("read started during the swap failed: %v", readErr)
	}
	if readValue != "before" && readValue != "during" {
		t.Fatalf("read during the swap returned %q, want the row", readValue)
	}
	for _, latency := range []time.Duration{writeLatency, readLatency} {
		if latency > 2*time.Second {
			t.Fatalf("caller blocked %s during the swap", latency)
		}
	}

	if mode, err := s.AutoVacuumMode(); err != nil || mode != AutoVacuumIncremental {
		t.Fatalf("auto_vacuum after convert = %v (%v), want incremental", mode, err)
	}
	// VACUUM INTO writes a fresh file in the default rollback journal
	// mode; the swapped-in file must already be WAL or the read pool
	// silently stops being usable at the next open.
	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode after convert: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode after convert = %s, want wal", journalMode)
	}
	fileAfter, err := os.Stat(s.path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if os.SameFile(fileBefore, fileAfter) {
		t.Fatal("the database file was not replaced")
	}
	if _, err := os.Stat(s.path + asideSuffix); err == nil {
		t.Fatal("the outgoing database was not unlinked")
	}
	if _, err := os.Stat(s.path + incrementalTmpSuffix); err == nil {
		t.Fatal("the snapshot file was left behind")
	}

	// Nothing may still be writing to the old file: close and reopen,
	// and the write that landed during the swap must be there.
	if err := s.SetUIState("client:test", map[string]string{"k": "after"}); err != nil {
		t.Fatalf("write after swap: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close after swap: %v", err)
	}
	reopened, err := New(s.path)
	if err != nil {
		t.Fatalf("reopen after swap: %v", err)
	}
	defer reopened.Close()
	state, err := reopened.GetUIState("client:test")
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if state["k"] != "after" {
		t.Fatalf("row after reopen = %q, want after: a connection to the replaced file survived", state["k"])
	}
	if mode, err := reopened.AutoVacuumMode(); err != nil || mode != AutoVacuumIncremental {
		t.Fatalf("reopened auto_vacuum = %v (%v), want incremental", mode, err)
	}
	if reopened.read == nil {
		t.Fatal("the reopened store has no read pool: the swapped-in file is not in WAL mode")
	}
}

func TestConvertToIncrementalVacuumRefusesWhenAWriteLandsDuringTheSnapshot(t *testing.T) {
	s := newLegacyStore(t)

	s.convertHooks.afterSnapshot = func() {
		// VACUUM INTO copies the database as of the start of its read
		// transaction, so this row is missing from the snapshot.
		if err := s.SetUIState("client:test", map[string]string{"k": "raced"}); err != nil {
			t.Errorf("racing write: %v", err)
		}
	}
	defer func() { s.convertHooks.afterSnapshot = nil }()

	result, err := s.ConvertToIncrementalVacuum(context.Background())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if result.Outcome != ConvertNotQuiet {
		t.Fatalf("outcome = %v, want not quiet", result.Outcome)
	}
	if _, err := os.Stat(s.path + incrementalTmpSuffix); err == nil {
		t.Fatal("the refused snapshot was not removed")
	}
	if mode, err := s.AutoVacuumMode(); err != nil || mode != AutoVacuumNone {
		t.Fatalf("auto_vacuum = %v (%v), want the database untouched", mode, err)
	}
	state, err := s.GetUIState("client:test")
	if err != nil || state["k"] != "raced" {
		t.Fatalf("the racing write must survive: %q (%v)", state["k"], err)
	}
}

func TestConvertToIncrementalVacuumRefusesWhileAnotherConnectionHoldsTheFile(t *testing.T) {
	s := newLegacyStore(t)
	watcher, err := s.NewCommitWatcher(context.Background())
	if err != nil {
		t.Fatalf("commit watcher: %v", err)
	}
	defer watcher.Close()
	if _, err := watcher.DataVersion(context.Background()); err != nil {
		t.Fatalf("data_version: %v", err)
	}

	result, err := s.ConvertToIncrementalVacuum(context.Background())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if result.Outcome != ConvertNotQuiet {
		t.Fatalf("outcome = %v, want not quiet while a connection holds the file", result.Outcome)
	}
	if _, err := os.Stat(s.path + incrementalTmpSuffix); err == nil {
		t.Fatal("the refused snapshot was not removed")
	}
	if err := s.SetUIState("client:test", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("store must still work after a refused swap: %v", err)
	}
}

func TestConvertToIncrementalVacuumIsANoOpOnAnIncrementalDatabase(t *testing.T) {
	s := newTestStore(t)
	result, err := s.ConvertToIncrementalVacuum(context.Background())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if result.Outcome != ConvertAlreadyIncremental {
		t.Fatalf("outcome = %v, want already incremental", result.Outcome)
	}
}

func TestConvertToIncrementalVacuumIsUnsupportedInMemory(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	result, err := s.ConvertToIncrementalVacuum(context.Background())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if result.Outcome != ConvertUnsupported {
		t.Fatalf("outcome = %v, want unsupported", result.Outcome)
	}
}

// TestWaitSidecarsGone pins the rule the swap renames on: a -wal or -shm
// beside the database means a connection still holds it, in this process
// or another, and the file must not be replaced.
func TestWaitSidecarsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.sqlite")
	if err := os.WriteFile(path, []byte("db"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !waitSidecarsGone(path) {
		t.Fatal("a database with no sidecars must report clear")
	}
	for _, suffix := range walSidecarSuffixes {
		if err := os.WriteFile(path+suffix, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", suffix, err)
		}
		if waitSidecarsGone(path) {
			t.Fatalf("a database with a %s must not be swapped", suffix)
		}
		if err := os.Remove(path + suffix); err != nil {
			t.Fatalf("remove %s: %v", suffix, err)
		}
	}
}

func TestNewRemovesLeftoverConversionSnapshot(t *testing.T) {
	path := newTestStorePath(t)
	leftover := path + incrementalTmpSuffix
	if err := os.WriteFile(leftover, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed leftover: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(leftover); err == nil {
		t.Fatal("a leftover snapshot must be removed at open: it is older than the live database")
	}
}

func TestNewRestoresDatabaseInterruptedBetweenRenames(t *testing.T) {
	path := newTestStorePath(t)
	// The shape a crash between the two renames leaves: the live
	// database moved aside, nothing at the live path.
	if err := os.Rename(path, path+asideSuffix); err != nil {
		t.Fatalf("simulate interrupted swap: %v", err)
	}
	if err := os.WriteFile(path+incrementalTmpSuffix, []byte("partial"), 0o600); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(path + asideSuffix); err == nil {
		t.Fatal("the outgoing database must be moved back, not left aside")
	}
	if _, err := os.Stat(path + incrementalTmpSuffix); err == nil {
		t.Fatal("the partial snapshot must be removed")
	}
	if _, err := s.GetUIState("client:test"); err != nil {
		t.Fatalf("restored database is not usable: %v", err)
	}
}
