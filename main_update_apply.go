package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/startuppage"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/store"
	"agent-overflow/internal/supervise"
)

// The macOS and Linux desktop's in-app update and its no-live-migration gate
// (docs/specs/app-update.md, "macOS and Linux desktop"). A downloaded target,
// or a boot whose database this version would migrate, starts this binary's
// helper mode (supervise.DesktopApplyCommand). The helper waits for the app
// to exit, shows the loading page, runs the update or the migration under
// the backend lock and starts the install path again. The desktop boot
// reconciles the record and checks the database before the store opens and
// before any window (desktopGate). The steps and the recovery table live in
// supervise.DesktopUpdate; this file owns argv, the lock, the processes
// started and the pages shown. The window is main_update_apply_desktop.go.

// desktopWaitTimeout bounds the helper's wait for the app that started it,
// and the app's wait for the helper that started it, as the Wails swap's
// helper waits.
const desktopWaitTimeout = 30 * time.Second

// desktopUpdateDetails ends a page's detail. The page names the file.
const desktopUpdateDetails = "Details are in the update log."

// desktopApplyFlags is the helper's argv (supervise.DesktopHelperArgs).
type desktopApplyFlags struct {
	// id is the update the helper applies, or the migration it resumes.
	id string
	// migrate starts a migration of the database at migration version
	// schema, which the app does not migrate live.
	migrate bool
	schema  int
	// wait is the app the helper waits for.
	wait    supervise.ProcessRef
	dataDir string
	// relaunch is the app's own argv, which the install path is started
	// with again.
	relaunch []string
}

func parseDesktopApplyFlags(args []string) (desktopApplyFlags, error) {
	name := supervise.DesktopApplyCommand
	var flags desktopApplyFlags
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&flags.id, "id", "", "the update or migration to continue")
	schema := set.Int("migrate", -1, "the migration version of a database the app does not migrate live")
	set.IntVar(&flags.wait.PID, supervise.DesktopWaitPIDFlag, 0, "the app to wait for")
	set.StringVar(&flags.wait.Start, supervise.DesktopWaitStartFlag, "", "its start time")
	set.StringVar(&flags.dataDir, "data-dir", "", "the data root, as for a boot")
	if err := set.Parse(args); err != nil {
		return desktopApplyFlags{}, fmt.Errorf("%s: %w", name, err)
	}
	if parsed := len(args) - set.NArg(); parsed == 0 || args[parsed-1] != "--" {
		return desktopApplyFlags{}, fmt.Errorf("%s: the app's arguments must follow --", name)
	}
	flags.relaunch = set.Args()
	switch {
	case flags.id != "" && *schema >= 0:
		return desktopApplyFlags{}, fmt.Errorf("%s: --id and --migrate are exclusive", name)
	case flags.id != "":
		if !supervise.ValidUpdateID(flags.id) {
			return desktopApplyFlags{}, fmt.Errorf("%s: --id %q is not an update id", name, flags.id)
		}
	case *schema >= 0:
		flags.migrate, flags.schema = true, *schema
	default:
		return desktopApplyFlags{}, fmt.Errorf("%s: --id or --migrate is required", name)
	}
	if flags.wait.PID <= 0 || flags.wait.Start == "" {
		return desktopApplyFlags{}, fmt.Errorf("%s: --%s and --%s are required", name, supervise.DesktopWaitPIDFlag, supervise.DesktopWaitStartFlag)
	}
	return flags, nil
}

// runDesktopApply is the helper: it waits for the app that started it to
// exit, then runs the window. Its output is the update log
// (supervise.StartDesktopHelper).
func runDesktopApply(args []string) int {
	flags, err := parseDesktopApplyFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !desktopUpdateTrial {
		fmt.Fprintf(os.Stderr, "%s: this build does not apply desktop updates\n", supervise.DesktopApplyCommand)
		return 2
	}
	dataDirRoot = flags.dataDir
	if err := waitForReplacedApp(flags.wait, desktopWaitTimeout); err != nil {
		log.Printf("updater: %v", err)
		return 1
	}
	return runDesktopApplyWindow(flags)
}

// waitForReplacedApp waits for the app that started the helper to exit, so
// the helper claims the single-instance identity and the lock after it. The
// record of an app that did not exit is left as it is: only a process that
// holds the backend lock writes it, and that app holds it. The next launch
// recovers it under the lock (a pending update without a trial settles
// failed and is reported).
func waitForReplacedApp(wait supervise.ProcessRef, timeout time.Duration) error {
	if err := supervise.WaitForExit(context.Background(), wait, timeout); err != nil {
		return fmt.Errorf("the app that started this helper (pid %d) did not exit; the next launch recovers the update: %w", wait.PID, err)
	}
	return nil
}

