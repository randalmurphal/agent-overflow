//go:build !windows

package wsllauncher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// sleeperCommand is a child that runs until it is killed.
func sleeperCommand() *exec.Cmd { return exec.Command("/bin/sh", "-c", "while :; do sleep 1; done") }

// startApplierProcess starts a stand-in for the launcher applying an update
// and returns it with its reference.
func startApplierProcess(t *testing.T) (*exec.Cmd, supervise.ProcessRef) {
	t.Helper()
	cmd := sleeperCommand()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ref, err := supervise.ProcessRefOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, ref
}

// exitedProcess is a reference to a process that ran and was reaped.
func exitedProcess(t *testing.T) supervise.ProcessRef {
	t.Helper()
	cmd, ref := startApplierProcess(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	return ref
}

func thisProcess(t *testing.T) supervise.ProcessRef {
	t.Helper()
	self, err := supervise.CurrentProcessRef()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// saveWithApplier saves the fixture's record in state and names applier.
func (f *updateFixture) saveWithApplier(state supervise.UpdateState, attempts int, reported bool, applier supervise.ProcessRef) supervise.LauncherRecord {
	f.t.Helper()
	record := f.save(state, attempts, reported)
	record.Applier = &applier
	if err := supervise.SaveLauncherRecord(f.sequence.RecordPath, record); err != nil {
		f.t.Fatal(err)
	}
	return record
}

// waitFor polls cond until it holds or the test's patience ends.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReconcileJoinsAnUpdateWhoseApplierRuns: a launch while the named
// applier runs, including before it counted a trial, joins the update and
// changes nothing, whatever the record's state.
func TestReconcileJoinsAnUpdateWhoseApplierRuns(t *testing.T) {
	for _, c := range []struct {
		name     string
		state    supervise.UpdateState
		attempts int
	}{
		{"before its first trial", supervise.UpdatePending, 0},
		{"during a trial", supervise.UpdatePending, 1},
		{"at the attempt limit", supervise.UpdatePending, supervise.TrialAttemptLimit},
		{"committing", supervise.UpdateCommitted, 1},
		{"settled", supervise.UpdateRolledBack, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			_, applier := startApplierProcess(t)
			f.saveWithApplier(c.state, c.attempts, false, applier)
			decision, err := f.sequence.Reconcile(t.Context(), "previous")
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != ReconcileJoin || decision.Record.Applier == nil || *decision.Record.Applier != applier {
				t.Fatalf("decision = %+v, want a join of %+v", decision, applier)
			}
			f.wantCalls()
			reason := "earlier reason"
			if c.state == supervise.UpdatePending {
				reason = ""
			}
			f.wantRecord(c.state, c.attempts, reason, false)
		})
	}
}

// TestReconcileActsOnAnUpdateWhoseApplierExited: an applier that exited
// left the update interrupted, and the recovery table applies. A record
// whose applier cannot be checked is refused.
func TestReconcileActsOnAnUpdateWhoseApplierExited(t *testing.T) {
	f := newUpdateFixture(t)
	f.saveWithApplier(supervise.UpdatePending, 0, false, exitedProcess(t))
	decision, err := f.sequence.Reconcile(t.Context(), "previous")
	if err != nil || decision.Action != ReconcileLaunch {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	f.wantRecord(supervise.UpdateFailed, 0, "the update was interrupted before its trial started", true)

	f = newUpdateFixture(t)
	f.saveWithApplier(supervise.UpdatePending, 0, false, supervise.ProcessRef{PID: 1})
	if _, err := f.sequence.Reconcile(t.Context(), "previous"); err == nil {
		t.Fatal("reconciled an update whose applier could not be checked")
	}
	f.wantRecord(supervise.UpdatePending, 0, "", false)
}

// TestApplyRefusesAnUpdateAnotherLauncherApplies: only the named applier,
// or any launcher once it exited, runs the update.
func TestApplyRefusesAnUpdateAnotherLauncherApplies(t *testing.T) {
	f := newUpdateFixture(t)
	_, applier := startApplierProcess(t)
	f.saveWithApplier(supervise.UpdatePending, 0, false, applier)
	f.sequence.Self = thisProcess(t)
	if _, err := f.sequence.Apply(t.Context(), "u1"); err == nil {
		t.Fatal("applied an update another running launcher applies")
	}
	f.wantCalls()
	f.wantRecord(supervise.UpdatePending, 0, "", false)

	f.sequence.Self = applier
	if end, err := f.sequence.Apply(t.Context(), "u1"); err != nil || end.State != supervise.UpdateCommitted {
		t.Fatalf("the named applier: end = %+v, %v", end, err)
	}

	f = newUpdateFixture(t)
	f.saveWithApplier(supervise.UpdatePending, 1, false, exitedProcess(t))
	f.sequence.Self = thisProcess(t)
	if end, err := f.sequence.Apply(t.Context(), "u1"); err != nil || end.State != supervise.UpdateCommitted {
		t.Fatalf("after the applier exited: end = %+v, %v", end, err)
	}
}

// joinFixture joins the fixture's update in the background.
type joinFixture struct {
	mu       sync.Mutex
	progress []startupprogress.Progress
	done     chan struct{}
	decision ReconcileDecision
	err      error
}

func (f *updateFixture) join(ctx context.Context, record supervise.LauncherRecord, fingerprint string) *joinFixture {
	j := &joinFixture{done: make(chan struct{})}
	f.sequence.Self = thisProcess(f.t)
	f.sequence.JoinPoll = 5 * time.Millisecond
	f.sequence.Progress = func(p startupprogress.Progress) {
		j.mu.Lock()
		j.progress = append(j.progress, p)
		j.mu.Unlock()
	}
	go func() {
		defer close(j.done)
		j.decision, j.err = f.sequence.Join(ctx, record, fingerprint)
	}()
	return j
}

func (j *joinFixture) relayed() []startupprogress.Progress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]startupprogress.Progress(nil), j.progress...)
}

