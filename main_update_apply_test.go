//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/startuppage"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/store"
	"agent-overflow/internal/supervise"
)

// The macOS and Linux desktop's helper and boot against the real update
// steps: supervise.DesktopUpdate built by newDesktopUpdate, its snapshot,
// restore and discard in this process, and the trial this test binary run
// as UpdateTrialCommand, which TestMain answers with the trial protocol
// around a stub boot. The window is a fake that records what it was told,
// and the app a relaunch starts is recorded rather than started.

// fakeApplyUI records what the helper's window was told.
type fakeApplyUI struct {
	mu      sync.Mutex
	loads   int
	reports []startupprogress.Progress
	pages   []startuppage.Failure
	quits   int
}

func (u *fakeApplyUI) loading() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.loads++
}

func (u *fakeApplyUI) progress(p startupprogress.Progress) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reports = append(u.reports, p)
}

func (u *fakeApplyUI) fail(page startuppage.Failure) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pages = append(u.pages, page)
}

func (u *fakeApplyUI) quit() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.quits++
}

func (u *fakeApplyUI) lastPage(t *testing.T) startuppage.Failure {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.pages) == 0 {
		t.Fatal("the helper showed no failure page")
	}
	return u.pages[len(u.pages)-1]
}

type desktopApplyRig struct {
	t    *testing.T
	root string
	dir  string
	ui   *fakeApplyUI

	mu       sync.Mutex
	lock     *harnessInstanceLock
	starts   [][]string
	startErr error
}

func newDesktopApplyRig(t *testing.T) *desktopApplyRig {
	t.Helper()
	kerneltest.IsolateSpawns(t)
	root := t.TempDir()
	previous := dataDirRoot
	dataDirRoot = root
	t.Cleanup(func() { dataDirRoot = previous })
	dir := filepath.Join(root, appdirs.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := &desktopApplyRig{t: t, root: root, dir: dir, ui: &fakeApplyUI{}}
	r.writeDB("live")
	t.Cleanup(r.release)
	return r
}

func (r *desktopApplyRig) writeDB(contents string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "agent-overflow.db"), []byte(contents), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *desktopApplyRig) readDB() string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, "agent-overflow.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data)
}

// trialRuns is how many stub trials ran.
func (r *desktopApplyRig) trialRuns() int {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, stubTrialRuns))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func (r *desktopApplyRig) layout() supervise.Layout {
	r.t.Helper()
	layout, err := supervise.NewAppUpdateLayout(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	return layout
}

func (r *desktopApplyRig) memory() (supervise.FailedTrial, bool) {
	r.t.Helper()
	failed, found, err := supervise.LoadFailedTrial(supervise.FailedTrialPath(r.layout().StatePath()))
	if err != nil {
		r.t.Fatal(err)
	}
	return failed, found
}

// release drops the backend lock the last helper took, as its exit does.
func (r *desktopApplyRig) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock != nil {
		r.lock.file.Close()
		r.lock = nil
	}
}

func (r *desktopApplyRig) started() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.starts...)
}

// applier is a helper over the rig whose trial is the stub in mode. Each
// one is a new helper process: the previous one's lock is released first.
func (r *desktopApplyRig) applier(flags desktopApplyFlags, stub string, edit func(*supervise.DesktopUpdate)) *desktopApplier {
	r.t.Helper()
	r.release()
	update, err := newDesktopUpdate(r.t.Logf)
	if err != nil {
		r.t.Fatal(err)
	}
	update.Trial.Env = append(os.Environ(), updateTrialStubEnv+"="+stub)
	// The stub's database is not SQLite; the schema version the snapshot
	// records under the lock is fixed.
	update.SchemaVersion = func() (int, error) { return 88, nil }
	if edit != nil {
		edit(&update)
	}
	return &desktopApplier{
		flags:       flags,
		update:      update,
		ui:          r.ui,
		logPath:     "/data/update.log",
		retryMethod: "main.desktopApplyWindow.RetryMigration",
		acquireLock: func(context.Context) (*os.File, error) {
			lock, err := acquireBackendInstanceLock(r.dir)
			if err != nil {
				return nil, err
			}
			r.mu.Lock()
			r.lock = lock
			r.mu.Unlock()
			return lock.file, nil
		},
		start: func(install string, args []string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.starts = append(r.starts, append([]string{install}, args...))
			return r.startErr
		},
		logf: r.t.Logf,
	}
}

