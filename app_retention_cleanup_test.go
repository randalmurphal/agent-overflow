package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/settings"
	"agent-overflow/internal/uitrace"
)

// retentionTestApp wraps newTestAppWithStore with a settings service
// and configDir on a t.TempDir(). The retention sweep needs both: it
// reads Retention.Days from settings and uses configDir to locate the
// log + bookmark directories. deleteThreadTreeLocked is heavy and
// touches `a.replay`, `a.terminals`, etc., so we install a no-op
// stopSessionFn (Codex test threads with no live session don't need
// any of the other subsystems either).
func retentionTestApp(t *testing.T) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	app.settings = settings.NewService(app.configDir)
	app.stopSessionFn = func(string) error { return nil }
	return app
}

func seedThread(t *testing.T, app *App, id string, updatedAt int64) {
	t.Helper()
	thr := testThread(id)
	thr.UpdatedAt = updatedAt
	thr.CreatedAt = updatedAt
	if err := app.store.CreateThread(thr); err != nil {
		t.Fatalf("seed thread %s: %v", id, err)
	}
}

func TestRunRetentionSweepEvictsOnlyOlderThanCutoff(t *testing.T) {
	app := retentionTestApp(t)

	// Configure 30-day retention.
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 30},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.retentionNowFn = func() time.Time { return now }

	// Seed threads spanning the cutoff (30 days before 'now' = May 2).
	cutoff := now.Add(-30 * 24 * time.Hour).UnixMilli()
	seedThread(t, app, "ancient", cutoff-100_000_000)               // ~28h before cutoff
	seedThread(t, app, "stale", cutoff-1_000)                       // 1s before cutoff
	seedThread(t, app, "boundary", cutoff)                          // exactly at cutoff (NOT eligible: strict <)
	seedThread(t, app, "fresh", now.Add(-time.Hour).UnixMilli())    // an hour ago
	seedThread(t, app, "newest", now.Add(-time.Minute).UnixMilli()) // a minute ago

	app.runRetentionSweep(now)

	for _, gone := range []string{"ancient", "stale"} {
		if _, err := app.store.GetThread(gone); err == nil {
			t.Errorf("%s still present after sweep, expected deletion", gone)
		}
	}
	for _, kept := range []string{"boundary", "fresh", "newest"} {
		if _, err := app.store.GetThread(kept); err != nil {
			t.Errorf("%s missing after sweep, expected preservation: %v", kept, err)
		}
	}
}

func TestRunRetentionSweepDisabledWhenDaysZero(t *testing.T) {
	app := retentionTestApp(t)
	// Default Settings has Days=30. Override to 0.
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 0},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.retentionNowFn = func() time.Time { return now }

	seedThread(t, app, "obviously-stale", 0)

	app.runRetentionSweep(now)

	if _, err := app.store.GetThread("obviously-stale"); err != nil {
		t.Fatalf("retention.days=0 should not delete anything: %v", err)
	}
}

func TestRunRetentionSweepHandlesMissingConfigDir(t *testing.T) {
	app := retentionTestApp(t)
	app.configDir = "" // skip log/bookmark prune entirely
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 30},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.retentionNowFn = func() time.Time { return now }
	seedThread(t, app, "stale", now.Add(-365*24*time.Hour).UnixMilli())

	app.runRetentionSweep(now)

	if _, err := app.store.GetThread("stale"); err == nil {
		t.Fatal("thread should be deleted even with empty configDir")
	}
}

func TestRunRetentionSweepPrunesLogsAndBookmarks(t *testing.T) {
	app := retentionTestApp(t)
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 7},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.retentionNowFn = func() time.Time { return now }

	logsDir := filepath.Join(app.configDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	bookmarksDir := filepath.Join(app.configDir, uitrace.DirName, uitrace.BookmarkSubdir)
	if err := os.MkdirAll(bookmarksDir, 0o700); err != nil {
		t.Fatalf("mkdir bookmarks: %v", err)
	}

	oldMtime := now.Add(-30 * 24 * time.Hour)
	freshMtime := now.Add(-1 * time.Hour)

	files := []struct {
		path     string
		mtime    time.Time
		wantGone bool
	}{
		{filepath.Join(logsDir, "provider-events-2026-01-01.ndjson"), oldMtime, true},
		{filepath.Join(logsDir, "provider-events-2026-01-01.ndjson.1"), oldMtime, true},
		{filepath.Join(logsDir, "provider-events-2026-05-30.ndjson"), freshMtime, false},
		{filepath.Join(bookmarksDir, "bug-report-20260101T000000Z.jsonl"), oldMtime, true},
		{filepath.Join(bookmarksDir, "bug-report-20260530T120000Z.jsonl"), freshMtime, false},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", f.path, err)
		}
		if err := os.Chtimes(f.path, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f.path, err)
		}
	}

	app.runRetentionSweep(now)

	for _, f := range files {
		_, err := os.Stat(f.path)
		switch {
		case f.wantGone && err == nil:
			t.Errorf("%s still present, expected removal", f.path)
		case !f.wantGone && err != nil:
			t.Errorf("%s missing, expected preservation: %v", f.path, err)
		}
	}
}

func TestRunRetentionSweepNoSettingsServiceIsNoOp(t *testing.T) {
	app := newTestAppWithStore(t)
	app.stopSessionFn = func(string) error { return nil }
	// The fixture wires a settings service; this test pins the nil-service
	// guard, so drop it explicitly.
	app.settings = nil
	seedThread(t, app, "stale", 0)

	app.runRetentionSweep(time.Now())

	if _, err := app.store.GetThread("stale"); err != nil {
		t.Fatalf("sweep without settings service must be a no-op: %v", err)
	}
}