// desktopInstall is this binary as a helper or a relaunch starts it, and
// what the user starts for it (supervise.DesktopInstallPath). An
// AppImage's executable lives in a mount that ends with its process, so
// under one both are the .AppImage file, which mounts it again.
func desktopInstall() (executable, install string, err error) {
	if path := os.Getenv("APPIMAGE"); path != "" && appimage.Running() {
		if !filepath.IsAbs(path) {
			return "", "", fmt.Errorf("the AppImage path %q is not absolute", path)
		}
		return path, path, nil
	}
	executable, err = os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return "", "", fmt.Errorf("locate this executable: %w", err)
	}
	install, err = supervise.DesktopInstallPath(executable, runtime.GOOS)
	return executable, install, err
}

// databaseSchemaVersion reads the migration version of the database in
// dataDir under the lock the caller holds.
func databaseSchemaVersion(dataDir string) func() (int, error) {
	return func() (int, error) {
		return store.ReadSchemaVersion(filepath.Join(dataDir, supervise.DatabaseFiles()[0]))
	}
}

// newDesktopUpdate is the DesktopUpdate of this binary over the boot's data
// root, without its lock and its progress. The trial is this process's
// executable, which lives as long as the process that runs the trial.
func newDesktopUpdate(logf func(string, ...any)) (supervise.DesktopUpdate, error) {
	dataDir := bootSettingsDir()
	if dataDir == "" {
		return supervise.DesktopUpdate{}, errors.New("cannot determine the data directory")
	}
	executable, install, err := desktopInstall()
	if err != nil {
		return supervise.DesktopUpdate{}, err
	}
	trialBinary, err := os.Executable()
	if err != nil {
		return supervise.DesktopUpdate{}, fmt.Errorf("locate this executable: %w", err)
	}
	trialArgs := []string{supervise.UpdateTrialCommand}
	if dataDirRoot != "" {
		trialArgs = append(trialArgs, "--data-dir", dataDirRoot)
	}
	return supervise.DesktopUpdate{
		DataDir:       dataDir,
		Version:       version,
		Executable:    executable,
		InstallPath:   install,
		Trial:         supervise.TrialRunOptions{Binary: trialBinary, Args: trialArgs},
		SchemaVersion: databaseSchemaVersion(dataDir),
		Files:         supervise.NativeDesktopFiles(),
		Now:           time.Now,
		Logf:          logf,
	}, nil
}

// desktopUpdateLogPath is the update log of the boot's data root.
func desktopUpdateLogPath() (string, error) {
	layout, err := supervise.NewAppUpdateLayout(bootSettingsDir())
	if err != nil {
		return "", err
	}
	return supervise.DesktopUpdateLogPath(layout), nil
}

// desktopUpdateLogf logs to this process's log and appends the line to the
// update log, which the pages name. The helper's own output already is the
// update log, so only the app uses it.
func desktopUpdateLogf(path string) func(string, ...any) {
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		log.Print(line)
		out, err := supervise.OpenDesktopUpdateLog(path)
		if err != nil {
			log.Printf("updater: open the update log: %v", err)
			return
		}
		log.New(out, "", log.LstdFlags).Print(line)
		if err := out.Close(); err != nil {
			log.Printf("updater: write the update log: %v", err)
		}
	}
}

// desktopApplyUI is the helper's window. Its methods are unexported: the
// window is also a Wails service, whose exported methods a page can call.
type desktopApplyUI interface {
	// loading shows the loading page, which progress fills. While it
	// shows, closing the window hides it and the helper continues.
	loading()
	progress(p startupprogress.Progress)
	// fail shows page in front of the user. Closing the window then ends
	// the helper.
	fail(page startuppage.Failure)
	// quit ends the helper.
	quit()
}

// desktopApplier is the helper's work once its window shows.
type desktopApplier struct {
	flags desktopApplyFlags
	// update is newDesktopUpdate's; run adds the lock and the progress.
	update supervise.DesktopUpdate
	// acquireLock takes the data root's backend lock for the helper's life.
	acquireLock func(ctx context.Context) (*os.File, error)
	// start starts the app at install with args, detached.
	start func(install string, args []string) error
	ui    desktopApplyUI
	// logPath is the update log the pages name.
	logPath string
	// retryMethod is the bound method the Retry button calls.
	retryMethod string
	logf        func(string, ...any)

	mu sync.Mutex
	// retryOffered is set while a failure page offers Retry.
	retryOffered bool
}