// relaunchArgs is the argv a helper in this process starts the install
// path with.
func relaunchArgs(t *testing.T, relaunch []string) []string {
	t.Helper()
	self, err := supervise.CurrentProcessRef()
	if err != nil {
		t.Fatal(err)
	}
	return supervise.DesktopAppArgs(self, relaunch)
}

// TestDesktopMigrationRemembersAFailedTrial: the desktop gate's migration
// runs one trial for two launches of the same build over the same schema,
// a second on Retry, and runs normally for a new build.
func TestDesktopMigrationRemembersAFailedTrial(t *testing.T) {
	r := newDesktopApplyRig(t)
	flags := desktopApplyFlags{migrate: true, schema: 88, relaunch: []string{"--data-dir", r.root}}

	first := r.applier(flags, "fail", nil)
	first.run(t.Context())
	page := r.ui.lastPage(t)
	if page.Title != supervise.MigrationFailedTitle || page.Retry != "" ||
		!strings.Contains(page.Detail, "is newer than this build knows") || page.Log != "/data/update.log" {
		t.Fatalf("the first launch's page = %+v", page)
	}
	if runs, db := r.trialRuns(), r.readDB(); runs != 1 || db != "live" {
		t.Fatalf("the first launch ran %d trials and left the database %q; want one, restored", runs, db)
	}
	if failed, found := r.memory(); !found || failed.Build != version || failed.Schema != 88 {
		t.Fatalf("failure memory = %+v, found %v", failed, found)
	}

	second := r.applier(flags, "fail", nil)
	second.run(t.Context())
	page = r.ui.lastPage(t)
	if page.Retry != second.retryMethod || page.Title != supervise.MigrationFailedTitle {
		t.Fatalf("the second launch's page = %+v; want the remembered failure with Retry", page)
	}
	if runs := r.trialRuns(); runs != 1 {
		t.Fatalf("the second launch ran a trial (%d in all)", runs)
	}

	if err := second.retryMigration(t.Context()); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if runs, page := r.trialRuns(), r.ui.lastPage(t); runs != 2 || page.Retry != "" || page.Title != supervise.MigrationFailedTitle {
		t.Fatalf("Retry ran %d trials in all and showed %+v; want a second trial, failed without Retry", runs, page)
	}
	if err := second.retryMigration(t.Context()); err == nil || !strings.Contains(err.Error(), "no database upgrade to retry") {
		t.Fatalf("a Retry the page no longer offers = %v", err)
	}

	third := r.applier(flags, "prepare", func(u *supervise.DesktopUpdate) { u.Version = "2.0.1" })
	third.run(t.Context())
	if runs, db := r.trialRuns(), r.readDB(); runs != 3 || db != "trial" {
		t.Fatalf("a new build ran %d trials in all and left the database %q; want a third, committed", runs, db)
	}
	want := append([]string{third.update.InstallPath}, relaunchArgs(t, flags.relaunch)...)
	if starts := r.started(); len(starts) != 1 || !reflect.DeepEqual(starts[0], want) {
		t.Fatalf("started %q, want %q", starts, want)
	}
	if r.ui.quits != 1 {
		t.Fatalf("the helper quit %d times after the relaunch, want once", r.ui.quits)
	}
	if _, found := r.memory(); found {
		t.Fatal("the committed trial left the failure memory")
	}
	if _, found, err := supervise.LoadDesktopRecord(r.layout()); found || err != nil {
		t.Fatalf("the settled migration left its record: found=%v err=%v", found, err)
	}
}

