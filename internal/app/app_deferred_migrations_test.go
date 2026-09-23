package app

import (
	"database/sql"
	"testing"
	"time"
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
