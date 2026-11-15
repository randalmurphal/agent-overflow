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
	t.Parallel()
	var gate activation
	if gate.Parked() {
		t.Fatal("the zero value is parked, so every ordinary boot would defer its startup work")
	}

	var order []string
	step := func(name string) func() error {
		return func() error { order = append(order, name); return nil }
	}
	if err := gate.Run(context.Background(), step("before clients"), step("work"), noReport(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(order, ", ") != "before clients, work" {
		t.Fatalf("Run ran %v, want both halves in order", order)
	}

	// Inline means the caller sees the failure. A boot whose scheduler will not
	// start must still fail the boot, and a failed first half stops the second.
	want := errors.New("the scheduler would not start")
	if got := gate.Run(context.Background(), step("before clients"), func() error { return want }, noReport(t)); !errors.Is(got, want) {
		t.Fatalf("Run returned %v, want the work's own error", got)
	}
	order = nil
	if got := gate.Run(context.Background(), func() error { return want }, step("work"), noReport(t)); !errors.Is(got, want) || len(order) != 0 {
		t.Fatalf("Run returned %v and ran %v, want the first half's error and no work", got, order)
	}
}

// noReport fails a test whose Run reports an error.
func noReport(t *testing.T) func(error) {
	return func(err error) { t.Errorf("Run reported %v", err) }
}

// Parked: the work is deferred, not skipped, and nothing runs until the commit
// arrives. That is the whole property a rollback rests on — a trial that fetched,
// swept or spent would have done something no snapshot can undo.
func TestAParkedGateDefersWorkUntilItOpens(t *testing.T) {
	t.Parallel()
	var gate activation
	gate.Park()
	if !gate.Parked() {
		t.Fatal("Park did not close the gate")
	}

	started := make(chan struct{})
	if err := gate.Run(context.Background(), func() error { return nil }, func() error { close(started); return nil }, noReport(t)); err != nil {
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
	t.Parallel()
	var gate activation
	gate.Park()
	ctx, cancel := context.WithCancel(context.Background())

	ran := make(chan struct{}, 2)
	step := func() error { ran <- struct{}{}; return nil }
	if err := gate.Run(ctx, step, step, noReport(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	select {
	case <-ran:
		t.Fatal("a cancelled trial still ran its deferred boot steps")
	case <-time.After(100 * time.Millisecond):
	}
}

// After a commit, clients enter only once the boot's first half has finished,
// and the unattended half runs only after them. A first half that fails is
// reported, not swallowed, and still admits clients: the trial is committed
// and refusing every request would not undo the failure.
func TestACommittedTrialAdmitsClientsAfterTheStepsThatPrecedeThem(t *testing.T) {
	t.Parallel()
	app := &App{}
	ParkUnattendedWork(app)
	release := make(chan struct{})
	beforeStarted, workRan := make(chan struct{}), make(chan struct{})
	reported := make(chan error, 2)
	failed := errors.New("a sweep failed")
	if err := app.activation.Run(context.Background(), func() error {
		close(beforeStarted)
		<-release
		return failed
	}, func() error { close(workRan); return nil }, func(err error) { reported <- err }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	admitted := make(chan error, 1)
	go func() { admitted <- WaitForActivation(app, context.Background()) }()
	ActivateUnattendedWork(app, ServiceUpdateOutcome{})
	<-beforeStarted
	select {
	case err := <-admitted:
		t.Fatalf("a client was admitted (%v) before the steps that precede clients finished", err)
	case <-workRan:
		t.Fatal("the unattended work ran before clients were admitted")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-admitted; err != nil {
		t.Fatalf("WaitForActivation after the commit: %v", err)
	}
	<-workRan
	if err := <-reported; !errors.Is(err, failed) {
		t.Fatalf("reported %v, want the first half's failure", err)
	}
}

// With no supervisor there is no update path, and the refusal names what to do
// instead rather than what field was missing.
func TestServiceUpdateRequestRefusesWithoutASupervisor(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