// run applies the update, or runs the migration, the flags name, then
// starts the install path or shows why it does not.
func (a *desktopApplier) run(ctx context.Context) {
	a.ui.loading()
	if a.flags.migrate {
		a.update.Progress = a.ui.progress
		if !a.lock(ctx, true) {
			return
		}
		a.finishMigration(a.update.Migrate(ctx, supervise.DesktopMigration{Schema: a.flags.schema}), a.update.InstallPath)
		return
	}
	id := a.flags.id
	layout, err := supervise.NewAppUpdateLayout(a.update.DataDir)
	var record supervise.DesktopRecord
	found := false
	if err == nil {
		record, found, err = supervise.LoadDesktopRecord(layout)
	}
	switch {
	case err != nil:
		a.logf("updater: update %s: %v", id, err)
		a.fail("Agent Overflow could not read its update record.", "Nothing was started, so the data is left as it is. "+desktopUpdateDetails)
		return
	case !found || record.Update.ID != id:
		a.logf("updater: update %s is not recorded; there is nothing to apply", id)
		a.fail("Agent Overflow could not be restarted.", "Start Agent Overflow again. "+desktopUpdateDetails)
		return
	case record.Migration():
		a.update.Progress = a.ui.progress
		if !a.lock(ctx, true) {
			return
		}
		a.finishMigration(a.update.ResumeMigration(ctx, record), record.InstallPath)
		return
	}
	to := record.Update.To
	a.update.Progress = func(p startupprogress.Progress) {
		p.UpdatingTo = to
		a.ui.progress(p)
	}
	now := time.Now().UnixMilli()
	a.update.Progress(startupprogress.Progress{Phase: "update.start", Detail: "Preparing the update", StartedAt: now, UpdatedAt: now})
	if !a.lock(ctx, false) {
		return
	}
	end, err := a.update.Apply(ctx, id)
	switch {
	case err != nil:
		a.logf("updater: update %s: %v", id, err)
		a.fail("The update could not finish.", "Start Agent Overflow again to finish it. "+desktopUpdateDetails)
		return
	case !end.Settled():
		a.logf("updater: update %s: %s", id, end.Reason)
		a.fail("The update did not finish, and the database backup could not be restored.",
			"Nothing was started, so the data is left as it is. Start Agent Overflow again to retry the restore. "+desktopUpdateDetails)
		return
	}
	a.logf("updater: update %s ended %s", id, end.State)
	a.relaunch(record.InstallPath)
}

// lock takes the backend lock. Another backend on the data root refuses
// the run, and the record is left as it is: only the lock's holder writes
// it, and the holder may be a helper of the same record between its lock
// and its trial. The next launch recovers it under the lock.
func (a *desktopApplier) lock(ctx context.Context, migration bool) bool {
	lock, err := a.acquireLock(ctx)
	if err == nil {
		a.update.Lock = lock
		return true
	}
	a.logf("updater: the database is in use by another Agent Overflow backend: %v", err)
	if migration {
		a.fail("Agent Overflow could not start the database upgrade this version needs.",
			"Another Agent Overflow backend is using the data folder, so the upgrade did not run. Close it, then start Agent Overflow again. "+desktopUpdateDetails)
		return false
	}
	a.fail("The update could not start.",
		"Another Agent Overflow backend is using the data folder, so the update did not run. Close it, then start Agent Overflow again. "+desktopUpdateDetails)
	return false
}

// finishMigration starts the install path after a migration that
// committed, or shows why it did not, with Retry when the failure memory
// stopped it.
func (a *desktopApplier) finishMigration(end supervise.MigrationEnd, install string) {
	if end.Launch {
		a.relaunch(install)
		return
	}
	page := startuppage.Failure{Title: end.Title, Detail: end.Detail, Log: a.logPath}
	if end.Retry {
		a.mu.Lock()
		a.retryOffered = true
		a.mu.Unlock()
		page.Retry = a.retryMethod
	}
	a.ui.fail(page)
}

// retryMigration runs the migration the failure memory stopped, which the
// person asked for on its page. Only a page that offered it runs it, once.
func (a *desktopApplier) retryMigration(ctx context.Context) error {
	a.mu.Lock()
	offered := a.retryOffered
	a.retryOffered = false
	a.mu.Unlock()
	if !offered {
		return errors.New("there is no database upgrade to retry")
	}
	a.ui.loading()
	a.finishMigration(a.update.Migrate(ctx, supervise.DesktopMigration{Schema: a.flags.schema, Retry: true}), a.update.InstallPath)
	return nil
}