// desktopUpdateFixture records a pending update from install to a staged
// target beside it, as the old app's handoff does.
func (r *desktopApplyRig) desktopUpdateFixture(id string, attempts int) (supervise.DesktopRecord, string) {
	r.t.Helper()
	install := filepath.Join(r.t.TempDir(), "agent-overflow")
	if err := os.WriteFile(install, []byte("old"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	staged := supervise.DesktopStagedPath(install, id)
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("new"))
	base, err := supervise.Adopt("1.0.0")
	if err != nil {
		r.t.Fatal(err)
	}
	state, err := base.Begin(id, "2.0.0", time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	for range attempts {
		if state, err = state.Retry(); err != nil {
			r.t.Fatal(err)
		}
	}
	record := supervise.DesktopRecord{State: state, InstallPath: install, StagedPath: staged, TargetDigest: hex.EncodeToString(digest[:])}
	if err := supervise.SaveDesktopRecord(r.layout(), record); err != nil {
		r.t.Fatal(err)
	}
	return record, install
}

func TestDesktopHelperAppliesAnUpdateThroughTheRealTrial(t *testing.T) {
	const id = "0123456789abcdef"
	for _, tc := range []struct {
		name      string
		stub      string
		state     supervise.UpdateState
		installed string
		db        string
	}{
		{"prepared publishes", "prepare", supervise.UpdateCommitted, "new", "trial"},
		{"failed rolls back", "fail", supervise.UpdateRolledBack, "old", "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDesktopApplyRig(t)
			record, install := r.desktopUpdateFixture(id, 0)
			flags := desktopApplyFlags{id: id, relaunch: []string{"--data-dir", r.root}}
			r.applier(flags, tc.stub, nil).run(t.Context())

			if len(r.ui.pages) != 0 {
				t.Fatalf("the helper showed %+v", r.ui.pages)
			}
			loaded, _, err := supervise.LoadDesktopRecord(r.layout())
			if err != nil || loaded.Update.State != tc.state {
				t.Fatalf("record = %+v, %v; want %s", loaded.Update, err, tc.state)
			}
			if got, err := os.ReadFile(install); err != nil || string(got) != tc.installed {
				t.Fatalf("the install path holds %q (%v), want %q", got, err, tc.installed)
			}
			if _, err := os.Stat(record.StagedPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the staged target survived the update: %v", err)
			}
			if db := r.readDB(); db != tc.db {
				t.Fatalf("database = %q, want %q", db, tc.db)
			}
			want := append([]string{install}, relaunchArgs(t, flags.relaunch)...)
			if starts := r.started(); len(starts) != 1 || !reflect.DeepEqual(starts[0], want) || r.ui.quits != 1 {
				t.Fatalf("started %q and quit %d times; want %q once", starts, r.ui.quits, want)
			}
			if len(r.ui.reports) == 0 {
				t.Fatal("the helper reported no progress")
			}
			for _, p := range r.ui.reports {
				if p.UpdatingTo != "2.0.0" {
					t.Fatalf("progress %+v does not name the update to 2.0.0", p)
				}
			}
			if _, remembered := r.memory(); remembered != (tc.state == supervise.UpdateRolledBack) {
				t.Fatalf("failure memory present = %v after %s", remembered, tc.state)
			}
		})
	}
}

func TestDesktopHelperShowsWhyItCannotRun(t *testing.T) {
	const id = "0123456789abcdef"
	t.Run("another backend holds the data root", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		r.desktopUpdateFixture(id, 0)
		held, err := acquireBackendInstanceLock(r.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer held.file.Close()
		a := r.applier(desktopApplyFlags{id: id}, "prepare", nil)
		a.acquireLock = func(context.Context) (*os.File, error) {
			lock, err := acquireBackendInstanceLock(r.dir)
			if err != nil {
				return nil, err
			}
			return lock.file, nil
		}
		a.run(t.Context())
		if page := r.ui.lastPage(t); page.Title != "The update could not start." {
			t.Fatalf("page = %+v", page)
		}
		loaded, _, err := supervise.LoadDesktopRecord(r.layout())
		if err != nil || loaded.Update.State != supervise.UpdateFailed || loaded.Update.Reported ||
			loaded.Update.Reason != "another Agent Overflow backend was using the data folder" {
			t.Fatalf("record = %+v, %v; want failed and left for the next launch to report", loaded.Update, err)
		}
		if r.trialRuns() != 0 || len(r.started()) != 0 {
			t.Fatal("the helper ran or relaunched without the lock")
		}
	})

	t.Run("an update that is not recorded", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		r.applier(desktopApplyFlags{id: id}, "prepare", nil).run(t.Context())
		if page := r.ui.lastPage(t); page.Title != "Agent Overflow could not be restarted." || len(r.started()) != 0 {
			t.Fatalf("page = %+v, started %q", page, r.started())
		}
	})

	t.Run("the relaunch cannot start", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		r.startErr = errors.New("exec format error")
		r.applier(desktopApplyFlags{migrate: true, schema: 88}, "prepare", nil).run(t.Context())
		if page := r.ui.lastPage(t); page.Title != "Agent Overflow could not be restarted." || r.ui.quits != 0 {
			t.Fatalf("page = %+v, quit %d", page, r.ui.quits)
		}
	})
}