func (j *joinFixture) wait(t *testing.T) (ReconcileDecision, error) {
	t.Helper()
	select {
	case <-j.done:
		return j.decision, j.err
	case <-time.After(5 * time.Second):
		t.Fatal("the join did not end")
		return ReconcileDecision{}, nil
	}
}

func publish(t *testing.T, recordPath string, p startupprogress.Progress) {
	t.Helper()
	if err := atomicfile.WriteJSON(UpdateProgressPath(recordPath), p); err != nil {
		t.Fatal(err)
	}
}

// TestJoinRelaysProgressUntilTheApplierExits: the joiner names itself for
// the applier, shows each report the applier publishes once, and decides
// only after the applier exits.
func TestJoinRelaysProgressUntilTheApplierExits(t *testing.T) {
	f := newUpdateFixture(t)
	cmd, applier := startApplierProcess(t)
	record := f.saveWithApplier(supervise.UpdatePending, 1, false, applier)
	first := startupprogress.Progress{Phase: "update.trial", Detail: "applying migration 1 of 2", UpdatedAt: 1, UpdatingTo: "2.0.0"}
	second := first
	second.Detail, second.UpdatedAt = "applying migration 2 of 2", 2
	publish(t, f.sequence.RecordPath, first)

	j := f.join(t.Context(), record, "previous")
	waitFor(t, "the first report", func() bool { return len(j.relayed()) == 1 })
	waitFor(t, "the joiner to be named", func() bool {
		running, err := JoinerRunning(f.sequence.RecordPath)
		return err == nil && running
	})
	publish(t, f.sequence.RecordPath, second)
	waitFor(t, "the second report", func() bool { return len(j.relayed()) == 2 })
	time.Sleep(50 * time.Millisecond) // many polls of an unchanged report
	if got := j.relayed(); len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("relayed %+v", got)
	}
	select {
	case <-j.done:
		t.Fatal("the join ended while the applier runs")
	default:
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	decision, err := j.wait(t)
	if err != nil {
		t.Fatal(err)
	}
	// The applier died mid-trial: the table resumes it.
	if decision.Action != ReconcileHandOff {
		t.Fatalf("decision = %+v", decision)
	}
	if _, err := os.Stat(UpdateJoinerPath(f.sequence.RecordPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the joiner file outlived the join: %v", err)
	}
}

// TestJoinRelaunchesALauncherTheCommitReplaced: after a commit, a joiner
// that is not the target runs no backend of its own; the target does.
func TestJoinRelaunchesALauncherTheCommitReplaced(t *testing.T) {
	for _, c := range []struct {
		name        string
		state       supervise.UpdateState
		reported    bool
		fingerprint string
		action      ReconcileAction
	}{
		{"committed, older launcher", supervise.UpdateCommitted, false, "previous", ReconcileRelaunch},
		{"committed and reported, older launcher", supervise.UpdateCommitted, true, "previous", ReconcileRelaunch},
		{"committed, the target", supervise.UpdateCommitted, false, "target", ReconcileLaunch},
		{"rolled back, older launcher", supervise.UpdateRolledBack, false, "previous", ReconcileLaunch},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			record := f.saveWithApplier(c.state, 1, c.reported, exitedProcess(t))
			decision, err := f.join(t.Context(), record, c.fingerprint).wait(t)
			if err != nil || decision.Action != c.action {
				t.Fatalf("decision = %+v, %v; want action %d", decision, err, c.action)
			}
			if c.action == ReconcileRelaunch {
				// Nothing is settled or reported on the stale launcher's word.
				f.wantCalls()
				f.wantRecord(c.state, 1, "earlier reason", c.reported)
			}
		})
	}
}

