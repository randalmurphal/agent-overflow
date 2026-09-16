package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	app.maintenance.chunkPause = time.Millisecond
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

// TestRunRetentionThreadSweepAbortsAtTheNextThreadOnShutdown pins the
// abort contract the pacing requires: every iteration costs at least one
// pause, so the shutdown poll runs per thread rather than per batch.
func TestRunRetentionThreadSweepAbortsAtTheNextThreadOnShutdown(t *testing.T) {
	app := retentionTestApp(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cutoffMs := now.UnixMilli()

	const seeded = 5
	for i := 0; i < seeded; i++ {
		seedThread(t, app, fmt.Sprintf("mb-%03d", i), 0)
	}

	// Flip shuttingDown from inside deleteThreadTreeLocked (it calls
	// stopSessionFn for any tracked session; we hook the same path even
	// though our test threads have no live session), so the flag is
	// visible to the next iteration's check.
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
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1 (abort at the next thread boundary)", deleted)
	}
	survived := 0
	for i := 0; i < seeded; i++ {
		if _, err := app.store.GetThread(fmt.Sprintf("mb-%03d", i)); err == nil {
			survived++
		}
	}
	if want := seeded - 1; survived != want {
		t.Fatalf("survived=%d, want %d", survived, want)
	}
}

// TestRunRetentionThreadSweepPacesItsWrites proves the sweep yields
// between write chunks: the store delete calls the pause hook between
// item chunks, and the sweep pauses between threads.
func TestRunRetentionThreadSweepPacesItsWrites(t *testing.T) {
	app := retentionTestApp(t)
	app.maintenance.chunkPause = 20 * time.Millisecond

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cutoffMs := now.UnixMilli()
	seedThread(t, app, "p-1", 0)
	seedThread(t, app, "p-2", 0)

	start := time.Now()
	deleted, failed := app.runRetentionThreadSweep(cutoffMs)
	elapsed := time.Since(start)
	if deleted != 2 || failed != 0 {
		t.Fatalf("deleted=%d failed=%d, want 2/0", deleted, failed)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("sweep of two threads took %s, expected a pause between them", elapsed)
	}
}

func TestRetentionPauseSkippedWhileShuttingDown(t *testing.T) {
	app := retentionTestApp(t)
	app.maintenance.chunkPause = 2 * time.Second
	app.shuttingDown.Store(true)
	defer app.shuttingDown.Store(false)

	start := time.Now()
	app.retentionPause()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("pause waited %s during shutdown; a quit must not wait out the pacing", elapsed)
	}
}

// TestRetentionSettledGate pins what defers the first sweep: uptime and
// live turns, not a fixed timer.
func TestRetentionSettledGate(t *testing.T) {
	app := retentionTestApp(t)
	app.maintenance.settleUptime = time.Minute

	if app.retentionSettled(30 * time.Second) {
		t.Fatal("the first sweep must not run inside the settle window")
	}
	if !app.retentionSettled(2 * time.Minute) {
		t.Fatal("an idle app past the settle window is settled")
	}

	liveness := newSessionLiveness(time.Now())
	liveness.ActiveTurns.Store(1)
	app.sessionManager().put("live-thread", session{Liveness: liveness})
	defer app.sessionManager().take("live-thread")
	if app.retentionSettled(2 * time.Minute) {
		t.Fatal("the first sweep must not run while a turn is live")
	}
	liveness.ActiveTurns.Store(0)
	if !app.retentionSettled(2 * time.Minute) {
		t.Fatal("the sweep is settled once the turn ends")
	}
}

func TestAwaitRetentionSettledReturnsOnStop(t *testing.T) {
	app := retentionTestApp(t)
	app.maintenance.settleUptime = time.Hour
	app.maintenance.settlePoll = time.Millisecond

	stop := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- app.awaitRetentionSettled(stop) }()
	close(stop)
	select {
	case settled := <-done:
		if settled {
			t.Fatal("a stopped gate must not report settled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitRetentionSettled did not return on stop")
	}
}

// TestStartRetentionCleanupWaitsForTheSettledGate drives the real
// goroutine with a shortened gate and an advancing clock.
func TestStartRetentionCleanupWaitsForTheSettledGate(t *testing.T) {
	app := retentionTestApp(t)
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 30},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	app.maintenance.settlePoll = time.Millisecond
	app.maintenance.settleUptime = 10 * time.Millisecond
	app.maintenance.chunkPause = time.Millisecond

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(base.UnixMilli())
	app.retentionNowFn = func() time.Time {
		return time.UnixMilli(clock.Add(5))
	}
	seedThread(t, app, "stale", 0)

	app.startRetentionCleanup()
	defer app.stopRetentionCleanup()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := app.store.GetThread("stale"); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the first sweep never ran after the settle gate opened")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRunRetentionSweepReclaimsFreeSpaceWithRetentionDisabled pins the
// split between the two halves of a tick. Retention.Days is the TTL for
// deletes; the freelist left behind by deletes the user made by hand is
// reclaimed on the same schedule whether or not the TTL is on.
func TestRunRetentionSweepReclaimsFreeSpaceWithRetentionDisabled(t *testing.T) {
	app := retentionTestApp(t)
	if _, err := app.settings.Update(map[string]any{
		"retention": map[string]any{"days": 0},
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.retentionNowFn = func() time.Time { return now }
	seedThread(t, app, "ancient", now.Add(-10*365*24*time.Hour).UnixMilli())

	var calls int
	var gotPause time.Duration
	app.maintenance.chunkPause = 7 * time.Millisecond
	app.reclaimFreeSpaceFn = func(ctx context.Context, pause time.Duration) (int64, error) {
		calls++
		gotPause = pause
		if ctx == nil {
			t.Error("reclaim got a nil context")
		}
		return 12, nil
	}

	app.runRetentionSweep(now)

	if calls != 1 {
		t.Fatalf("reclaim called %d times with retention off, want 1", calls)
	}
	if gotPause != 7*time.Millisecond {
		t.Fatalf("reclaim pause = %s, want the sweep's chunk pause", gotPause)
	}
	if _, err := app.store.GetThread("ancient"); err != nil {
		t.Fatalf("retention is off, so nothing may be deleted: %v", err)
	}
}

// TestReclaimStoreFreeSpaceSkippedWhileShuttingDown keeps the quit free
// of the reclaim loop's pacing.
func TestReclaimStoreFreeSpaceSkippedWhileShuttingDown(t *testing.T) {
	app := retentionTestApp(t)
	var calls int
	app.reclaimFreeSpaceFn = func(context.Context, time.Duration) (int64, error) {
		calls++
		return 0, nil
	}

	app.reclaimStoreFreeSpace()
	if calls != 1 {
		t.Fatalf("reclaim called %d times, want 1", calls)
	}

	app.shuttingDown.Store(true)
	app.reclaimStoreFreeSpace()
	if calls != 1 {
		t.Fatalf("reclaim called %d times, want no call during shutdown", calls)
	}
}
