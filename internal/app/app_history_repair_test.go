package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"agent-overflow/internal/settings"
)

// seedSealedHistory writes one sealed chunk holding one assistant row and an
// unreferenced payload row. No store accessor writes either any more, so they
// go in through a second handle on the file.
func seedSealedHistory(t *testing.T, app *App, dbPath, threadID string) {
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
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func historyRepairTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	app.configDir = t.TempDir()
	app.settings = settings.NewService(app.configDir)
	app.maintenance.chunkPause = time.Millisecond
	if _, err := app.settings.Update(map[string]any{"retention": map[string]any{"days": 0}}); err != nil {
		t.Fatal(err)
	}
	return app, dbPath
}

func TestRetentionSweepRepairsHistoryBeforeReclaim(t *testing.T) {
	app, dbPath := historyRepairTestApp(t)
	seedSealedHistory(t, app, dbPath, "sealed")
	if item, found, err := app.store.GetThreadItem("sealed", "i1"); err != nil || !found || item.Rev != -1 {
		t.Fatalf("seeded row = %+v found=%v err=%v; want an imported row", item, found, err)
	}

	reclaims := 0
	app.reclaimFreeSpaceFn = func(ctx context.Context, _ time.Duration) (int64, error) {
		reclaims++
		threads, err := app.store.SealedHistoryThreads(ctx)
		if err != nil || len(threads) != 0 {
			t.Errorf("reclaim ran before the repair: sealed threads %v, %v", threads, err)
		}
		orphans, err := app.store.CountOrphanPayloads(ctx)
		if err != nil || orphans.Payloads != 0 {
			t.Errorf("reclaim ran before the orphan prune: %+v, %v", orphans, err)
		}
		return 0, nil
	}
	app.runRetentionSweep(time.Now())

	if reclaims != 1 {
		t.Fatalf("reclaim ran %d times, want 1", reclaims)
	}
	item, found, err := app.store.GetThreadItem("sealed", "i1")
	if err != nil || !found || item.Rev < 0 || item.Summary != "sealed row" {
		t.Fatalf("repaired row = %+v found=%v err=%v", item, found, err)
	}
	if data, err := app.store.GetPayloadData("sealed", "p1"); err != nil || string(data) != "sealed bytes" {
		t.Fatalf("repaired payload = %q, %v", data, err)
	}
	if _, err := app.store.GetPayloadData("sealed", "orphan"); err == nil {
		t.Fatal("orphan payload survived the sweep")
	}
}

func TestHistoryRepairSkippedWhileShuttingDown(t *testing.T) {
	app, dbPath := historyRepairTestApp(t)
	seedSealedHistory(t, app, dbPath, "sealed")
	app.shuttingDown.Store(true)

	app.repairStoredHistory()

	threads, err := app.store.SealedHistoryThreads(context.Background())
	if err != nil || len(threads) != 1 {
		t.Fatalf("sealed threads = %v, %v; want the repair skipped", threads, err)
	}
	if data, err := app.store.GetPayloadData("sealed", "orphan"); err != nil || string(data) != "leaked" {
		t.Fatalf("orphan = %q, %v; want the prune skipped", data, err)
	}
}

func TestHistoryRepairStopsWhenAppContextEnds(t *testing.T) {
	app, dbPath := historyRepairTestApp(t)
	seedSealedHistory(t, app, dbPath, "sealed")
	app.appCancel()

	app.repairStoredHistory()

	threads, err := app.store.SealedHistoryThreads(context.Background())
	if err != nil || len(threads) != 1 {
		t.Fatalf("sealed threads = %v, %v; want no repair after cancellation", threads, err)
	}
	if data, err := app.store.GetPayloadData("sealed", "orphan"); err != nil || string(data) != "leaked" {
		t.Fatalf("orphan = %q, %v; want no prune after cancellation", data, err)
	}
}
