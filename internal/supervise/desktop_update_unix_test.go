//go:build !windows

package supervise

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"

	"golang.org/x/sys/unix"
)

// desktopRig is a data root, an install folder and a scripted trial for the
// macOS and Linux helper. The platform's file operations are fakes that
// count what they do: Replace defaults to the Linux rename.
type desktopRig struct {
	*commandRig
	apps   string
	lock   *os.File
	schema int

	mu       sync.Mutex
	replaced int
	inUse    map[string]bool
	swap     func(from, to string) error
	logged   []string
	details  []string
}

const desktopTestID = "0123456789abcdef"

func newDesktopRig(t *testing.T) *desktopRig {
	t.Helper()
	r := &desktopRig{commandRig: newCommandRig(t), schema: 7, inUse: map[string]bool{}}
	r.apps = filepath.Join(r.dir, "Applications")
	if err := os.MkdirAll(r.apps, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(r.dataDir, "backend.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	r.lock = lock
	t.Cleanup(func() {
		if t.Failed() {
			r.mu.Lock()
			t.Logf("update log:\n  %s", strings.Join(r.logged, "\n  "))
			r.mu.Unlock()
		}
	})
	return r
}

func (r *desktopRig) files() DesktopFiles {
	return DesktopFiles{
		Replace: func(staged, install, previous string) error {
			r.mu.Lock()
			r.replaced++
			swap := r.swap
			r.mu.Unlock()
			if swap != nil {
				return replaceBundle(staged, install, previous, swap)
			}
			return os.Rename(staged, install)
		},
		InUse: func(path string) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.inUse[path], nil
		},
	}
}

// update is the helper or app of version running executable, whose trial
// runs behavior.
func (r *desktopRig) update(version, executable, install, behavior string) DesktopUpdate {
	opts := r.trialOptions(behavior, 0)
	return DesktopUpdate{
		DataDir: r.dataDir, Version: version, Executable: executable, InstallPath: install, Lock: r.lock,
		Trial:         TrialRunOptions{Binary: opts.Binary, Env: opts.Env, Rule: opts.Rule, StopTimeout: opts.StopTimeout},
		SchemaVersion: func() (int, error) { return r.schema, nil },
		Files:         r.files(),
		Progress: func(p startupprogress.Progress) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.details = append(r.details, p.Detail)
		},
		Logf: func(format string, args ...any) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.logged = append(r.logged, fmt.Sprintf(format, args...))
		},
	}
}