// relaunch starts the app at install, which waits for this helper to exit,
// with the arguments it was started with, and ends the helper.
func (a *desktopApplier) relaunch(install string) {
	self, err := supervise.CurrentProcessRef()
	if err == nil {
		err = a.start(install, supervise.DesktopAppArgs(self, a.flags.relaunch))
	}
	if err != nil {
		a.logf("updater: start %s: %v", install, err)
		a.fail("Agent Overflow could not be restarted.", "Start Agent Overflow again. "+desktopUpdateDetails)
		return
	}
	a.logf("updater: started %s", install)
	a.ui.quit()
}

func (a *desktopApplier) fail(title, detail string) {
	a.ui.fail(startuppage.Failure{Title: title, Detail: detail, Log: a.logPath})
}

// desktopGate is the desktop boot's no-live-migration gate (rule 7). The
// boot's store refuses pending migrations whatever happens here. The update
// half hands a database with pending migrations to this version's helper
// before any window opens; a boot without one says why on the failure page.
type desktopGate struct {
	// boot is the update half, nil when unavailable says why there is none.
	boot        *desktopBoot
	unavailable error
	logPath     string
	logf        func(string, ...any)
}

// newDesktopGate is this boot's gate under lock, the backend lock the boot
// holds, which the record's recovery runs under. Its lines go to the update
// log too.
func newDesktopGate(lock *os.File) desktopGate {
	logPath, err := desktopUpdateLogPath()
	if err != nil {
		log.Printf("updater: in-app updates with a trial and the database upgrade helper are unavailable: %v", err)
		return desktopGate{unavailable: err, logf: log.Printf}
	}
	logf := desktopUpdateLogf(logPath)
	boot, err := newDesktopBoot(lock, logPath, logf)
	if err != nil {
		logf("updater: in-app updates with a trial and the database upgrade helper are unavailable: %v", err)
		return desktopGate{unavailable: err, logPath: logPath, logf: logf}
	}
	return desktopGate{boot: &boot, logPath: logPath, logf: logf}
}

// reconcile is the update half's plan; a boot without one launches, and its
// store refuses a database it would migrate.
func (g desktopGate) reconcile(ctx context.Context) desktopBootPlan {
	if g.boot == nil {
		return desktopBootPlan{launch: true}
	}
	return g.boot.reconcile(ctx)
}

// startFailed is the page for a start the store refused because the
// database needs migrating (store.MigrationsPendingError), nil for any other
// failure. The boot routes such a database to the helper before the window
// opens, so this is a boot without the update half, or the store's guard
// for a database the boot's check did not find pending. Nothing was
// changed either way.
func (g desktopGate) startFailed(err error) *startuppage.Failure {
	var pending *store.MigrationsPendingError
	if !errors.As(err, &pending) {
		return nil
	}
	cause := g.unavailable
	if cause == nil {
		cause = err
	}
	logf := g.logf
	if logf == nil {
		logf = log.Printf
	}
	logf("updater: the database needs an upgrade this launch cannot start: %v", cause)
	reason := strings.TrimSuffix(strings.TrimSpace(cause.Error()), ".")
	return &startuppage.Failure{
		Title:  "Agent Overflow could not start the database upgrade this version needs.",
		Detail: "Reason: " + reason + ". Nothing was changed. " + desktopUpdateDetails,
		Log:    g.logPath,
	}
}

// desktopBoot is the desktop boot's half of the update: it runs under the
// backend lock, while this process holds the single-instance identity and
// before the store opens.
type desktopBoot struct {
	update supervise.DesktopUpdate
	// startHelper starts a helper detached (supervise.StartDesktopHelper).
	startHelper func(executable string, args []string, logPath string) error
	// dataDirFlag is the boot's --data-dir, which the helper gets too.
	dataDirFlag string
	// relaunch is this boot's argv without the wait flags.
	relaunch []string
	logPath  string
	logf     func(string, ...any)
}

// desktopBootPlan is what the boot does after reconciling the record.
type desktopBootPlan struct {
	// launch starts the App. Otherwise a helper continues the record or
	// migrates the database and the boot ends without a window, or page
	// says why nothing starts.
	launch bool
	page   *startuppage.Failure
	// updatingTo is the update this launch finishes, which the startup
	// report names.
	updatingTo string
	// failedTo and failedReason are an update from this version that did
	// not apply, for the updater's notice.
	failedTo, failedReason string
}

