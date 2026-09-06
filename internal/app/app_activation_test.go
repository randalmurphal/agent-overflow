package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/supervise"
)

// The zero value is OPEN, and that is what keeps every non-trial boot
// byte-for-byte unchanged: Run executes inline, in the caller's own goroutine,
// and its error is still the boot's error.
func TestTheZeroActivationGateRunsWorkInlineAndPropagatesItsError(t *testing.T) {
	var gate activation
	if gate.Parked() {
		t.Fatal("the zero value is parked, so every ordinary boot would defer its startup work")
	}

	ran := false
	if err := gate.Run(context.Background(), func() error { ran = true; return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ran {
		t.Fatal("Run did not execute the work")
	}

	// Inline means the caller sees the failure. A boot whose scheduler will not
	// start must still fail the boot.
	want := errors.New("the scheduler would not start")
	if got := gate.Run(context.Background(), func() error { return want }); !errors.Is(got, want) {
		t.Fatalf("Run returned %v, want the work's own error", got)
	}
}

// Parked: the work is deferred, not skipped, and nothing runs until the commit
// arrives. That is the whole property a rollback rests on — a trial that fetched,
// swept or spent would have done something no snapshot can undo.
func TestAParkedGateDefersWorkUntilItOpens(t *testing.T) {
	var gate activation
	gate.Park()
	if !gate.Parked() {
		t.Fatal("Park did not close the gate")
	}

	started := make(chan struct{})
	if err := gate.Run(context.Background(), func() error { close(started); return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-started:
		t.Fatal("parked work ran before the gate opened")
	case <-time.After(50 * time.Millisecond):
	}

	gate.Open()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the work did not run after the gate opened")
	}
	if gate.Parked() {
		t.Fatal("Open did not open the gate")
	}
	// A commit that arrives twice, or a shutdown opening the gate to release a
	// waiter, must not panic on a second close.
	gate.Open()
}

// A rolled-back trial is stopped rather than committed, so its parked work must
// never run — and the goroutine holding it must not outlive the process either.
func TestAParkedGateReleasesItsWaiterOnShutdownWithoutRunningTheWork(t *testing.T) {
	var gate activation
	gate.Park()
	ctx, cancel := context.WithCancel(context.Background())

	ran := make(chan struct{})
	if err := gate.Run(ctx, func() error { close(ran); return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	select {
	case <-ran:
		t.Fatal("a cancelled trial still ran its unattended work")
	case <-time.After(100 * time.Millisecond):
	}
}

// With no supervisor there is no update path, and the refusal names what to do
// instead rather than what field was missing.
func TestServiceUpdateRequestRefusesWithoutASupervisor(t *testing.T) {
	app := &App{}
	if _, err := app.serviceUpdateRequest("2.0.0"); !errors.Is(err, errNoSupervisor) {
		t.Fatalf("serviceUpdateRequest = %v, want errNoSupervisor", err)
	}
	if !strings.Contains(errNoSupervisor.Error(), "service install") {
		t.Errorf("the refusal does not name the remedy: %v", errNoSupervisor)
	}

	SetServiceUpdateRequester(app, func(target string) (string, error) {
		return "upd-" + target, nil
	})
	id, err := app.serviceUpdateRequest("2.0.0")
	if err != nil || id != "upd-2.0.0" {
		t.Fatalf("serviceUpdateRequest = (%q, %v)", id, err)
	}
}

// internal/supervise snapshots and restores this app's database while no
// process holds it, and it cannot import this package to learn the name. It
// restates the triple; this is where the two are held together. A rename that
// missed the restatement would leave the supervisor snapshotting a file nobody
// writes and restoring nothing over a trial's work.
func TestSuperviseSnapshotsTheDatabaseFilesThisPackageOpens(t *testing.T) {
	want := []string{databaseFileName, databaseFileName + "-wal", databaseFileName + "-shm"}
	got := supervise.DatabaseFiles()
	if len(got) != len(want) {
		t.Fatalf("supervise.DatabaseFiles() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("supervise.DatabaseFiles()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestClientAdmissionFollowsTrialCommit(t *testing.T) {
	app := &App{}
	if err := WaitForActivation(app, context.Background()); err != nil {
		t.Fatal("ordinary boot refused clients", err)
	}
	ParkUnattendedWork(app)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitForActivation(app, ctx); err != context.Canceled {
		t.Fatal("trial admitted clients", err)
	}
	ActivateUnattendedWork(app, ServiceUpdateOutcome{})
	if err := WaitForActivation(app, context.Background()); err != nil {
		t.Fatal("committed trial refused clients", err)
	}
}
