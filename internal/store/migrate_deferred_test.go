package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
)

func deferredWatermarkOf(t *testing.T, s *Store) int {
	t.Helper()
	watermark, err := readDeferredWatermark(s.db)
	if err != nil {
		t.Fatal(err)
	}
	return watermark
}

func deferredPending(t *testing.T, s *Store) bool {
	t.Helper()
	pending, err := s.DeferredMigrationsPending()
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func deferredFailureOf(t *testing.T, s *Store) *DeferredFailure {
	t.Helper()
	failure, err := s.DeferredMigrationFailure()
	if err != nil {
		t.Fatal(err)
	}
	return failure
}

// reopenStore closes s and opens its file again.
func reopenStore(t *testing.T, s *Store) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	return reopened
}

// openStoreAt opens a store on a copy of the migrated template. The caller
// closes it, usually through reopenStore.
func openStoreAt(t *testing.T) *Store {
	t.Helper()
	s, err := New(newTestStorePath(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// sealedStoreBelowV119 opens a store whose threads hold sealed history and
// whose watermark is below v119, as an upgraded database is.
func sealedStoreBelowV119(t *testing.T, rows int, threads ...string) *Store {
	t.Helper()
	return reopenStore(t, sealHistoryBelowV119(t, openStoreAt(t), rows, threads...))
}

func sealHistoryBelowV119(t *testing.T, s *Store, rows int, threads ...string) *Store {
	t.Helper()
	for _, thread := range threads {
		ids := localHistoryFixture(t, s, thread, rows)
		for start := 0; start < rows; start += 60 {
			sealItemsForTest(t, s, thread, ids[start:min(start+60, rows)]...)
		}
	}
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	return s
}

// captureLog collects the standard logger's output for the test.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNewDatabaseHasNoPendingDeferredMigrations(t *testing.T) {
	if latestDeferredVersion == 0 {
		t.Fatal("no migration carries a deferred phase")
	}
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if deferredPending(t, s) {
		t.Fatal("a new database has a pending deferred migration")
	}
	if got := deferredWatermarkOf(t, s); got != latestDeferredVersion {
		t.Fatalf("new database watermark = %d, want %d", got, latestDeferredVersion)
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("new database has a recorded failure: %+v", failure)
	}
}

// The v119 phase runs once: a quit leaves it pending and the next run
// resumes from the data, a finished run records the watermark in the file,
// and a recorded phase never runs again.
func TestDeferredMigrationResumesAndRunsOnce(t *testing.T) {
	s := openStoreAt(t)
	ids := localHistoryFixture(t, s, "t", 300)
	before := readRepairView(t, s, "t")
	for start := 0; start < 300; start += 60 {
		sealItemsForTest(t, s, "t", ids[start:start+60]...)
	}
	// A payload row the leak before v119 left behind.
	mustExec(t, s.db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('t','leaked','text','{}',CAST('leaked' AS BLOB),1)`)
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	s = reopenStore(t, s)
	if !deferredPending(t, s) {
		t.Fatal("a database below the v119 watermark has nothing pending")
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := s.RunDeferredMigrations(ctx, DeferredHost{Pause: ChunkPause(cancel)}); err != nil {
		t.Fatal(err)
	}
	moved := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id='t'`)
	if moved == 0 || moved == 300 {
		t.Fatalf("interrupted phase moved %d of 300 rows, want part of them", moved)
	}
	if !deferredPending(t, s) || deferredWatermarkOf(t, s) != 118 {
		t.Fatalf("interrupted phase recorded the watermark: %d", deferredWatermarkOf(t, s))
	}

	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredWatermarkOf(t, s) != 119 {
		t.Fatalf("finished phase left watermark %d", deferredWatermarkOf(t, s))
	}
	requireSameRepairView(t, "folded", readRepairView(t, s, "t"), before)
	requireNoImportedHistory(t, s)
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE id='leaked'`); n != 0 {
		t.Fatal("the orphan payload survived the phase")
	}

	s = reopenStore(t, s)
	if deferredPending(t, s) {
		t.Fatal("the watermark did not persist")
	}
	mustExec(t, s.db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('t','later','text','{}',CAST('later' AS BLOB),1)`)
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE id='later'`); n != 1 {
		t.Fatal("a recorded phase ran again")
	}
}

// A chunk whose fold fails is reported with its thread and left sealed, and
// the rest of the run goes on. The run leaves the watermark and records the
// failure, which survives a restart. The next run retries the chunk; once
// the fault is gone it finishes, advances the watermark and clears the
// record.
func TestHistoryRepairRetriesAFailingChunkOnTheNextRun(t *testing.T) {
	s := sealedStoreBelowV119(t, 20, "a", "b")
	var chunk string
	if err := s.db.QueryRow(`SELECT chunk_id FROM thread_import_chunks WHERE thread_id = 'a'`).Scan(&chunk); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `CREATE TRIGGER fail_chunk BEFORE DELETE ON import_history_chunks WHEN OLD.id = '`+chunk+`' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	logged := captureLog(t)
	requireRecorded := func(label string) {
		t.Helper()
		if !deferredPending(t, s) || deferredWatermarkOf(t, s) != 118 {
			t.Fatalf("%s: a run with a failed chunk moved the watermark to %d", label, deferredWatermarkOf(t, s))
		}
		failure := deferredFailureOf(t, s)
		if failure == nil || failure.Version != 119 || failure.Title != "History repair" || failure.Failures != 1 ||
			!strings.Contains(failure.FirstError, "thread a") || !strings.Contains(failure.FirstError, "injected") {
			t.Fatalf("%s: recorded failure = %+v", label, failure)
		}
	}

	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	requireRecorded("first run")
	if n := countRows(t, s, `SELECT count(*) FROM thread_import_chunks WHERE thread_id='b'`); n != 0 {
		t.Fatal("the failing chunk stopped the fold of another thread")
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_import_chunks WHERE thread_id='a'`); n != 1 {
		t.Fatal("the failing chunk lost its reference")
	}
	if n := countRows(t, s, `SELECT count(*) FROM timeline_items WHERE thread_id='a'`); n != 20 {
		t.Fatalf("the failing thread reads %d rows, want 20", n)
	}
	if n := strings.Count(logged(), "thread a keeps the rest of its sealed history"); n != 1 {
		t.Fatalf("the failing chunk was reported %d times in one run, want once:\n%s", n, logged())
	}

	s = reopenStore(t, s)
	requireRecorded("after a restart")
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	requireRecorded("second run")
	if n := strings.Count(logged(), "thread a keeps the rest of its sealed history"); n != 2 {
		t.Fatalf("the second run did not retry the chunk once:\n%s", logged())
	}

	mustExec(t, s.db, `DROP TRIGGER fail_chunk`)
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{}); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) || deferredWatermarkOf(t, s) != 119 {
		t.Fatalf("the run after the fault left watermark %d", deferredWatermarkOf(t, s))
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("a finished run kept the failure record: %+v", failure)
	}
	if n := countRows(t, s, `SELECT count(*) FROM deferred_migration_failures`); n != 0 {
		t.Fatalf("a finished run left %d failure rows", n)
	}
	requireNoImportedHistory(t, s)
}

// A step that returns an error counts as one failed item: the next step
// still runs, the watermark stays and the error is recorded. The next run
// retries the phase.
func TestDeferredStepErrorIsRecordedAndRetried(t *testing.T) {
	s := openStoreAt(t)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	injected := errors.New("injected")
	first, second := 0, 0
	chain := []Migration{{Version: 119, Deferred: &DeferredMigration{Title: "Probe", Steps: []DeferredStep{
		{Name: "first", Run: func(context.Context, *Store, *deferredRun) error {
			first++
			if first == 1 {
				return injected
			}
			return nil
		}},
		{Name: "second", Run: func(context.Context, *Store, *deferredRun) error {
			second++
			return nil
		}},
	}}}}

	if err := s.runDeferredMigrations(context.Background(), DeferredHost{}, chain); err != nil {
		t.Fatal(err)
	}
	if second != 1 {
		t.Fatal("a failing step stopped the next step")
	}
	if deferredWatermarkOf(t, s) != 118 {
		t.Fatal("a run with a failed step recorded the watermark")
	}
	failure, err := s.deferredMigrationFailure(chain)
	if err != nil {
		t.Fatal(err)
	}
	if failure == nil || failure.Title != "Probe" || failure.Failures != 1 || failure.FirstError != "v119 first: injected" {
		t.Fatalf("recorded failure = %+v", failure)
	}

	if err := s.runDeferredMigrations(context.Background(), DeferredHost{}, chain); err != nil {
		t.Fatal(err)
	}
	if err := s.runDeferredMigrations(context.Background(), DeferredHost{}, chain); err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != 2 || deferredWatermarkOf(t, s) != 119 {
		t.Fatalf("runs = %d/%d, watermark = %d; want the retry recorded once", first, second, deferredWatermarkOf(t, s))
	}
	if failure, err := s.deferredMigrationFailure(chain); err != nil || failure != nil {
		t.Fatalf("the finished retry kept the failure record: %+v, %v", failure, err)
	}
}

// A quit records nothing, not even the items that failed before it.
func TestDeferredMigrationQuitRecordsNothing(t *testing.T) {
	s := sealedStoreBelowV119(t, 20, "a", "b")
	mustExec(t, s.db, `CREATE TRIGGER fail_repair BEFORE INSERT ON items WHEN NEW.thread_id = 'a' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	ctx, cancel := context.WithCancel(context.Background())
	// Thread a fails first; the pause before thread b is the quit.
	if err := s.RunDeferredMigrations(ctx, DeferredHost{Pause: ChunkPause(cancel)}); err != nil {
		t.Fatal(err)
	}
	if deferredWatermarkOf(t, s) != 118 {
		t.Fatal("a quit moved the watermark")
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("a quit recorded a failure: %+v", failure)
	}
	if n := countRows(t, s, `SELECT count(*) FROM thread_import_chunks WHERE thread_id='b'`); n == 0 {
		t.Fatal("the run went on after the quit")
	}
}

// A restore while a phase runs replaces the rows the phase was working on,
// so the phase is not recorded over them and runs again on the restored rows.
func TestDeferredMigrationRerunsAfterRestore(t *testing.T) {
	snapshot := sealHistoryBelowV119(t, openStoreAt(t), 20, "restored")
	snapshotPath := snapshot.path
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	s := sealedStoreBelowV119(t, 120, "live")

	restored := false
	pause := func() {
		if restored {
			return
		}
		restored = true
		if _, err := s.RestoreFrom(snapshotPath); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	if err := s.RunDeferredMigrations(context.Background(), DeferredHost{Pause: pause}); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("the phase never paused")
	}
	if deferredPending(t, s) {
		t.Fatal("the phase did not finish on the restored rows")
	}
	requireNoImportedHistory(t, s)
	if n := countRows(t, s, `SELECT count(*) FROM items WHERE thread_id='restored'`); n != 20 {
		t.Fatalf("restored thread has %d local rows, want 20", n)
	}
}

// A restore replaces the rows, so the watermark and the failure record come
// from the snapshot.
func TestRestoreCarriesDeferredWatermark(t *testing.T) {
	live := newTestStore(t)
	mustExec(t, live.db, `INSERT INTO deferred_migration_failures(version,failures,first_error,failed_at) VALUES(200,3,'live',1)`)
	snapshot := newTestStorePath(t)
	raw, err := sql.Open("sqlite", "file:"+snapshot)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw, `PRAGMA user_version = 118`)
	mustExec(t, raw, `INSERT INTO deferred_migration_failures(version,failures,first_error,failed_at) VALUES(119,2,'snapshot',1)`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := live.RestoreFrom(snapshot); err != nil {
		t.Fatal(err)
	}
	if !deferredPending(t, live) || deferredWatermarkOf(t, live) != 118 {
		t.Fatalf("restored watermark = %d", deferredWatermarkOf(t, live))
	}
	failure := deferredFailureOf(t, live)
	if failure == nil || failure.Version != 119 || failure.Failures != 2 || failure.FirstError != "snapshot" {
		t.Fatalf("restored failure record = %+v", failure)
	}
	if n := countRows(t, live, `SELECT count(*) FROM deferred_migration_failures`); n != 1 {
		t.Fatalf("the restore kept the live failure record beside the snapshot's: %d rows", n)
	}
}