// TestJoinEndsWithItsContextAndNeedsANamedLauncher.
func TestJoinEndsWithItsContextAndNeedsANamedLauncher(t *testing.T) {
	f := newUpdateFixture(t)
	_, applier := startApplierProcess(t)
	record := f.saveWithApplier(supervise.UpdatePending, 1, false, applier)
	ctx, cancel := context.WithCancel(t.Context())
	j := f.join(ctx, record, "previous")
	waitFor(t, "the joiner to be named", func() bool {
		running, err := JoinerRunning(f.sequence.RecordPath)
		return err == nil && running
	})
	cancel()
	if _, err := j.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if running, err := JoinerRunning(f.sequence.RecordPath); err != nil || running {
		t.Fatalf("JoinerRunning after the join = %v, %v", running, err)
	}

	// The applier still runs: a join that went ahead would wait on it.
	bounded, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	f.sequence.Self = supervise.ProcessRef{}
	if _, err := f.sequence.Join(bounded, record, "previous"); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joined without naming the joiner: %v", err)
	}
	record.Applier = nil
	f.sequence.Self = thisProcess(t)
	if _, err := f.sequence.Join(bounded, record, "previous"); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joined an update without an applier: %v", err)
	}
}

// TestStartApplierNamesTheLauncherInTheRecord: the started launcher is the
// record's applier before StartApplier returns, and one that cannot be
// named does not run.
func TestStartApplierNamesTheLauncherInTheRecord(t *testing.T) {
	f := newUpdateFixture(t)
	f.save(supervise.UpdatePending, 0, false)
	cmd := sleeperCommand()
	if err := StartApplier(cmd, f.sequence.RecordPath, "u1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	want, err := supervise.ProcessRefOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.record().Applier; got == nil || *got != want {
		t.Fatalf("Applier = %+v, want %+v", got, want)
	}

	unnamed := sleeperCommand()
	if err := StartApplier(unnamed, f.sequence.RecordPath, "u2"); err == nil {
		t.Fatal("started an applier for an update without a record")
	}
	if unnamed.ProcessState == nil {
		_ = unnamed.Process.Kill()
		_ = unnamed.Wait()
		t.Fatal("the applier that could not be named was left running")
	}
	if got := f.record().Applier; got == nil || *got != want {
		t.Fatalf("Applier = %+v after the refused start", got)
	}
}

func readPublished(t *testing.T, path string) (startupprogress.Progress, bool) {
	t.Helper()
	var p startupprogress.Progress
	found, err := atomicfile.ReadJSON(path, &p)
	if err != nil {
		t.Fatal(err)
	}
	return p, found
}

// TestProgressPublisherWritesTheLatestReportAtItsInterval.
func TestProgressPublisherWritesTheLatestReportAtItsInterval(t *testing.T) {
	path := UpdateProgressPath(supervise.LauncherRecordPath(t.TempDir(), "prod", "Ubuntu"))
	publisher := NewProgressPublisher(path, time.Second, t.Logf)
	t.Cleanup(func() { _ = publisher.Close() })
	report := func(detail string) startupprogress.Progress {
		p := startupprogress.Progress{Phase: "update.trial", Detail: detail}
		publisher.Report(p)
		return p
	}
	first := report("first")
	waitFor(t, "the first report", func() bool { p, found := readPublished(t, path); return found && p == first })
	report("second")
	time.Sleep(200 * time.Millisecond) // well inside the interval
	if p, _ := readPublished(t, path); p != first {
		t.Fatalf("a report within the interval was written at once: %+v", p)
	}
	latest := report("third")
	waitFor(t, "the latest report", func() bool { p, _ := readPublished(t, path); return p == latest })
}

// TestProgressPublisherCloseRemovesItsFileWithoutWaiting.
func TestProgressPublisherCloseRemovesItsFileWithoutWaiting(t *testing.T) {
	path := UpdateProgressPath(supervise.LauncherRecordPath(t.TempDir(), "prod", "Ubuntu"))
	publisher := NewProgressPublisher(path, time.Hour, t.Logf)
	p := startupprogress.Progress{Phase: "update.commit"}
	publisher.Report(p)
	waitFor(t, "the report", func() bool { _, found := readPublished(t, path); return found })
	publisher.Report(startupprogress.Progress{Phase: "update.commit", Detail: "later"})
	closed := make(chan error, 1)
	go func() { closed <- publisher.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited out the interval")
	}
	if _, found := readPublished(t, path); found {
		t.Fatal("the published progress outlived the publisher")
	}
}

// TestWatchJoinerCallsOnlyWhileAJoinerRuns.
func TestWatchJoinerCallsOnlyWhileAJoinerRuns(t *testing.T) {
	recordPath := supervise.LauncherRecordPath(t.TempDir(), "prod", "Ubuntu")
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchJoiner(t.Context(), recordPath, 5*time.Millisecond, func() bool {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return calls == 3
		}, t.Logf)
	}()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	time.Sleep(50 * time.Millisecond)
	if err := atomicfile.WriteJSON(UpdateJoinerPath(recordPath), exitedProcess(t)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("joined called %d times without a running joiner", n)
	}
	if err := atomicfile.WriteJSON(UpdateJoinerPath(recordPath), thisProcess(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not end when joined returned true")
	}
	if n := count(); n != 3 {
		t.Fatalf("joined called %d times", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		WatchJoiner(ctx, recordPath, 5*time.Millisecond, func() bool { return false }, t.Logf)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived its context")
	}
}