// newDesktopBoot is the update's half of this desktop boot, under lock. Its
// lines go to logf, and the helpers it starts get this process's
// environment without an AppImage's mount.
func newDesktopBoot(lock *os.File, logPath string, logf func(string, ...any)) (desktopBoot, error) {
	update, err := newDesktopUpdate(logf)
	if err != nil {
		return desktopBoot{}, err
	}
	update.Lock = lock
	env := appimage.ScrubInherited()
	return desktopBoot{
		update: update,
		startHelper: func(executable string, args []string, logPath string) error {
			return supervise.StartDesktopHelper(executable, args, logPath, env)
		},
		dataDirFlag: dataDirRoot,
		relaunch:    supervise.DesktopRelaunchArgs(os.Args[1:]),
		logPath:     logPath,
		logf:        logf,
	}, nil
}

// handoff is the running app's side of an in-app update (appupdate.DesktopTrial).
func (b desktopBoot) handoff() supervise.DesktopHandoff {
	return supervise.DesktopHandoff{
		DataDir:      b.update.DataDir,
		DataDirFlag:  b.dataDirFlag,
		Version:      b.update.Version,
		Executable:   b.update.Executable,
		GOOS:         runtime.GOOS,
		RelaunchArgs: b.relaunch,
		Preflight:    supervise.PreflightBinary,
		Start:        b.startHelper,
		Logf:         b.logf,
	}
}

// reconcile applies the record's recovery (supervise.DesktopUpdate.Reconcile)
// and starts the helper it hands to. A launch whose database this version
// would migrate starts the helper instead (migrate), before any window
// opens.
func (b desktopBoot) reconcile(ctx context.Context) desktopBootPlan {
	decision, err := b.update.Reconcile(ctx)
	if err != nil {
		b.logf("updater: reconcile the update record: %v", err)
		return b.refuse("Agent Overflow could not read its update record.",
			"Nothing was started, so the data is left as it is. "+desktopUpdateDetails)
	}
	record := decision.Record
	switch decision.Action {
	case supervise.DesktopHandOff:
		work := "update"
		if record.Migration() {
			work = "database upgrade"
		}
		b.logf("updater: continuing %s %s with %s", work, record.Update.ID, decision.Helper)
		if err := b.hand(decision.Helper, []string{"--id", record.Update.ID}); err != nil {
			b.logf("updater: start %s: %v", decision.Helper, err)
			return b.refuse("The "+work+" could not resume.", "Start Agent Overflow again. "+desktopUpdateDetails)
		}
		return desktopBootPlan{}
	case supervise.DesktopBlocked:
		b.logf("updater: update %s blocks this launch: %s", record.Update.ID, decision.Title)
		return b.refuse(decision.Title, decision.Detail)
	}
	if schema, pending := b.pendingMigrations(); pending {
		if err := b.migrate(schema); err != nil {
			b.logf("updater: start the database upgrade: %v", err)
			return b.refuse("Agent Overflow could not start the database upgrade this version needs.",
				"Nothing was changed. Start Agent Overflow again. "+desktopUpdateDetails)
		}
		return desktopBootPlan{}
	}
	return desktopBootPlan{launch: true, updatingTo: decision.UpdatingTo, failedTo: decision.FailedTo, failedReason: decision.FailedReason}
}

// pendingMigrations reads the database's migration version under the boot's
// lock and asks the store whether this build migrates it
// (store.PendingMigrations). A missing database is new. One whose version
// cannot be read launches: the store's open reports why, and its refusal
// still guards the database.
func (b desktopBoot) pendingMigrations() (schema int, pending bool) {
	schema, err := b.update.SchemaVersion()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, false
	case err != nil:
		b.logf("updater: read the database's schema version: %v", err)
		return 0, false
	}
	return schema, store.PendingMigrations(schema) != nil
}

// migrate hands the database this boot does not migrate live, at migration
// version schema, to a helper of this version.
func (b desktopBoot) migrate(schema int) error {
	b.logf("updater: this version does not migrate its database (schema v%d) live; its helper migrates it through a trial", schema)
	return b.hand(b.update.Executable, []string{"--migrate", strconv.Itoa(schema)})
}

// hand starts helper with mode. It waits for this process to exit.
func (b desktopBoot) hand(helper string, mode []string) error {
	self, err := supervise.CurrentProcessRef()
	if err != nil {
		return err
	}
	return b.startHelper(helper, supervise.DesktopHelperArgs(mode, self, b.dataDirFlag, b.relaunch), b.logPath)
}

func (b desktopBoot) refuse(title, detail string) desktopBootPlan {
	return desktopBootPlan{page: &startuppage.Failure{Title: title, Detail: detail, Log: b.logPath}}
}