func TestStartStopRetentionCleanupRoundTrip(t *testing.T) {
	app := retentionTestApp(t)

	app.startRetentionCleanup()
	// Idempotent — must not fan out a second goroutine.
	app.startRetentionCleanup()
	app.stopRetentionCleanup()
	// Idempotent — must not panic on double close.
	app.stopRetentionCleanup()
	// Restart after stop must work.
	app.startRetentionCleanup()
	app.stopRetentionCleanup()
}

func TestStopRetentionCleanupBeforeStart(t *testing.T) {
	app := retentionTestApp(t)
	app.stopRetentionCleanup() // must not panic
}

func TestStartRetentionCleanupExitsOnStop(t *testing.T) {
	app := retentionTestApp(t)
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 30},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}

	app.startRetentionCleanup()

	stopped := make(chan struct{})
	go func() {
		app.stopRetentionCleanup()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stopRetentionCleanup did not return within 2s")
	}
}

func TestRunRetentionThreadSweepIsRaceFreeUnderChurn(t *testing.T) {
	app := retentionTestApp(t)
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 30},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cutoffMs := now.Add(-30 * 24 * time.Hour).UnixMilli()

	const seeded = 20
	for i := 0; i < seeded; i++ {
		seedThread(t, app, fmt.Sprintf("t-%02d", i), cutoffMs-int64(i+1)*1000)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("churn-%d-%d", slot, j)
				thr := testThread(id)
				thr.UpdatedAt = now.UnixMilli() // fresh — won't match cutoff
				thr.CreatedAt = thr.UpdatedAt
				if err := app.store.CreateThread(thr); err != nil {
					// FK conflict on the test project shouldn't happen here;
					// any error indicates a real problem.
					return
				}
				_ = app.store.DeleteThread(id)
			}
		}(i)
	}

	deleted, failed := app.runRetentionThreadSweep(cutoffMs)
	close(stop)
	wg.Wait()

	if failed != 0 {
		t.Fatalf("sweep recorded %d failures under churn", failed)
	}
	if deleted != seeded {
		t.Fatalf("deleted=%d, want %d (all seeded stale threads)", deleted, seeded)
	}
	// Sanity: every seeded thread is gone.
	for i := 0; i < seeded; i++ {
		id := fmt.Sprintf("t-%02d", i)
		if _, err := app.store.GetThread(id); err == nil {
			t.Errorf("%s still present after sweep", id)
		}
	}
}

func TestRunRetentionThreadSweepCancelsOnShutdownFlag(t *testing.T) {
	app := retentionTestApp(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cutoffMs := now.UnixMilli()

	// Seed exactly one stale thread so the loop enters the per-thread
	// branch. We flip shuttingDown BEFORE the call so the first
	// iteration's check trips.
	seedThread(t, app, "stale", 0)
	app.shuttingDown.Store(true)
	defer app.shuttingDown.Store(false)

	deleted, failed := app.runRetentionThreadSweep(cutoffMs)
	if deleted != 0 || failed != 0 {
		t.Fatalf("expected no work with shuttingDown=true, got deleted=%d failed=%d", deleted, failed)
	}
	// Thread must still be present.
	if _, err := app.store.GetThread("stale"); err != nil {
		t.Fatalf("thread should not have been deleted: %v", err)
	}
}

// TestRunRetentionThreadSweepAbortsMidBatchOnShutdown drives enough
// stale threads through the sweep to cross at least one polling
// boundary, then flips shuttingDown from inside stopSessionFn so the
// next boundary trips. Asserts the sweep returns at exactly the
// polling boundary rather than draining the rest.
func TestRunRetentionThreadSweepAbortsMidBatchOnShutdown(t *testing.T) {
	app := retentionTestApp(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cutoffMs := now.UnixMilli()

	const seeded = retentionShutdownCheckEvery*2 + 1 // 101 with current constant
	for i := 0; i < seeded; i++ {
		seedThread(t, app, fmt.Sprintf("mb-%03d", i), 0)
	}

	// Flip shuttingDown from inside deleteThreadTreeLocked (it calls
	// stopSessionFn for any tracked session; we hook the same path
	// even though our test threads have no live session). The flag is
	// then visible to the next polling boundary check at i = N.
	var calls int
	app.stopSessionFn = func(string) error {
		calls++
		if calls == 1 {
			app.shuttingDown.Store(true)
		}
		return nil
	}
	defer app.shuttingDown.Store(false)

	deleted, failed := app.runRetentionThreadSweep(cutoffMs)
	if failed != 0 {
		t.Fatalf("unexpected failures: %d", failed)
	}
	// shuttingDown is set during iteration 0's delete; the poll fires
	// next at i = retentionShutdownCheckEvery, which trips and returns.
	// That means iterations 0..N-1 ran, so deleted == N.
	if deleted != retentionShutdownCheckEvery {
		t.Fatalf("deleted=%d, want %d (polling boundary abort)", deleted, retentionShutdownCheckEvery)
	}
	// And the corresponding number of threads survived. We don't know
	// which specific ids because ThreadIDsOlderThan returns rows in a
	// tie-break order the test doesn't constrain — what matters is the
	// count, which is what the abort contract delivers.
	survived := 0
	for i := 0; i < seeded; i++ {
		id := fmt.Sprintf("mb-%03d", i)
		if _, err := app.store.GetThread(id); err == nil {
			survived++
		}
	}
	if want := seeded - retentionShutdownCheckEvery; survived != want {
		t.Fatalf("survived=%d, want %d (= seeded - polled boundary)", survived, want)
	}
}
