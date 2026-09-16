package app

import (
	"testing"
	"time"
)

// The conversion scheduler exists to pick a moment the user cannot
// notice. These tests pin the two predicates that decide it: a full
// quiet window with no commits, and at most one attempt an hour.

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

// TestStoreConversionTickWaitsForQuietThenFinishes drives the real tick
// against the test store, which this build already creates as an
// incremental database: the scheduler must reach the attempt only after
// the quiet window, and then stop for good.
func TestStoreConversionTickWaitsForQuietThenFinishes(t *testing.T) {
	app := retentionTestApp(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.maintenance.quietWindow = time.Minute
	gate := newCommitQuietGate(base)
	defer gate.close()

	if done := app.storeConversionTick(gate, base); done {
		t.Fatal("the first tick only establishes the baseline")
	}
	// A commit restarts the window.
	if err := app.store.SetUIState("client:test", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if done := app.storeConversionTick(gate, base.Add(30*time.Second)); done {
		t.Fatal("a tick right after a commit must not attempt the conversion")
	}
	// Quiet from here on: the tick past the window attempts, finds the
	// database already incremental and stops the scheduler.
	if done := app.storeConversionTick(gate, base.Add(2*time.Minute)); !done {
		t.Fatal("a quiet tick on an incremental database must finish the scheduler")
	}
}

func TestStoreConversionTickTreatsALiveTurnAsActivity(t *testing.T) {
	app := retentionTestApp(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	app.maintenance.quietWindow = time.Minute
	gate := newCommitQuietGate(base)
	defer gate.close()

	liveness := newSessionLiveness(time.Now())
	liveness.ActiveTurns.Store(1)
	app.sessionManager().put("live-thread", session{Liveness: liveness})
	defer app.sessionManager().take("live-thread")

	if done := app.storeConversionTick(gate, base.Add(2*time.Minute)); done {
		t.Fatal("a live turn must not produce an attempt")
	}
	if gate.watch != nil {
		t.Fatal("a live turn must not open the commit watcher")
	}
	if !gate.lastAttempt.IsZero() {
		t.Fatal("a live turn must not consume the hourly attempt")
	}
}

func TestStoreMaintenanceLoopExitsOnAnIncrementalDatabase(t *testing.T) {
	app := retentionTestApp(t)
	app.maintenance.convertPoll = time.Millisecond

	done := make(chan struct{})
	go func() {
		app.runStoreConversionLoop(make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler must stop immediately on an incremental database")
	}
}

func TestStartStopStoreMaintenanceRoundTrip(t *testing.T) {
	app := retentionTestApp(t)
	app.maintenance.convertPoll = time.Millisecond

	app.startStoreMaintenance()
	// Idempotent: a second start must not fan out a goroutine.
	app.startStoreMaintenance()
	app.stopStoreMaintenance()
	// Idempotent: a second stop must not panic on the closed channel.
	app.stopStoreMaintenance()
	// Restart after stop must work.
	app.startStoreMaintenance()
	app.stopStoreMaintenance()
}