func (r *desktopRig) logMentions(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.logged {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func (r *desktopRig) replaces() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.replaced
}

func (r *desktopRig) setInUse(path string, inUse bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inUse[path] = inUse
}

func digestOf(contents string) string {
	sum := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(sum[:])
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stage records update 1.0.0 to 2.0.0 pending with install holding "old"
// and its staged target "new". A bundle install is a .app path.
func (r *desktopRig) stage(t *testing.T, install string) DesktopRecord {
	t.Helper()
	record := desktopUpdateRecord(t, install, desktopTestID)
	writeExecutable(t, DesktopExecutable(install), "old")
	writeExecutable(t, DesktopExecutable(record.StagedPath), "new")
	record.TargetDigest = digestOf("new")
	if err := SaveDesktopRecord(r.layout, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func (r *desktopRig) record(t *testing.T) DesktopRecord {
	t.Helper()
	record, found, err := LoadDesktopRecord(r.layout)
	if err != nil || !found {
		t.Fatalf("LoadDesktopRecord = %t, %v", found, err)
	}
	return record
}

func (r *desktopRig) setRecord(t *testing.T, change func(*UpdateRecord)) DesktopRecord {
	t.Helper()
	record := r.record(t)
	update := *record.Update
	change(&update)
	record.Update = &update
	if err := SaveDesktopRecord(r.layout, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestDesktopApplyPublishesTheCommittedTargetOnce(t *testing.T) {
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	helper := r.update("1.0.0", record.StagedPath, record.InstallPath, trialWritesAndPrepares)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateCommitted {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	if got := readFile(t, record.InstallPath); got != "new" {
		t.Fatalf("install path = %q, want the target", got)
	}
	if got := r.database(t); got != "migrated" {
		t.Fatalf("database = %q, want the trial's", got)
	}
	if present, _ := SnapshotPresent(r.layout); present {
		t.Fatal("the snapshot outlived the commit")
	}
	if u := r.record(t).Update; u.State != UpdateCommitted || u.Reported || u.FromSchema != 7 {
		t.Fatalf("record = %+v, want committed and unreported over schema 7", u)
	}
	if !strings.Contains(strings.Join(r.details, "\n"), "Installing v2.0.0") {
		t.Fatalf("progress %q does not name the install", r.details)
	}

	// A repeat, as after an interruption, finds the target installed.
	end, err = helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateCommitted {
		t.Fatalf("repeated Apply = %+v, %v", end, err)
	}
	if n := r.replaces(); n != 1 {
		t.Fatalf("the install path was replaced %d times, want once", n)
	}
	if got := readFile(t, record.InstallPath); got != "new" {
		t.Fatalf("install path after the repeat = %q", got)
	}
	if _, err := helper.Apply(context.Background(), "fedcba9876543210"); err == nil {
		t.Fatal("a helper for another update applied this one")
	}
}

func TestDesktopApplyRollsBackAndTheNextLaunchReportsIt(t *testing.T) {
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	helper := r.update("2.0.0", record.StagedPath, record.InstallPath, trialWritesAndCrashes)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateRolledBack || !strings.Contains(end.Reason, "exit status 3") {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	if got := r.database(t); got != "live" {
		t.Fatalf("database = %q, want the snapshot restored", got)
	}
	if got := readFile(t, record.InstallPath); got != "old" || r.replaces() != 0 {
		t.Fatalf("install path = %q after %d replaces, want the previous version untouched", got, r.replaces())
	}
	// The helper runs from the staged version, so it is left.
	if !exists(record.StagedPath) || !r.logMentions("runs this helper") {
		t.Fatal("the helper removed the version it runs from")
	}
	failed, found, err := LoadFailedTrial(FailedTrialPath(r.layout.StatePath()))
	if err != nil || !found || !failed.Matches("2.0.0", 7) {
		t.Fatalf("failure memory = %+v, %t, %v", failed, found, err)
	}

	app := r.update("1.0.0", record.InstallPath, record.InstallPath, "")
	decision, err := app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || decision.FailedTo != "2.0.0" ||
		!strings.Contains(decision.FailedReason, "exit status 3") || decision.UpdatingTo != "" {
		t.Fatalf("Reconcile = %+v, %v", decision, err)
	}
	if exists(record.StagedPath) {
		t.Fatal("the next launch left the staged version")
	}
	if !r.record(t).Update.Reported {
		t.Fatal("the outcome was not marked reported")
	}
	decision, err = app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || decision.FailedTo != "" {
		t.Fatalf("second Reconcile = %+v, %v; want a launch that reports nothing", decision, err)
	}
}

func bundleSwap(from, to string) error {
	tmp := to + ".swap"
	if err := os.Rename(to, tmp); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return os.Rename(tmp, from)
}

func TestDesktopBundlePublishKeepsThePreviousBundleWhileItRuns(t *testing.T) {
	r := newDesktopRig(t)
	r.swap = bundleSwap
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "Agent Overflow.app"))
	// A process still runs from the previous bundle, which the swap leaves
	// at the staged path.
	r.setInUse(record.StagedPath, true)
	helper := r.update("1.0.0", DesktopExecutable(record.StagedPath), record.InstallPath, trialWritesAndPrepares)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateCommitted {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	if got := readFile(t, DesktopExecutable(record.InstallPath)); got != "new" {
		t.Fatalf("installed bundle runs %q", got)
	}
	if got := readFile(t, DesktopExecutable(record.StagedPath)); got != "old" {
		t.Fatalf("the previous bundle = %q, want it kept at the staged path", got)
	}
	if !r.logMentions("a process runs from it") {
		t.Fatal("keeping the previous bundle was not logged")
	}

	// The first launch of the target finishes the update and still keeps it.
	app := r.update("2.0.0", DesktopExecutable(record.InstallPath), record.InstallPath, "")
	decision, err := app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || decision.UpdatingTo != "2.0.0" {
		t.Fatalf("Reconcile = %+v, %v", decision, err)
	}
	if !exists(record.StagedPath) {
		t.Fatal("a bundle in use was removed")
	}
	// A later launch removes it once nothing runs from it.
	r.setInUse(record.StagedPath, false)
	decision, err = app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || decision.UpdatingTo != "" {
		t.Fatalf("later Reconcile = %+v, %v", decision, err)
	}
	if exists(record.StagedPath) {
		t.Fatal("the unused previous bundle was kept")
	}
}

func TestDesktopBundlePublishTakesTwoRenamesWhereItCannotSwap(t *testing.T) {
	r := newDesktopRig(t)
	r.swap = func(string, string) error { return &os.LinkError{Op: "renamex_np", Err: unix.ENOTSUP} }
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "Agent Overflow.app"))
	previous := desktopPreviousPath(record)
	r.setInUse(previous, true)
	helper := r.update("1.0.0", DesktopExecutable(record.StagedPath), record.InstallPath, trialWritesAndPrepares)

	end, err := helper.Apply(context.Background(), desktopTestID)
	if err != nil || end.State != UpdateCommitted {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	if got := readFile(t, DesktopExecutable(record.InstallPath)); got != "new" {
		t.Fatalf("installed bundle runs %q", got)
	}
	if got := readFile(t, DesktopExecutable(previous)); got != "old" {
		t.Fatalf("set-aside bundle = %q, want the previous version kept while in use", got)
	}
	r.setInUse(previous, false)
	app := r.update("2.0.0", DesktopExecutable(record.InstallPath), record.InstallPath, "")
	if _, err := app.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(previous) || exists(record.StagedPath) {
		t.Fatal("the next launch left the update's paths")
	}
}

func TestReplaceBundleFinishesAnInterruptedPairOfRenames(t *testing.T) {
	dir := t.TempDir()
	staged, install, previous := filepath.Join(dir, "s.app"), filepath.Join(dir, "i.app"), filepath.Join(dir, "p.app")
	writeExecutable(t, DesktopExecutable(staged), "new")
	// Interrupted after the first rename: the install path is empty.
	writeExecutable(t, DesktopExecutable(previous), "old")
	notSupported := func(string, string) error { return unix.EINVAL }
	if err := replaceBundle(staged, install, previous, notSupported); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, DesktopExecutable(install)); got != "new" || readFile(t, DesktopExecutable(previous)) != "old" {
		t.Fatalf("install = %q", got)
	}

	// Any other swap failure is the publish's error and moves nothing.
	writeExecutable(t, DesktopExecutable(staged), "newer")
	denied := func(string, string) error { return unix.EACCES }
	if err := replaceBundle(staged, install, filepath.Join(dir, "q.app"), denied); !errors.Is(err, unix.EACCES) {
		t.Fatalf("replaceBundle = %v, want the swap's error", err)
	}
	if readFile(t, DesktopExecutable(install)) != "new" || !exists(staged) {
		t.Fatal("a failed swap moved a bundle")
	}
}

func TestDesktopMigrationFailureIsRememberedUntilRetriedOrRebuilt(t *testing.T) {
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	install := filepath.Join(r.apps, "agent-overflow")
	app := r.update("2.0.0", install, install, trialWritesAndCrashes)
	trials := func() int { return strings.Count(r.read("activate"), "\n") }

	// Launch one runs the trial, which fails.
	end := app.Migrate(context.Background(), DesktopMigration{Schema: 7})
	if end.Launch || end.Retry || end.Title != MigrationFailedTitle || !strings.Contains(end.Detail, "The backup was restored") {
		t.Fatalf("first launch = %+v", end)
	}
	if trials() != 1 {
		t.Fatalf("trials = %d, want 1", trials())
	}
	if exists(r.layout.StatePath()) {
		t.Fatal("the settled migration's record was kept")
	}

	// Launch two, same build over the same schema: the page, no trial.
	end = app.Migrate(context.Background(), DesktopMigration{Schema: 7})
	if end.Launch || !end.Retry || !strings.Contains(end.Detail, "The last attempt stopped at: Applying migration 1 of 1.") ||
		!strings.Contains(end.Detail, "exit status 3") || !strings.Contains(end.Detail, "update log") {
		t.Fatalf("second launch = %+v", end)
	}
	if trials() != 1 {
		t.Fatalf("trials = %d after the remembered launch, want 1", trials())
	}

	// Retry runs it again.
	if end := app.Migrate(context.Background(), DesktopMigration{Schema: 7, Retry: true}); end.Launch || end.Retry {
		t.Fatalf("retry = %+v, want the trial's failure", end)
	}
	if trials() != 2 {
		t.Fatalf("trials = %d after Retry, want 2", trials())
	}

	// A changed schema version runs without Retry.
	r.schema = 8
	if end := app.Migrate(context.Background(), DesktopMigration{Schema: 8}); end.Retry {
		t.Fatalf("a changed schema version was stopped: %+v", end)
	}
	if trials() != 3 {
		t.Fatalf("trials = %d after a schema change, want 3", trials())
	}

	// A new build runs without Retry, and its success removes the memory.
	rebuilt := r.update("2.0.1", install, install, trialWritesAndPrepares)
	if end := rebuilt.Migrate(context.Background(), DesktopMigration{Schema: 8}); !end.Launch {
		t.Fatalf("new build = %+v, want a launch", end)
	}
	if trials() != 4 {
		t.Fatalf("trials = %d after a new build, want 4", trials())
	}
	if _, found, _ := LoadFailedTrial(FailedTrialPath(r.layout.StatePath())); found {
		t.Fatal("a committed migration left the failure memory")
	}
	if got := r.database(t); got != "migrated" {
		t.Fatalf("database = %q, want the committed trial's", got)
	}
}

func TestDesktopMigrationRefusesWhileAnUpdateIsPending(t *testing.T) {
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	app := r.update("1.0.0", record.InstallPath, record.InstallPath, trialWritesAndPrepares)
	end := app.Migrate(context.Background(), DesktopMigration{Schema: 7})
	if end.Launch || end.Retry || !strings.Contains(end.Title, "could not start the database upgrade") {
		t.Fatalf("Migrate = %+v", end)
	}
	if r.read("activate") != "" {
		t.Fatal("a trial ran over a pending update")
	}
	if u := r.record(t).Update; u.ID != desktopTestID || u.State != UpdatePending {
		t.Fatalf("the pending update became %+v", u)
	}
}

func TestDesktopReconcileResumesAMigrationInThisBinary(t *testing.T) {
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	install := filepath.Join(r.apps, "agent-overflow")
	app := r.update("2.0.0", install, install, "")
	begin := func(attempts int, state UpdateState) {
		t.Helper()
		if err := os.Remove(r.layout.StatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := app.beginMigration(r.layout, 7, desktopTestID); err != nil {
			t.Fatal(err)
		}
		r.setRecord(t, func(u *UpdateRecord) { u.Attempts, u.State = attempts, state })
	}

	begin(1, UpdatePending)
	decision, err := app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopHandOff || decision.Helper != install || !decision.Record.Migration() {
		t.Fatalf("Reconcile of a migration with an attempt = %+v, %v", decision, err)
	}

	begin(0, UpdatePending)
	decision, err = app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || exists(r.layout.StatePath()) {
		t.Fatalf("Reconcile of a migration before its trial = %+v, %v; want a launch without the record", decision, err)
	}

	begin(1, UpdateCommitted)
	decision, err = app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || exists(r.layout.StatePath()) {
		t.Fatalf("Reconcile of a settled migration = %+v, %v", decision, err)
	}
}

func TestDesktopReconcileHandsACommittedUpdateBackToItsHelper(t *testing.T) {
	r := newDesktopRig(t)
	writeFile(t, r.db, "live")
	record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
	r.setRecord(t, func(u *UpdateRecord) { u.Attempts, u.State = 1, UpdateCommitted })
	previous := r.update("1.0.0", record.InstallPath, record.InstallPath, "")

	decision, err := previous.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopHandOff || decision.Helper != record.StagedPath {
		t.Fatalf("Reconcile = %+v, %v; want the staged helper", decision, err)
	}
	if r.record(t).Update.Reported {
		t.Fatal("a hand-off marked the update reported")
	}

	if err := os.Remove(record.StagedPath); err != nil {
		t.Fatal(err)
	}
	decision, err = previous.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopBlocked || decision.Title != "The update to v2.0.0 could not be installed." ||
		!strings.Contains(decision.Detail, "Install v2.0.0 from the releases page") {
		t.Fatalf("Reconcile without the staged version = %+v, %v", decision, err)
	}

	// A version installed by hand finishes the record without replacing
	// itself, and names no update it finished.
	other := r.update("3.0.0", record.InstallPath, record.InstallPath, "")
	decision, err = other.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || decision.UpdatingTo != "" || decision.FailedTo != "" {
		t.Fatalf("Reconcile from another version = %+v, %v", decision, err)
	}
	if !r.record(t).Update.Reported {
		t.Fatal("the committed update was not marked reported")
	}
}

func TestDesktopReconcileRecoversAPendingUpdate(t *testing.T) {
	t.Run("interrupted before its trial", func(t *testing.T) {
		r := newDesktopRig(t)
		writeFile(t, r.db, "live")
		record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
		app := r.update("1.0.0", record.InstallPath, record.InstallPath, "")
		decision, err := app.Reconcile(context.Background())
		if err != nil || decision.Action != DesktopLaunch || decision.FailedTo != "2.0.0" ||
			decision.FailedReason != "the update was interrupted before its trial started" {
			t.Fatalf("Reconcile = %+v, %v", decision, err)
		}
		if u := r.record(t).Update; u.State != UpdateFailed || !u.Reported {
			t.Fatalf("record = %+v", u)
		}
		if exists(record.StagedPath) {
			t.Fatal("the staged version was left")
		}
	})

	t.Run("mid-trial with its target staged", func(t *testing.T) {
		r := newDesktopRig(t)
		writeFile(t, r.db, "live")
		record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
		r.setRecord(t, func(u *UpdateRecord) { u.Attempts = 1 })
		app := r.update("1.0.0", record.InstallPath, record.InstallPath, "")
		decision, err := app.Reconcile(context.Background())
		if err != nil || decision.Action != DesktopHandOff || decision.Helper != record.StagedPath {
			t.Fatalf("Reconcile = %+v, %v", decision, err)
		}
	})

	for _, tc := range []struct {
		name, version, reason string
		removeStaged          bool
		attempts              int
	}{
		{"mid-trial with its target gone", "1.0.0", "the update was interrupted and its new version is missing", true, 1},
		{"mid-trial under another version", "3.0.0", "the update was interrupted and v3.0.0 was started instead", false, 1},
		{"interrupted at every attempt", "1.0.0", fmt.Sprintf("the trial was interrupted %d times without finishing", TrialAttemptLimit), false, TrialAttemptLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDesktopRig(t)
			writeFile(t, r.db, "live")
			record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
			if _, err := TakeSnapshot(r.layout, r.dataDir, time.Now(), SnapshotOptions{UpdateID: desktopTestID}); err != nil {
				t.Fatal(err)
			}
			writeFile(t, r.db, "half-migrated")
			r.setRecord(t, func(u *UpdateRecord) { u.Attempts, u.FromSchema = tc.attempts, 7 })
			if tc.removeStaged {
				if err := os.Remove(record.StagedPath); err != nil {
					t.Fatal(err)
				}
			}
			app := r.update(tc.version, record.InstallPath, record.InstallPath, "")
			decision, err := app.Reconcile(context.Background())
			if err != nil || decision.Action != DesktopLaunch {
				t.Fatalf("Reconcile = %+v, %v", decision, err)
			}
			if u := r.record(t).Update; u.State != UpdateRolledBack || u.Reason != tc.reason || !u.Reported {
				t.Fatalf("record = %+v, want rolled back: %s", u, tc.reason)
			}
			wantFailed := ""
			if tc.version == "1.0.0" {
				wantFailed = "2.0.0"
			}
			if decision.FailedTo != wantFailed {
				t.Fatalf("FailedTo = %q, want %q", decision.FailedTo, wantFailed)
			}
			if got := r.database(t); got != "live" {
				t.Fatalf("database = %q, want the snapshot restored", got)
			}
			_, remembered, _ := LoadFailedTrial(FailedTrialPath(r.layout.StatePath()))
			if remembered != (tc.attempts >= TrialAttemptLimit) {
				t.Fatalf("remembered = %t", remembered)
			}
		})
	}

	t.Run("a restore that cannot finish starts nothing", func(t *testing.T) {
		r := newDesktopRig(t)
		writeFile(t, r.db, "live")
		record := r.stage(t, filepath.Join(r.apps, "agent-overflow"))
		if _, err := TakeSnapshot(r.layout, r.dataDir, time.Now(), SnapshotOptions{UpdateID: desktopTestID}); err != nil {
			t.Fatal(err)
		}
		r.setRecord(t, func(u *UpdateRecord) { u.Attempts = 1 })
		if err := os.Remove(record.StagedPath); err != nil {
			t.Fatal(err)
		}
		app := r.update("1.0.0", record.InstallPath, record.InstallPath, "")
		var decision DesktopDecision
		var err error
		failRestore(t, r.commandRig, func() { decision, err = app.Reconcile(context.Background()) })
		if err != nil || decision.Action != DesktopBlocked || !strings.Contains(decision.Title, "could not be restored") ||
			!strings.Contains(decision.Detail, "update log") {
			t.Fatalf("Reconcile = %+v, %v", decision, err)
		}
		if u := r.record(t).Update; u.State != UpdatePending {
			t.Fatalf("record = %+v, want it left pending for the next launch", u)
		}
	})
}

// handoffRig is the running app's half: the downloaded release and the
// fakes for its preflight and its helper's start.
type handoffRig struct {
	*desktopRig
	install string
	// tempDir stands for the temp directory the framework downloads under.
	tempDir    string
	downloaded string
	answer     Preflight
	startErr   error
	started    [][]string
	logged     []string
}

func newHandoffRig(t *testing.T) *handoffRig {
	t.Helper()
	h := &handoffRig{desktopRig: newDesktopRig(t), answer: Preflight{ProtocolVersion: ProtocolVersion, Version: "2.0.0", AppUpdateTrial: true}}
	writeFile(t, h.db, "live")
	h.install = filepath.Join(h.apps, "agent-overflow")
	writeExecutable(t, h.install, "old")
	h.tempDir = t.TempDir()
	h.downloaded = filepath.Join(h.tempDir, frameworkDownloadPrefix+"1", "agent-overflow")
	h.download(t, h.downloaded)
	return h
}

// download writes a release at path as the framework does: without the
// executable bit, beside the archive it came in.
func (h *handoffRig) download(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(path), "agent-overflow.tar.gz"), "archive")
}

func (h *handoffRig) handoff() DesktopHandoff {
	return DesktopHandoff{
		DataDir: h.dataDir, DataDirFlag: h.dataDir, Version: "1.0.0", Executable: h.install, GOOS: "linux",
		RelaunchArgs: []string{"--data-dir", h.dataDir},
		TempDir:      h.tempDir,
		Preflight: func(_ context.Context, binary string) (Preflight, error) {
			info, err := os.Stat(binary)
			if err != nil {
				return Preflight{}, err
			}
			if info.Mode().Perm()&0o100 == 0 {
				return Preflight{}, errors.New("permission denied")
			}
			return h.answer, nil
		},
		Start: func(executable string, args []string, logPath string) error {
			h.started = append(h.started, append([]string{executable, logPath}, args...))
			return h.startErr
		},
		Logf: func(format string, args ...any) { h.logged = append(h.logged, fmt.Sprintf(format, args...)) },
	}
}

func TestDesktopCheckGivesTheTrialOnlyToATargetThatRunsIt(t *testing.T) {
	h := newHandoffRig(t)
	for _, tc := range []struct {
		name, version string
		answer        Preflight
		want          bool
		wantErr       string
	}{
		{"a target that runs the helper", "2.0.0", h.answer, true, ""},
		{"the running version", "1.0.0", h.answer, false, ""},
		{"a release before the helper", "0.0.14", Preflight{Version: "0.0.14", AppUpdateTrial: true}, false, ""},
		{"a dev build's version", "dev", Preflight{Version: "dev", AppUpdateTrial: true}, false, ""},
		{"a build without the helper", "2.0.0", Preflight{Version: "2.0.0"}, false, ""},
		{"a target that answers another version", "2.0.0", Preflight{Version: "2.0.1", AppUpdateTrial: true}, false, "reports version 2.0.1"},
	} {
		h.answer = tc.answer
		got, err := h.handoff().Check(context.Background(), h.downloaded, tc.version)
		if got != tc.want || (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: Check = %t, %v; want %t, %q", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
	entries, err := os.ReadDir(h.apps)
	if err != nil || len(entries) != 1 || exists(h.layout.StatePath()) {
		t.Fatalf("install folder = %v, %v; Check staged or recorded something", entries, err)
	}
}

func TestDesktopHandOffStagesRecordsAndStartsTheTargetsHelper(t *testing.T) {
	h := newHandoffRig(t)
	handoff := h.handoff()
	if err := handoff.HandOff(context.Background(), h.downloaded, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	record := h.record(t)
	u := record.Update
	if u.State != UpdatePending || u.From != "1.0.0" || u.To != "2.0.0" || u.Attempts != 0 {
		t.Fatalf("record = %+v", u)
	}
	if record.InstallPath != h.install || record.StagedPath != DesktopStagedPath(h.install, u.ID) || record.TargetDigest != digestOf("new") {
		t.Fatalf("record = %+v", record)
	}
	info, err := os.Stat(record.StagedPath)
	if err != nil || info.Mode().Perm() != 0o755 || exists(h.downloaded) {
		t.Fatalf("staged = %v, %v; want the download moved beside the install with its mode", info, err)
	}
	if exists(filepath.Dir(h.downloaded)) || !exists(h.tempDir) {
		t.Fatal("the framework's download folder was left, or more than it was removed")
	}
	self, err := CurrentProcessRef()
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{record.StagedPath, DesktopUpdateLogPath(h.layout)},
		DesktopHelperArgs([]string{"--id", u.ID}, self, h.dataDir, []string{"--data-dir", h.dataDir})...)
	if len(h.started) != 1 || strings.Join(h.started[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("started %q\nwant %q", h.started, want)
	}
	if got := handoff.RestartingTo(); got != "2.0.0" {
		t.Fatalf("RestartingTo = %q", got)
	}
	// Another version reading the record did not hand it off.
	other := h.handoff()
	other.Version = "1.5.0"
	if got := other.RestartingTo(); got != "" {
		t.Fatalf("RestartingTo of another version = %q", got)
	}
	if err := handoff.HandOff(context.Background(), h.downloaded, "2.0.0"); err == nil || !strings.Contains(err.Error(), "still in progress") {
		t.Fatalf("a second HandOff = %v, want the pending update's refusal", err)
	}
}

// TestDesktopHandOffRemovesOnlyTheFrameworksDownloadFolder: the handoff
// replaces the framework's Restart, which removed the folder it downloaded
// into. The handoff removes that folder once the record names the staged
// copy, and leaves any other folder, saying so.
func TestDesktopHandOffRemovesOnlyTheFrameworksDownloadFolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rel is the download under the temp directory; outside places it
		// in another directory.
		rel     string
		outside bool
		// removed is the folder under the temp directory that goes.
		removed string
	}{
		{name: "a bundle extracted in a subfolder", rel: filepath.Join(frameworkDownloadPrefix+"7", "extracted", "agent-overflow"), removed: frameworkDownloadPrefix + "7"},
		{name: "a folder the framework did not name", rel: filepath.Join("other-1", "agent-overflow")},
		{name: "the temp directory itself", rel: "agent-overflow"},
		{name: "outside the temp directory", rel: filepath.Join(frameworkDownloadPrefix+"1", "agent-overflow"), outside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandoffRig(t)
			if err := os.RemoveAll(filepath.Dir(h.downloaded)); err != nil {
				t.Fatal(err)
			}
			base := h.tempDir
			if tc.outside {
				base = t.TempDir()
			}
			h.downloaded = filepath.Join(base, tc.rel)
			h.download(t, h.downloaded)
			if err := h.handoff().HandOff(context.Background(), h.downloaded, "2.0.0"); err != nil {
				t.Fatal(err)
			}
			if exists(h.downloaded) || h.record(t).Update.State != UpdatePending {
				t.Fatal("the download was not handed off")
			}
			archive := filepath.Join(filepath.Dir(h.downloaded), "agent-overflow.tar.gz")
			if tc.removed != "" {
				if exists(filepath.Join(h.tempDir, tc.removed)) || !exists(h.tempDir) {
					t.Fatal("the framework's download folder was left, or the temp directory went with it")
				}
				return
			}
			if !exists(archive) || !exists(h.tempDir) {
				t.Fatal("a folder the framework did not download into was removed")
			}
			if len(h.logged) == 0 || !strings.Contains(strings.Join(h.logged, "\n"), "is not in a download folder of the updater") {
				t.Fatalf("logged %q; want the folder left named", h.logged)
			}
		})
	}

	t.Run("a folder that cannot be removed", func(t *testing.T) {
		h := newHandoffRig(t)
		if err := os.Chmod(h.tempDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(h.tempDir, 0o700) })
		if err := h.handoff().HandOff(context.Background(), h.downloaded, "2.0.0"); err != nil {
			t.Fatalf("HandOff = %v; a folder left behind does not stop the update", err)
		}
		if len(h.started) != 1 || !exists(filepath.Dir(h.downloaded)) {
			t.Fatalf("started %q; want the helper started and the folder left", h.started)
		}
		if !strings.Contains(strings.Join(h.logged, "\n"), "remove the download folder") {
			t.Fatalf("logged %q; want the failed removal named", h.logged)
		}
	})
}

func TestDesktopHandOffThatCannotStartItsHelperSettlesTheUpdate(t *testing.T) {
	h := newHandoffRig(t)
	h.startErr = errors.New("exec format error")
	handoff := h.handoff()
	err := handoff.HandOff(context.Background(), h.downloaded, "2.0.0")
	if err == nil || !strings.Contains(err.Error(), "start the update: exec format error") {
		t.Fatalf("HandOff = %v", err)
	}
	record := h.record(t)
	if u := record.Update; u.State != UpdateFailed || !u.Reported || !strings.Contains(u.Reason, "exec format error") {
		t.Fatalf("record = %+v, want failed and already reported", u)
	}
	if exists(record.StagedPath) {
		t.Fatal("the staged version was left")
	}
	if got := handoff.RestartingTo(); got != "" {
		t.Fatalf("RestartingTo = %q after a failed start", got)
	}
	// The next launch neither resumes nor reports it again.
	app := h.update("1.0.0", h.install, h.install, "")
	decision, err := app.Reconcile(context.Background())
	if err != nil || decision.Action != DesktopLaunch || decision.FailedTo != "" {
		t.Fatalf("Reconcile = %+v, %v", decision, err)
	}
}

func TestDesktopHandOffRecordsNothingForATargetThatCannotApply(t *testing.T) {
	h := newHandoffRig(t)
	h.answer.AppUpdateTrial = false
	if err := h.handoff().HandOff(context.Background(), h.downloaded, "2.0.0"); err == nil {
		t.Fatal("a target without the helper was handed the update")
	}
	if exists(h.layout.StatePath()) || len(h.started) != 0 {
		t.Fatal("a refused target was recorded or started")
	}
	entries, err := os.ReadDir(h.apps)
	if err != nil || len(entries) != 1 {
		t.Fatalf("install folder = %v, %v; want only the install path", entries, err)
	}
}

func TestMovePathCopiesAcrossFilesystems(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Agent Overflow.app")
	writeExecutable(t, DesktopExecutable(src), "new")
	writeFile(t, filepath.Join(src, "Contents", "Info.plist"), "plist")
	if err := os.Symlink("Versions/A", filepath.Join(src, "Contents", "Current")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), ".agent-overflow-update-x.app")
	moveRename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	t.Cleanup(func() { moveRename = os.Rename })
	if err := movePath(src, dst); err != nil {
		t.Fatal(err)
	}
	if exists(src) {
		t.Fatal("the source outlived the move")
	}
	info, err := os.Stat(DesktopExecutable(dst))
	if err != nil || info.Mode().Perm() != 0o755 || readFile(t, DesktopExecutable(dst)) != "new" {
		t.Fatalf("copied executable = %v, %v", info, err)
	}
	if link, err := os.Readlink(filepath.Join(dst, "Contents", "Current")); err != nil || link != "Versions/A" {
		t.Fatalf("copied link = %q, %v", link, err)
	}

	// A copy that fails is removed and the source kept.
	src2 := filepath.Join(t.TempDir(), "agent-overflow")
	writeExecutable(t, src2, "new")
	dst2 := filepath.Join(t.TempDir(), "missing", "agent-overflow")
	if err := movePath(src2, dst2); err == nil {
		t.Fatal("a copy into a missing folder succeeded")
	}
	if !exists(src2) || exists(dst2) {
		t.Fatal("a failed copy lost the source or left a partial copy")
	}
}

func TestPrepareDataRootLetsTheDesktopUpdateOwnItsRecord(t *testing.T) {
	dataDir := t.TempDir()
	if err := SaveState(appLayout(t, dataDir), pendingState(t, "app-u")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDataRoot(dataDir, PrepareOptions{}); !IsPendingUpdate(err) {
		t.Fatalf("PrepareDataRoot = %v, want the pending update's refusal", err)
	}
	if err := PrepareDataRoot(dataDir, PrepareOptions{OwnsAppLayout: true}); err != nil {
		t.Fatalf("the desktop update was refused its own record: %v", err)
	}
}

func TestUpdateCommandProgressGoesToItsSinkInsteadOfItsOutput(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	var mu sync.Mutex
	var details []string
	command := r.command("u1")
	command.Progress = func(p startupprogress.Progress) {
		mu.Lock()
		defer mu.Unlock()
		details = append(details, p.Detail)
	}
	if result := command.Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(details, "\n"), "Backing up the database") {
		t.Fatalf("progress = %q", details)
	}
	if r.out.buf.Len() != 0 {
		t.Fatalf("the output got %q", r.out.buf.String())
	}
}

func TestPreflightAnswersWhetherTheBuildRunsTheDesktopHelper(t *testing.T) {
	for _, trial := range []bool{true, false} {
		var out bytes.Buffer
		if err := WritePreflight(&out, "2.0.0", trial); err != nil {
			t.Fatal(err)
		}
		answer, err := ParsePreflight(out.String())
		if err != nil || answer.AppUpdateTrial != trial || answer.Version != "2.0.0" {
			t.Fatalf("round trip of %t = %+v, %v", trial, answer, err)
		}
	}
	// An answer from before the field reads as a build without the helper.
	answer, err := ParsePreflight(`{"protocolVersion":1,"version":"0.0.14"}`)
	if err != nil || answer.AppUpdateTrial {
		t.Fatalf("an old answer = %+v, %v", answer, err)
	}
}
