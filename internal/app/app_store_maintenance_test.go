package app

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// The auto_vacuum conversion's wait exists to pick a moment the user cannot
// notice. These tests pin what decides it: the activation gate, no live
// turn, a full quiet window with no commits, and at most one attempt an
// hour.

func TestCommitQuietGateNeedsAFullWindowWithoutCommits(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	gate := newCommitQuietGate(base)
	const window = time.Minute

	// The first sample only establishes the baseline.
	if gate.observe(7, base, window) {
		t.Fatal("the first data_version sample cannot prove a quiet window")
	}
	// A changed version is a commit: the window restarts from there.
	if gate.observe(8, base.Add(30*time.Second), window) {
		t.Fatal("a commit must restart the quiet window")
	}
	if gate.observe(8, base.Add(80*time.Second), window) {
		t.Fatal("only 50s of quiet so far")
	}
	if !gate.observe(8, base.Add(95*time.Second), window) {
		t.Fatal("65s without a commit is a quiet window")
	}
	// Activity that is not a commit still disturbs the window.
	gate.disturb(base.Add(95 * time.Second))
	if gate.observe(8, base.Add(120*time.Second), window) {
		t.Fatal("a live turn must restart the quiet window")
	}
}

func TestCommitQuietGateCapsAttemptsPerHour(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	gate := newCommitQuietGate(base)

	if !gate.attemptDue(base, time.Hour) {
		t.Fatal("the first attempt is always due")
	}
	gate.attempted(base)
	if gate.attemptDue(base.Add(59*time.Minute), time.Hour) {
		t.Fatal("a second attempt inside the hour must be refused")
	}
	if !gate.attemptDue(base.Add(time.Hour), time.Hour) {
		t.Fatal("an attempt is due again after the interval")
	}
}