func TestDesktopHelperArgvRoundTrips(t *testing.T) {
	wait := supervise.ProcessRef{PID: 42, Start: "1700000000.25"}
	relaunch := []string{"--data-dir", "/data", "--", "x"}
	for _, mode := range [][]string{{"--id", "0123456789abcdef"}, {"--migrate", "88"}} {
		args := supervise.DesktopHelperArgs(mode, wait, "/data", relaunch)
		if args[0] != supervise.DesktopApplyCommand {
			t.Fatalf("argv = %q", args)
		}
		flags, err := parseDesktopApplyFlags(args[1:])
		if err != nil {
			t.Fatalf("parse %q: %v", args, err)
		}
		want := desktopApplyFlags{wait: wait, dataDir: "/data", relaunch: relaunch}
		if mode[0] == "--id" {
			want.id = mode[1]
		} else {
			want.migrate, want.schema = true, 88
		}
		if !reflect.DeepEqual(flags, want) {
			t.Fatalf("parsed %q = %+v, want %+v", args, flags, want)
		}
	}
	for _, bad := range [][]string{
		{"--id", "0123456789abcdef", "--wait-pid", "42", "--wait-start", "s"},
		{"--id", "0123456789abcdef", "--wait-pid", "42", "--wait-start", "s", "extra"},
		{"--id", "0123456789abcdef", "--migrate", "88", "--wait-pid", "42", "--wait-start", "s", "--"},
		{"--wait-pid", "42", "--wait-start", "s", "--"},
		{"--id", "../x", "--wait-pid", "42", "--wait-start", "s", "--"},
		{"--id", "0123456789abcdef", "--wait-start", "s", "--"},
		{"--migrate", "88", "--wait-pid", "42", "--"},
	} {
		if _, err := parseDesktopApplyFlags(bad); err == nil {
			t.Errorf("parseDesktopApplyFlags(%q) accepted", bad)
		}
	}
}

func TestDesktopWaitFlagsBelongToTheDesktopBoot(t *testing.T) {
	flags, err := parseFlags([]string{"--wait-pid", "42", "--wait-start", "1700000000.25", "--data-dir", "/data"})
	if err != nil || flags.waitFor != (supervise.ProcessRef{PID: 42, Start: "1700000000.25"}) {
		t.Fatalf("parseFlags = %+v, %v", flags.waitFor, err)
	}
	if err := checkBackendVerbFlags(serveVerb, flags); err == nil {
		t.Fatal("serve accepted the desktop boot's wait flags")
	}
	for _, bad := range [][]string{
		{"--wait-pid", "42"},
		{"--wait-start", "s"},
		{"--wait-pid", "-1", "--wait-start", "s"},
		{"--wait-pid", "42", "--wait-start", "s", "--print-url-fd", "3"},
		{"--wait-pid", "42", "--wait-start", "s", "--frontend"},
		{"--wait-pid", "42", "--wait-start", "s", "--harness", "--data-dir", "/tmp/h"},
	} {
		if _, err := parseFlags(bad); err == nil {
			t.Errorf("parseFlags(%q) accepted", bad)
		}
	}
	// The flag package reads either form, and the relaunch drops both.
	if got := supervise.DesktopRelaunchArgs([]string{"-wait-pid=42", "--data-dir", "/data", "--wait-start", "s"}); !reflect.DeepEqual(got, []string{"--data-dir", "/data"}) {
		t.Fatalf("DesktopRelaunchArgs = %q", got)
	}
}

