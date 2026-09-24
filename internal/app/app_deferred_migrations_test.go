package app

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
)

// seedPendingHistoryRepair leaves the database as an upgrade from before
// v119 does: one sealed chunk holding one assistant row, a payload row
// nothing references, and a deferred migration watermark below v119. No
// store accessor writes any of them, so they go in through a second handle on
// the file.
func seedPendingHistoryRepair(t *testing.T, app *App, dbPath, threadID string) {
	t.Helper()
	if err := app.store.CreateThread(testThread(threadID)); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, statement := range []string{
		`INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES('sealed:app',1,0,0)`,
		`INSERT INTO import_history_payloads(chunk_id,id,kind,meta,data,created_at) VALUES('sealed:app','p1','text','{}',CAST('sealed bytes' AS BLOB),1)`,
		`INSERT INTO import_history_items(chunk_id,id,turn_index,item_index,kind,role,status,summary,payload_id,meta,created_at,updated_at)
		 VALUES('sealed:app','i1',0,0,'assistant_text','assistant','completed','sealed row','p1','{}',1,1)`,
		`INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('` + threadID + `',0,'sealed:app')`,
		`INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('` + threadID + `','orphan','text','{}',CAST('leaked' AS BLOB),1)`,
		`PRAGMA user_version = 118`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func deferredMigrationsPending(t *testing.T, app *App) bool {
	t.Helper()
	pending, err := app.store.DeferredMigrationsPending()
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sealedRowFolded(t *testing.T, app *App, threadID string) bool {
	t.Helper()
	item, found, err := app.store.GetThreadItem(threadID, "i1")
	if err != nil || !found {
		t.Fatalf("row i1: found=%v err=%v", found, err)
	}
	return item.Rev >= 0
}

func TestDeferredMigrationsFinishInTheBackground(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	app.maintenance.chunkPause = time.Millisecond
	seedPendingHistoryRepair(t, app, dbPath, "sealed")
	if sealedRowFolded(t, app, "sealed") {
		t.Fatal("the seeded row is not sealed")
	}

	app.startDeferredMigrations()
	waitFor(t, "the deferred migrations", func() bool { return !deferredMigrationsPending(t, app) })
	app.stopDeferredMigrations()

	item, _, err := app.store.GetThreadItem("sealed", "i1")
	if err != nil || item.Rev < 0 || item.Summary != "sealed row" {
		t.Fatalf("folded row = %+v, %v", item, err)
	}
	if data, err := app.store.GetPayloadData("sealed", "p1"); err != nil || string(data) != "sealed bytes" {
		t.Fatalf("folded payload = %q, %v", data, err)
	}
	if _, err := app.store.GetPayloadData("sealed", "orphan"); err == nil {
		t.Fatal("the orphan payload survived the deferred migration")
	}
}

// A stop in the middle of the run returns within the pause, leaves the
// phase pending, and a later start finishes it.
func TestDeferredMigrationsStopWithoutRecording(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	app.maintenance.chunkPause = time.Hour
	seedPendingHistoryRepair(t, app, dbPath, "sealed")

	app.startDeferredMigrations()
	waitFor(t, "the first transaction", func() bool { return sealedRowFolded(t, app, "sealed") })
	stopped := time.Now()
	app.stopDeferredMigrations()
	if elapsed := time.Since(stopped); elapsed > 5*time.Second {
		t.Fatalf("stop waited %s for the pause", elapsed)
	}
	if !deferredMigrationsPending(t, app) {
		t.Fatal("a stopped run recorded the watermark")
	}
	if data, err := app.store.GetPayloadData("sealed", "orphan"); err != nil || string(data) != "leaked" {
		t.Fatalf("orphan = %q, %v; want it left for the next run", data, err)
	}

	app.maintenance.chunkPause = time.Millisecond
	app.startDeferredMigrations()
	waitFor(t, "the resumed run", func() bool { return !deferredMigrationsPending(t, app) })
	app.stopDeferredMigrations()
	if _, err := app.store.GetPayloadData("sealed", "orphan"); err == nil {
		t.Fatal("the resumed run did not prune the orphan")
	}
}

func TestDeferredMigrationsStartNothingWhenNoneArePending(t *testing.T) {
	app := newTestAppWithStore(t)
	if deferredMigrationsPending(t, app) {
		t.Fatal("a new database has a pending deferred migration")
	}
	app.startDeferredMigrations()
	app.deferredMigrations.mu.Lock()
	started := app.deferredMigrations.stop != nil
	app.deferredMigrations.mu.Unlock()
	if started {
		t.Fatal("a database with nothing pending started a run")
	}
}

func execOnFile(t *testing.T, dbPath, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatalf("%q: %v", statement, err)
	}
}

func waitForSends(t *testing.T, recorder *recordingNotificationSender, n int) []notify.Send {
	t.Helper()
	waitFor(t, "the deferred migration notice", func() bool { return len(recorder.snapshot()) >= n })
	return recorder.snapshot()
}

// One failing item leaves the watermark, records the count and first error
// durably and raises the notice. The next start, with the fault gone,
// retries it, advances the watermark, clears the record and takes the
// notice back.
func TestDeferredMigrationFailureNoticesAndTheNextStartRetries(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	recorder := &recordingNotificationSender{}
	app.osNotifications = recorder
	app.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	app.maintenance.chunkPause = time.Millisecond
	seedPendingHistoryRepair(t, app, dbPath, "sealed")
	execOnFile(t, dbPath, `CREATE TRIGGER fail_fold BEFORE INSERT ON items WHEN NEW.thread_id = 'sealed' BEGIN SELECT RAISE(ABORT, 'injected fault'); END`)

	app.startDeferredMigrations()
	sends := waitForSends(t, recorder, 1)
	app.stopDeferredMigrations()

	if !deferredMigrationsPending(t, app) {
		t.Fatal("a run with a failed item advanced the watermark")
	}
	failure, err := app.store.DeferredMigrationFailure()
	if err != nil || failure == nil || failure.Failures != 1 || !strings.Contains(failure.FirstError, "injected fault") {
		t.Fatalf("recorded failure = %+v, %v", failure, err)
	}
	if len(sends) != 1 {
		t.Fatalf("sends = %#v, want one notice", sends)
	}
	notice := sends[0]
	if notice.ID != deferredMigrationNoticeID || notice.Kind != notify.KindAppUpdate || notice.Retract ||
		notice.Title != "History repair incomplete" ||
		!strings.HasPrefix(notice.Body, "1 item failed; retrying on next start.") ||
		!strings.Contains(notice.Body, "injected fault") ||
		notice.Target != (notify.Target{Kind: notify.TargetNone, BackendID: "backend-under-test"}) {
		t.Fatalf("notice = %#v", notice)
	}
	if sealedRowFolded(t, app, "sealed") {
		t.Fatal("the failing thread's row folded despite the fault")
	}

	execOnFile(t, dbPath, `DROP TRIGGER fail_fold`)
	app.startDeferredMigrations()
	sends = waitForSends(t, recorder, 2)
	app.stopDeferredMigrations()

	if deferredMigrationsPending(t, app) {
		t.Fatal("the retry did not advance the watermark")
	}
	if failure, err := app.store.DeferredMigrationFailure(); err != nil || failure != nil {
		t.Fatalf("failure record after the retry = %+v, %v", failure, err)
	}
	if !sealedRowFolded(t, app, "sealed") {
		t.Fatal("the retry did not fold the row")
	}
	if len(sends) != 2 || sends[1] != (notify.Send{ID: deferredMigrationNoticeID, Kind: notify.KindAppUpdate, Retract: true}) {
		t.Fatalf("sends after the retry = %#v, want the notice retracted", sends)
	}
}

// A clean run with no earlier failure raises nothing.
func TestDeferredMigrationsCleanRunIsSilent(t *testing.T) {
	app, dbPath := newTestAppWithStorePath(t)
	recorder := &recordingNotificationSender{}
	app.osNotifications = recorder
	app.maintenance.chunkPause = time.Millisecond
	seedPendingHistoryRepair(t, app, dbPath, "sealed")

	app.startDeferredMigrations()
	waitFor(t, "the deferred migrations", func() bool { return !deferredMigrationsPending(t, app) })
	app.stopDeferredMigrations()
	if sends := recorder.snapshot(); len(sends) != 0 {
		t.Fatalf("a clean run sent %#v", sends)
	}
}

// A run that could not read or record its progress raises the notice with
// that error, and the preference toggle silences it.
func TestDeferredMigrationRunErrorNotices(t *testing.T) {
	app := newTestAppWithStore(t)
	recorder := &recordingNotificationSender{}
	app.osNotifications = recorder

	app.reportDeferredMigrations(false, errors.New("record watermark: disk I/O error"))
	sends := recorder.snapshot()
	if len(sends) != 1 || sends[0].Title != "Database maintenance incomplete" ||
		sends[0].Body != "record watermark: disk I/O error. Retrying on next start." || sends[0].Kind != notify.KindAppUpdate {
		t.Fatalf("sends = %#v", sends)
	}

	if _, err := app.settings.Update(map[string]any{"notifyAppUpdate": false}); err != nil {
		t.Fatal(err)
	}
	app.reportDeferredMigrations(false, errors.New("again"))
	if sends := recorder.snapshot(); len(sends) != 1 {
		t.Fatalf("a silenced kind still sent: %#v", sends)
	}
}