// TestStoreSwapReadyWaitsForQuietAndCapsAttempts drives the real sample
// against the test store: an attempt is allowed only after the quiet
// window, and the next one only after the retry interval.
func TestStoreSwapReadyWaitsForQuietAndCapsAttempts(t *testing.T) {
	app := retentionTestApp(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.maintenance.quietWindow = time.Minute
	gate := newCommitQuietGate(base)
	defer gate.close()

	sample := func(at time.Duration) bool {
		t.Helper()
		ready, err := app.storeSwapReady(ctx, gate, base.Add(at))
		if err != nil {
			t.Fatalf("sample at %s: %v", at, err)
		}
		return ready
	}
	if sample(0) {
		t.Fatal("the first sample only establishes the baseline")
	}
	// A commit restarts the window.
	if err := app.store.SetUIState("client:test", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if sample(30 * time.Second) {
		t.Fatal("a sample right after a commit must not allow an attempt")
	}
	if !sample(2 * time.Minute) {
		t.Fatal("a quiet sample past the window must allow the attempt")
	}
	if sample(3 * time.Minute) {
		t.Fatal("a second attempt inside the retry interval must be refused")
	}
	if !sample(2*time.Minute + storeConvertRetryInterval) {
		t.Fatal("an attempt is due again after the retry interval")
	}
}

func TestStoreSwapReadyTreatsALiveTurnAsActivity(t *testing.T) {
	app := retentionTestApp(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.maintenance.quietWindow = time.Minute
	gate := newCommitQuietGate(base)
	defer gate.close()

	liveness := newSessionLiveness(time.Now())
	liveness.ActiveTurns.Store(1)
	app.sessionManager().put("live-thread", session{Liveness: liveness})
	defer app.sessionManager().take("live-thread")

	ready, err := app.storeSwapReady(context.Background(), gate, base.Add(2*time.Minute))
	if err != nil || ready {
		t.Fatalf("a live turn produced ready = %v, err = %v", ready, err)
	}
	if gate.watch != nil {
		t.Fatal("a live turn must not open the commit watcher")
	}
	if !gate.lastAttempt.IsZero() {
		t.Fatal("a live turn must not consume the hourly attempt")
	}
}

// useLegacyStore replaces the fixture's store with one whose file predates
// incremental auto-vacuum and whose deferred watermark is below v119, as an
// upgraded install's is.
func useLegacyStore(t *testing.T, app *App) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	var journal string
	if err := raw.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE legacy_seed (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(path)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if mode, err := st.AutoVacuumMode(); err != nil || mode != store.AutoVacuumNone {
		t.Fatalf("legacy store auto_vacuum = %v (%v), want none", mode, err)
	}
	app.store = st
	setDeferredWatermark(t, path, 118)
}

// setDeferredWatermark writes the deferred phase watermark through a second
// handle on the file; no store accessor moves it backwards.
func setDeferredWatermark(t *testing.T, path string, version int) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		t.Fatal(err)
	}
}

func storeAutoVacuum(t *testing.T, app *App) store.AutoVacuumMode {
	t.Helper()
	mode, err := app.store.AutoVacuumMode()
	if err != nil {
		t.Fatal(err)
	}
	return mode
}

// A supervisor trial never replaces its database file: the conversion waits
// on the activation gate, and converts once the gate opens.
func TestDeferredConversionWaitsForActivation(t *testing.T) {
	app := retentionTestApp(t)
	useLegacyStore(t, app)
	app.maintenance.chunkPause = time.Millisecond
	app.maintenance.convertPoll = time.Millisecond
	app.maintenance.quietWindow = 10 * time.Millisecond
	app.activation.Park()

	app.startDeferredMigrations()
	time.Sleep(300 * time.Millisecond)
	if mode := storeAutoVacuum(t, app); mode != store.AutoVacuumNone {
		t.Fatalf("a parked backend converted its database (auto_vacuum = %v)", mode)
	}
	if !deferredMigrationsPending(t, app) {
		t.Fatal("the phase finished while the conversion was parked")
	}

	app.activation.Open()
	waitFor(t, "the conversion after activation", func() bool { return !deferredMigrationsPending(t, app) })
	app.stopDeferredMigrations()
	if mode := storeAutoVacuum(t, app); mode != store.AutoVacuumIncremental {
		t.Fatalf("auto_vacuum = %v after activation, want incremental", mode)
	}
	if failure, err := app.store.DeferredMigrationFailure(); err != nil || failure != nil {
		t.Fatalf("failure record = %+v, %v", failure, err)
	}
}

// A quit while the conversion waits returns promptly and records nothing,
// so the next launch runs the step again.
func TestDeferredConversionWaitStopsOnQuit(t *testing.T) {
	app := retentionTestApp(t)
	useLegacyStore(t, app)
	app.maintenance.chunkPause = time.Millisecond
	app.maintenance.convertPoll = time.Millisecond
	app.maintenance.quietWindow = time.Hour

	app.startDeferredMigrations()
	time.Sleep(100 * time.Millisecond)
	stopped := time.Now()
	app.stopDeferredMigrations()
	if elapsed := time.Since(stopped); elapsed > 5*time.Second {
		t.Fatalf("stop waited %s for the conversion wait", elapsed)
	}
	if !deferredMigrationsPending(t, app) {
		t.Fatal("a quit moved the watermark")
	}
	if failure, err := app.store.DeferredMigrationFailure(); err != nil || failure != nil {
		t.Fatalf("a quit recorded a failure: %+v, %v", failure, err)
	}
	if mode := storeAutoVacuum(t, app); mode != store.AutoVacuumNone {
		t.Fatalf("auto_vacuum = %v after a quit, want none", mode)
	}
}

func TestDeferredMigrationsStartStopRoundTrip(t *testing.T) {
	app := retentionTestApp(t)
	useLegacyStore(t, app)
	app.maintenance.convertPoll = time.Millisecond
	app.maintenance.quietWindow = time.Hour

	app.startDeferredMigrations()
	// Idempotent: a second start must not fan out a goroutine.
	app.startDeferredMigrations()
	app.stopDeferredMigrations()
	// Idempotent: a second stop must not panic on the closed channel.
	app.stopDeferredMigrations()
	// Restart after stop must work.
	app.startDeferredMigrations()
	app.stopDeferredMigrations()
}