// TestHelperSettlesAnUpdateWhoseAppDidNotExit: an app that outlives the
// wait fails its update before anything ran; one that exited is not waited
// on.
func TestHelperSettlesAnUpdateWhoseAppDidNotExit(t *testing.T) {
	const id = "0123456789abcdef"
	r := newDesktopApplyRig(t)
	r.desktopUpdateFixture(id, 0)
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	running, err := supervise.ProcessRefOf(sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	flags := desktopApplyFlags{id: id, wait: running}
	if err := waitForReplacedApp(flags, 200*time.Millisecond, time.Now); err == nil ||
		!errors.Is(err, supervise.ErrProcessRunning) {
		t.Fatalf("waiting on a running app = %v", err)
	}
	loaded, _, err := supervise.LoadDesktopRecord(r.layout())
	if err != nil || loaded.Update.State != supervise.UpdateFailed || loaded.Update.Reported ||
		loaded.Update.Reason != "the previous version did not exit" {
		t.Fatalf("record = %+v, %v", loaded.Update, err)
	}

	// One that started a trial is left for the recovery table.
	r.desktopUpdateFixture(id, 1)
	if err := waitForReplacedApp(flags, 200*time.Millisecond, time.Now); err == nil {
		t.Fatal("waiting on a running app succeeded")
	}
	if loaded, _, err := supervise.LoadDesktopRecord(r.layout()); err != nil || loaded.Update.State != supervise.UpdatePending {
		t.Fatalf("record = %+v, %v; want the tried update pending", loaded.Update, err)
	}

	if err := sleeper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = sleeper.Wait()
	started := time.Now()
	if err := waitForReplacedApp(flags, 10*time.Second, time.Now); err != nil || time.Since(started) > 5*time.Second {
		t.Fatalf("waiting on an exited app = %v after %s", err, time.Since(started))
	}
}

// boot is a desktop boot's half over the rig, as reconcileDesktopUpdate
// builds it under the backend lock, with helpers recorded rather than
// started.
func (r *desktopApplyRig) boot() (*desktopBoot, *[][]string) {
	r.t.Helper()
	r.release()
	lock, err := acquireBackendInstanceLock(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.lock = lock
	r.mu.Unlock()
	b, err := newDesktopBoot(lock.file)
	if err != nil {
		r.t.Fatal(err)
	}
	var helpers [][]string
	b.startHelper = func(executable string, args []string, logPath string) error {
		helpers = append(helpers, append([]string{executable, logPath}, args...))
		return r.startErr
	}
	return &b, &helpers
}

func TestDesktopBootHandsTheRecordToItsHelper(t *testing.T) {
	const id = "0123456789abcdef"
	r := newDesktopApplyRig(t)
	record, _ := r.desktopUpdateFixture(id, 1)
	b, helpers := r.boot()
	// This boot is the version the update started from.
	b.update.Version = record.Update.From

	plan := b.reconcile(t.Context())
	if plan.launch || plan.page != nil || len(*helpers) != 1 {
		t.Fatalf("plan = %+v, helpers %q; want the update handed on", plan, *helpers)
	}
	helper := (*helpers)[0]
	if helper[0] != record.StagedPath || helper[1] != b.logPath || helper[2] != supervise.DesktopApplyCommand {
		t.Fatalf("helper = %q", helper)
	}
	flags, err := parseDesktopApplyFlags(helper[3:])
	if err != nil {
		t.Fatal(err)
	}
	self, err := supervise.CurrentProcessRef()
	if err != nil {
		t.Fatal(err)
	}
	if flags.id != id || flags.wait != self || flags.dataDir != r.root || !reflect.DeepEqual(flags.relaunch, b.relaunch) {
		t.Fatalf("the helper was started with %+v", flags)
	}

	// A helper that cannot start leaves the record for the next launch and
	// says so.
	r.startErr = errors.New("exec format error")
	b, _ = r.boot()
	b.update.Version = record.Update.From
	if plan := b.reconcile(t.Context()); plan.launch || plan.page == nil || plan.page.Title != "The update could not resume." {
		t.Fatalf("plan = %+v", plan)
	}

	// The gate hands a database the App refused to migrate live to this
	// version's helper, and nothing else.
	r.startErr = nil
	b, helpers = r.boot()
	refused := fmt.Errorf("app: open store: %w", &store.MigrationsPendingError{Database: 88, Build: 90, Pending: 2})
	if handled, page := b.startFailed(refused); !handled || page != nil {
		t.Fatalf("startFailed = %v, %+v; want the database handed to the helper", handled, page)
	}
	if got := (*helpers)[0]; got[0] != b.update.Executable || !reflect.DeepEqual(got[2:5], []string{supervise.DesktopApplyCommand, "--migrate", "88"}) {
		t.Fatalf("the migration helper = %q", got)
	}
	if handled, _ := b.startFailed(errors.New("store: disk I/O error")); handled || len(*helpers) != 1 {
		t.Fatalf("another start failure was handed on (%v, %d helpers)", handled, len(*helpers))
	}
	r.startErr = errors.New("exec format error")
	if handled, page := b.startFailed(refused); !handled || page == nil ||
		page.Title != "Agent Overflow could not start the database upgrade this version needs." || page.Log != b.logPath {
		t.Fatalf("a helper that cannot start = %v, %+v", handled, page)
	}
}

func TestDesktopBootShowsWhatBlocksIt(t *testing.T) {
	const id = "0123456789abcdef"
	t.Run("a committed update whose target is gone", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		record, _ := r.desktopUpdateFixture(id, 1)
		state, err := record.Settle(supervise.UpdateCommitted, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		record.State = state
		if err := supervise.SaveDesktopRecord(r.layout(), record); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(record.StagedPath); err != nil {
			t.Fatal(err)
		}
		b, helpers := r.boot()
		b.update.Version = record.Update.From
		plan := b.reconcile(t.Context())
		if plan.launch || plan.page == nil || plan.page.Title != "The update to v2.0.0 could not be installed." ||
			plan.page.Log != b.logPath || len(*helpers) != 0 {
			t.Fatalf("plan = %+v, helpers %q", plan, *helpers)
		}
	})

	t.Run("an interrupted update whose target is gone is rolled back", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		record, _ := r.desktopUpdateFixture(id, 1)
		if _, err := supervise.TakeSnapshot(r.layout(), r.dir, time.Now(), supervise.SnapshotOptions{UpdateID: id}); err != nil {
			t.Fatal(err)
		}
		r.writeDB("trial")
		if err := os.Remove(record.StagedPath); err != nil {
			t.Fatal(err)
		}
		b, helpers := r.boot()
		b.update.Version = record.Update.From
		plan := b.reconcile(t.Context())
		if !plan.launch || plan.failedTo != "2.0.0" || len(*helpers) != 0 {
			t.Fatalf("plan = %+v, helpers %q; want the rolled-back update reported", plan, *helpers)
		}
		if db := r.readDB(); db != "live" {
			t.Fatalf("database = %q; want the snapshot restored under the boot's lock", db)
		}
		loaded, _, err := supervise.LoadDesktopRecord(r.layout())
		if err != nil || loaded.Update.State != supervise.UpdateRolledBack || !loaded.Update.Reported {
			t.Fatalf("record = %+v, %v", loaded.Update, err)
		}
		// The boot's lines are in the update log the pages name.
		if data, err := os.ReadFile(b.logPath); err != nil || !strings.Contains(string(data), "rolled-back") {
			t.Fatalf("update log = %q, %v", data, err)
		}
	})

	t.Run("an unreadable record", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		if err := os.MkdirAll(filepath.Dir(r.layout().StatePath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.layout().StatePath(), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		b, _ := r.boot()
		if plan := b.reconcile(t.Context()); plan.launch || plan.page == nil || plan.page.Title != "Agent Overflow could not read its update record." {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("an update that did not apply is reported once", func(t *testing.T) {
		r := newDesktopApplyRig(t)
		record, _ := r.desktopUpdateFixture(id, 0)
		if err := supervise.SettleDesktopUpdate(r.dir, id, supervise.UpdateFailed, "the previous version did not exit", false, time.Now()); err != nil {
			t.Fatal(err)
		}
		b, _ := r.boot()
		b.update.Version = record.Update.From
		plan := b.reconcile(t.Context())
		if !plan.launch || plan.failedTo != "2.0.0" || plan.failedReason != "the previous version did not exit" {
			t.Fatalf("plan = %+v", plan)
		}
		if plan := b.reconcile(t.Context()); !plan.launch || plan.failedTo != "" {
			t.Fatalf("the second launch = %+v; want the failure reported once", plan)
		}
	})
}

func TestDesktopUpdateLogfAppendsToTheUpdateLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "app-update", "update.log")
	logf := desktopUpdateLogf(path)
	logf("updater: first %d", 1)
	logf("updater: second")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "updater: first 1") || !strings.HasSuffix(lines[1], "updater: second") {
		t.Fatalf("update log = %q", data)
	}
}

func TestDesktopInstallIsTheAppImageUnderOne(t *testing.T) {
	t.Setenv("APPIMAGE", "/home/me/Apps/Agent-Overflow.AppImage")
	executable, install, err := desktopInstall()
	if err != nil || executable != "/home/me/Apps/Agent-Overflow.AppImage" || install != executable {
		t.Fatalf("desktopInstall = %q, %q, %v", executable, install, err)
	}
	t.Setenv("APPIMAGE", "Agent-Overflow.AppImage")
	if _, _, err := desktopInstall(); err == nil {
		t.Fatal("a relative AppImage path was accepted")
	}
	t.Setenv("APPIMAGE", "")
	executable, install, err = desktopInstall()
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	if err != nil || executable != self || install != self {
		t.Fatalf("desktopInstall outside an AppImage = %q, %q, %v; want this executable", executable, install, err)
	}
}
