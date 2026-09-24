//go:build windows

// update_trial.go is the launcher's side of trial-and-rollback updates
// (docs/specs/app-update.md): the old launcher's hand-off, the new launcher's
// --update-preflight and --update-apply modes, and the recovery a launcher at
// the install path runs before it starts WSL. The order and the recovery
// table live in wsllauncher.UpdateSequence; this file supplies the Windows
// side effects.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-overflow/internal/selfupdate"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	// updatePreflightTimeout bounds the new launcher's preflight: writing
	// and installing its payload into WSL, then asking it what it is and
	// whether the snapshot fits.
	updatePreflightTimeout = 3 * time.Minute
	// updateParentExitTimeout is how long a launcher waits for the one it
	// replaces to exit, as the Wails swap helper does.
	updateParentExitTimeout = 30 * time.Second
)

// errLegacyTarget is a target launcher that predates the trial flow. It
// writes no preflight answer, and the existing swap installs it.
var errLegacyTarget = errors.New("the new launcher predates trial updates")

// beginTrialUpdate is steps 2 and 3 of the Windows sequence: stage the new
// launcher, have it stage and check its payload, record the update durably,
// start the new launcher and quit. It returns errLegacyTarget when the target
// cannot run the trial, and nil only once the app is quitting.
func (a *launcherApp) beginTrialUpdate(directive selfupdate.InstallDirective, staged string, digest []byte) error {
	// The same version has the same schema: nothing for a trial to prove.
	if directive.Version == payloadVersion {
		return errLegacyTarget
	}
	a.mu.Lock()
	distro, stable := a.payloadDistro, a.payloadPath
	a.mu.Unlock()
	if distro == "" || stable == "" {
		return errors.New("the running backend's install location is unknown")
	}
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return errors.New(`resolve %APPDATA%\agent-overflow`)
	}
	install, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this launcher: %w", err)
	}
	id, err := wsllauncher.NewUpdateID()
	if err != nil {
		return err
	}
	launcherPath := wsllauncher.StagedLauncherPath(dir, id)
	// StageCopy verifies the digest of the bytes it writes, so the file the
	// new launcher runs from is the one the backend verified.
	if _, err := selfupdate.StageCopy(staged, filepath.Dir(launcherPath), filepath.Base(launcherPath), digest); err != nil {
		return fmt.Errorf("stage the new launcher: %w", err)
	}
	discardLauncher := func() {
		if err := os.Remove(launcherPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("updater: remove the staged launcher: %v", err)
		}
	}
	if err := probeWritable(filepath.Dir(install)); err != nil {
		discardLauncher()
		return fmt.Errorf("the folder %s cannot be written, so the update could not be installed: %w", filepath.Dir(install), err)
	}

	answerPath := wsllauncher.PreflightAnswerPath(dir, id)
	answer, found, err := runLauncherPreflight(launcherPath, answerPath, id, distro, stable)
	if removeErr := os.Remove(answerPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		log.Printf("updater: remove the preflight answer: %v", removeErr)
	}
	if err != nil {
		discardLauncher()
		return err
	}
	if !found {
		discardLauncher()
		return errLegacyTarget
	}
	if err := answer.Err(); err != nil {
		discardLauncher()
		return err
	}
	record := supervise.LauncherRecord{
		Distro: distro, StablePayload: stable, StagedPayload: answer.StagedPayload,
		StagedLauncher: launcherPath, InstallPath: install, TargetFingerprint: answer.Fingerprint,
	}
	payloads := newUpdatePayloads()
	if answer.Version != directive.Version {
		discardLauncher()
		if err := payloads.RemoveStagedPayload(context.Background(), record); err != nil {
			log.Printf("updater: remove the staged backend: %v", err)
		}
		return fmt.Errorf("the downloaded %s reports version %s", directive.Version, answer.Version)
	}
	recordPath := supervise.LauncherRecordPath(dir, launcherRuntimeMode(), distro)
	if _, err := wsllauncher.BeginLauncherUpdate(recordPath, record, payloadVersion, directive.Version, id, time.Now()); err != nil {
		discardLauncher()
		if removeErr := payloads.RemoveStagedPayload(context.Background(), record); removeErr != nil {
			log.Printf("updater: remove the staged backend: %v", removeErr)
		}
		return fmt.Errorf("record the update: %w", err)
	}
	if err := startApplier(recordPath, launcherPath, id, distro); err != nil {
		// Settled before the error is reported, so no later launch tries
		// to resume an update whose new launcher never ran.
		if settleErr := wsllauncher.SettleLauncherUpdate(recordPath, id, supervise.UpdateFailed,
			"the new launcher could not be started: "+err.Error(), time.Now()); settleErr != nil {
			log.Printf("updater: settle update %s: %v", id, settleErr)
		}
		return fmt.Errorf("start the new launcher: %w", err)
	}
	log.Printf("updater: update %s to %s handed to %s", id, directive.Version, launcherPath)
	armUpdateExitWatchdog(updateRestartExitWatchdogDelay)
	a.wails.Quit()
	return nil
}

// runLauncherPreflight runs the new launcher's --update-preflight and reads
// its answer. found is false when the launcher wrote none.
func runLauncherPreflight(launcherPath, answerPath, id, distro, stable string) (wsllauncher.PreflightAnswer, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), updatePreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcherPath, launcherArgs(
		"--update-preflight", answerPath, "--update-id", id, "--distro", distro, "--update-stable", stable)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		// A launcher that predates the flow exits non-zero on the unknown
		// flag, which is not a failure: the answer file decides.
		log.Printf("updater: the new launcher's preflight exited: %v", err)
		if ctx.Err() != nil {
			return wsllauncher.PreflightAnswer{}, false, fmt.Errorf("the new launcher did not finish its checks within %s", updatePreflightTimeout)
		}
	}
	return wsllauncher.ReadPreflightAnswer(answerPath)
}

// runUpdatePreflightMode is `--update-preflight`: install this launcher's
// payload beside the stable one and ask it what it is and whether the
// update's database snapshot fits (wsllauncher.PreflightStagedPayload). The
// answer file is the whole result.
func runUpdatePreflightMode(flags launcherFlags) int {
	answer := preflightStagedPayload(flags)
	if !answer.OK {
		log.Printf("updater: preflight for update %s failed: %s", flags.UpdateID, answer.Reason)
	}
	if err := wsllauncher.WritePreflightAnswer(flags.UpdatePreflight, answer); err != nil {
		log.Printf("updater: write the preflight answer: %v", err)
		return 1
	}
	return 0
}

func preflightStagedPayload(flags launcherFlags) wsllauncher.PreflightAnswer {
	ctx, cancel := context.WithTimeout(context.Background(), updatePreflightTimeout)
	defer cancel()
	return wsllauncher.PreflightStagedPayload(ctx, preflightHost{launcherUpdateHost{UpdatePayloads: newUpdatePayloads()}},
		wsllauncher.PreflightRequest{
			ID: flags.UpdateID, Distro: flags.Distro, Stable: flags.UpdateStable,
			Version: payloadVersion, Fingerprint: embeddedPayloadFingerprint(),
		})
}

// preflightHost is wsllauncher.PreflightHost on Windows.
type preflightHost struct {
	launcherUpdateHost
}

// InstallEmbeddedPayload writes this launcher's payload into the distro.
func (preflightHost) InstallEmbeddedPayload(ctx context.Context, distro, staged string) error {
	tmp, err := writeEmbeddedPayload()
	if err != nil {
		return fmt.Errorf("write the new backend: %w", err)
	}
	defer os.Remove(tmp)
	return wsllauncher.InstallPayload(ctx, distro, tmp, staged)
}

// waitForParentLauncher waits for the launcher that started this one to
// exit, so this one can claim the single-instance identity. The wait holds
// a handle to the process whose creation time matches, so a reused process
// id is never waited on.
func waitForParentLauncher(parent supervise.ProcessRef) error {
	if err := supervise.WaitForExit(context.Background(), parent, updateParentExitTimeout); err != nil {
		return fmt.Errorf("the previous launcher (pid %d): %w", parent.PID, err)
	}
	return nil
}

// failUpdateBeforeApply settles an update whose previous launcher never
// exited, so the next launch reports it rather than resuming it.
func failUpdateBeforeApply(id, distro string, cause error) {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return
	}
	recordPath := supervise.LauncherRecordPath(dir, launcherRuntimeMode(), distro)
	if err := wsllauncher.SettleLauncherUpdate(recordPath, id, supervise.UpdateFailed, cause.Error(), time.Now()); err != nil {
		log.Printf("updater: settle update %s: %v", id, err)
	}
}

// startLauncherAfterThis starts a launcher that waits for this one to exit
// before it claims the single-instance identity.
func startLauncherAfterThis(path string, args ...string) error {
	cmd, err := launcherAfterThis(path, args...)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// startApplier starts the launcher at path to apply update id once this one
// exits, and names it in the record before this one can exit, so a launch
// in between joins the update instead of settling it.
func startApplier(recordPath, path, id, distro string) error {
	cmd, err := launcherAfterThis(path, "--update-apply", id, "--distro", distro)
	if err != nil {
		return err
	}
	if err := wsllauncher.StartApplier(cmd, recordPath, id); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// launcherAfterThis is the command for a launcher that outlives this one
// and waits for it to exit.
func launcherAfterThis(path string, args ...string) (*exec.Cmd, error) {
	self, err := supervise.CurrentProcessRef()
	if err != nil {
		return nil, fmt.Errorf("name this launcher: %w", err)
	}
	cmd := exec.Command(path, launcherArgs(append(args, "--wait-pid", strconv.Itoa(self.PID), "--wait-start", self.Start)...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	return cmd, nil
}

// launcherArgs carries this launcher's profile to one it starts.
func launcherArgs(args ...string) []string {
	if activeProfile != "" {
		return append([]string{"--profile", activeProfile}, args...)
	}
	return args
}

// probeWritable proves a file can be created in dir, which publishing the
// new launcher beside the old one needs.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".agent-overflow-update-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeErr := f.Close()
	if err := os.Remove(name); err != nil {
		return err
	}
	return closeErr
}

func newUpdatePayloads() wsllauncher.UpdatePayloads {
	return wsllauncher.UpdatePayloads{Runner: wsllauncher.UpdateCommandRunner{Logf: log.Printf}}
}

// updateSequence builds the sequence for the record of this runtime
// profile and distro, with the loading page as its progress sink.
func (a *launcherApp) updateSequence(dir, distro string) wsllauncher.UpdateSequence {
	return wsllauncher.UpdateSequence{
		RecordPath: supervise.LauncherRecordPath(dir, launcherRuntimeMode(), distro),
		Host:       launcherUpdateHost{UpdatePayloads: newUpdatePayloads(), configDir: dir},
		Progress:   a.loading.setProgress,
		Logf:       log.Printf,
	}
}

// runUpdateApply is --update-apply: run the update from its record with the
// loading page showing progress, then start the install path and quit. The
// window hides instead of closing while the update runs. A launch while it
// runs joins the update (wsllauncher.Join): this window hides for it, and
// once the update ends this launcher quits and leaves the rest to it.
func (a *launcherApp) runUpdateApply(id, distro string) {
	w := a.win()
	a.updateRunning.Store(true)
	w.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if a.updateRunning.Load() {
			event.Cancel()
			w.Hide()
		}
	})
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		a.updateRunning.Store(false)
		a.showUpdateFailure("The update could not start.", `%APPDATA% could not be resolved.`)
		return
	}
	a.loading.begin(time.Now())
	sequence := a.updateSequence(dir, distro)
	go wsllauncher.WatchJoiner(context.Background(), sequence.RecordPath, wsllauncher.UpdateJoinPoll, a.yieldToJoiner, log.Printf)
	self, err := supervise.CurrentProcessRef()
	if err != nil {
		a.updateRunning.Store(false)
		log.Printf("updater: update %s: name this launcher: %v", id, err)
		a.showUpdateFailure("The update could not start.", "Start Agent Overflow again to retry it. Details are in the launcher log.")
		return
	}
	sequence.Self = self
	publisher := wsllauncher.NewProgressPublisher(wsllauncher.UpdateProgressPath(sequence.RecordPath), wsllauncher.UpdateJoinPoll, log.Printf)
	show := sequence.Progress
	sequence.Progress = func(p startupprogress.Progress) {
		show(p)
		publisher.Report(p)
	}
	record, found, err := supervise.LoadLauncherRecord(sequence.RecordPath)
	if err == nil && found {
		target := record.Update.To
		progress := sequence.Progress
		sequence.Progress = func(p startupprogress.Progress) {
			p.UpdatingTo = target
			progress(p)
		}
		sequence.Progress(startupprogress.Progress{Phase: "update.start", Detail: "Preparing the update"})
	}
	end, err := sequence.Apply(context.Background(), id)
	if closeErr := publisher.Close(); closeErr != nil {
		log.Printf("updater: update %s: %v", id, closeErr)
	}
	a.updateRunning.Store(false)
	if joined, joinedErr := wsllauncher.JoinerRunning(sequence.RecordPath); joinedErr != nil {
		log.Printf("updater: look for a launch joined to update %s: %v", id, joinedErr)
	} else if joined {
		log.Printf("updater: update %s ended (%s, %v); the joined launch takes over", id, end.State, err)
		a.wails.Quit()
		return
	}
	switch {
	case errors.Is(err, wsllauncher.ErrNoUpdateRecord):
		log.Printf("updater: update %s has no record; starting the installed version", id)
	case err != nil:
		log.Printf("updater: update %s: %v", id, err)
		a.showUpdateFailure("The update could not finish.",
			"Start Agent Overflow again to finish it. Details are in the launcher log.")
		return
	case !end.Settled():
		log.Printf("updater: update %s: %s", id, end.Reason)
		a.showUpdateFailure("The update did not finish, and the database backup could not be restored.",
			"Nothing was started, so the data is left as it is. Start Agent Overflow again to retry the restore. Details are in the launcher log.")
		return
	default:
		log.Printf("updater: update %s ended %s", id, end.State)
	}
	a.relaunchInstallPath(sequence.RecordPath)
}

// yieldToJoiner is WatchJoiner's call while a launch that joined the update
// runs: the window hides for it while the update runs, and once the update
// ended, as when this window shows why it could not finish, this launcher
// quits so the joined launch decides.
func (a *launcherApp) yieldToJoiner() bool {
	if a.updateRunning.Load() {
		if w := a.win(); w != nil && a.yielded.CompareAndSwap(false, true) {
			log.Printf("updater: a launch joined the update; hiding this window")
			w.Hide()
		}
		return false
	}
	log.Printf("updater: a launch joined the ended update; leaving it to that launch")
	a.wails.Quit()
	return true
}

// relaunchInstallPath starts the launcher at the install path, which waits
// for this one to exit, and quits.
func (a *launcherApp) relaunchInstallPath(recordPath string) {
	record, found, err := supervise.LoadLauncherRecord(recordPath)
	if err != nil || !found {
		log.Printf("updater: no install path to start (found=%v): %v", found, err)
		a.showUpdateFailure("The update finished, but Agent Overflow could not be restarted.", "Start Agent Overflow again.")
		return
	}
	if err := startLauncherAfterThis(record.InstallPath); err != nil {
		log.Printf("updater: start %s: %v", record.InstallPath, err)
		a.showUpdateFailure("The update finished, but Agent Overflow could not be restarted.", "Start Agent Overflow again.")
		return
	}
	a.wails.Quit()
}

// reconcileUpdate runs the recovery table for distro's record before its
// backend starts, first waiting out an update another launcher is applying
// while the window shows its progress. It returns false when this launch
// must not start the backend: it handed off to the new launcher, left the
// launch to the install path, or shows why it cannot start. transient is
// launchAndShow's: a --distro override the install path is started with.
func (a *launcherApp) reconcileUpdate(distro string, transient bool) bool {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return true
	}
	sequence := a.updateSequence(dir, distro)
	fingerprint := embeddedPayloadFingerprint()
	decision, err := sequence.Reconcile(context.Background(), fingerprint)
	for err == nil && decision.Action == wsllauncher.ReconcileJoin {
		if sequence.Self, err = supervise.CurrentProcessRef(); err != nil {
			break
		}
		decision, err = sequence.Join(context.Background(), decision.Record, fingerprint)
		// The applier's last report is not this launch's.
		a.loading.clearProgress()
	}
	if err != nil {
		log.Printf("updater: reconcile the update record: %v", err)
		a.showUpdateFailure("Agent Overflow could not read its update record.",
			"Nothing was started, so the data is left as it is. Details are in the launcher log.")
		return false
	}
	switch decision.Action {
	case wsllauncher.ReconcileHandOff:
		log.Printf("updater: resuming update %s with %s", decision.Record.Update.ID, decision.Record.StagedLauncher)
		if err := startApplier(sequence.RecordPath, decision.Record.StagedLauncher, decision.Record.Update.ID, distro); err != nil {
			log.Printf("updater: start %s: %v", decision.Record.StagedLauncher, err)
			a.showUpdateFailure("The update could not resume.", "Start Agent Overflow again. Details are in the launcher log.")
			return false
		}
		a.wails.Quit()
		return false
	case wsllauncher.ReconcileRelaunch:
		var args []string
		if transient {
			args = []string{"--distro", distro}
		}
		log.Printf("updater: update %s committed while this launch waited; starting %s", decision.Record.Update.ID, decision.Record.InstallPath)
		if err := startLauncherAfterThis(decision.Record.InstallPath, args...); err != nil {
			log.Printf("updater: start %s: %v", decision.Record.InstallPath, err)
			a.showUpdateFailure("The update finished, but Agent Overflow could not be restarted.", "Start Agent Overflow again.")
			return false
		}
		a.wails.Quit()
		return false
	case wsllauncher.ReconcileResume:
		log.Printf("updater: resuming migration %s", decision.Record.Update.ID)
		if end := sequence.ResumeMigration(context.Background(), decision.Record); !end.Launch {
			a.showUpdateFailure(end.Title, end.Detail)
			return false
		}
		a.loading.clearProgress()
	case wsllauncher.ReconcileBlocked:
		log.Printf("updater: update %s blocks this launch: %s", decision.Record.Update.ID, decision.Reason)
		title, detail := "The update did not finish, and the database backup could not be restored.",
			"Nothing was started, so the data is left as it is. Start Agent Overflow again to retry the restore. Details are in the launcher log."
		if decision.Record.Update.State == supervise.UpdateCommitted {
			version := startupprogress.DisplayVersion(truncateRunes(decision.Record.Update.To, 40))
			title, detail = "The update to "+version+" could not be installed.",
				"Its new launcher is missing. Install "+version+" from the releases page."
		}
		a.showUpdateFailure(title, detail)
		return false
	}
	args := decision.BackendArgs(payloadVersion)
	a.backendUpdateArgs.Store(&args)
	return true
}

// migrateBeforeLaunch is the no-live-migration gate: the backend at payload
// refused to migrate its database live and was stopped, so the database is
// migrated through a snapshot and a trial of that payload before it starts
// again (wsllauncher.UpdateSequence.Migrate). It returns false when the
// launch must not continue; the window then shows why.
func (a *launcherApp) migrateBeforeLaunch(distro, payload string, pending *wsllauncher.MigrationsPendingError) bool {
	log.Printf("updater: %v; migrating the database through a trial", pending)
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		a.showUpdateFailure("Agent Overflow could not start the database upgrade this version needs.",
			`%APPDATA% could not be resolved, so the upgrade has nowhere to record its progress. Nothing was started.`)
		return false
	}
	// The refused boot's last report is not the upgrade's.
	a.loading.clearProgress()
	end := a.updateSequence(dir, distro).Migrate(context.Background(), distro, payload, payloadVersion)
	if !end.Launch {
		a.showUpdateFailure(end.Title, end.Detail)
		return false
	}
	a.loading.clearProgress()
	return true
}

// showUpdateFailure puts a fixed-copy failure page in the window.
func (a *launcherApp) showUpdateFailure(title, detail string) {
	page := failurePageHTML(title, detail, "")
	a.startupFailure.Store(&page)
	if w := a.win(); w != nil {
		w.SetURL("/startup-error")
	}
}

// launcherUpdateHost is wsllauncher.UpdateHost on Windows.
type launcherUpdateHost struct {
	wsllauncher.UpdatePayloads
	configDir string
}

// HostFreeBytes reads the drive that holds the distro's virtual disk from
// the WSL registration.
func (h launcherUpdateHost) HostFreeBytes(distro string) (uint64, bool) {
	free, err := distroDiskFreeBytes(distro)
	if err != nil {
		log.Printf("updater: free space for %s's disk: %v", distro, err)
		return 0, false
	}
	return free, true
}

// InvalidatePayloadRecord clears wsl.json's installed payload. Isolated
// profiles never write wsl.json (persistSuccessfulLaunch).
func (h launcherUpdateHost) InvalidatePayloadRecord(supervise.LauncherRecord) error {
	if activeProfile != "" {
		return nil
	}
	return wsldistro.InvalidatePayload(h.configDir)
}

// RecordPayload records the committed payload in wsl.json: the trial was a
// successful boot of those bytes.
func (h launcherUpdateHost) RecordPayload(record supervise.LauncherRecord) error {
	if activeProfile != "" {
		return nil
	}
	cfg, err := wsldistro.Load(h.configDir)
	if err != nil {
		log.Printf("updater: read wsl.json before recording the new backend: %v", err)
	}
	if cfg == nil {
		cfg = &wsldistro.Config{}
	}
	cfg.InstalledSHA256 = record.TargetFingerprint
	cfg.InstalledVer = record.Update.To
	cfg.InstalledDistro = record.Distro
	cfg.InstalledBinPath = record.StablePayload
	return wsldistro.Save(h.configDir, cfg)
}

// PublishLauncher replaces the install path with the staged launcher.
func (h launcherUpdateHost) PublishLauncher(record supervise.LauncherRecord) error {
	return wsllauncher.PublishLauncherFile(record.StagedLauncher, record.InstallPath, record.Update.ID, replaceFile)
}

// RemoveLauncherResidue deletes the staged launcher and set-aside files.
func (h launcherUpdateHost) RemoveLauncherResidue(record supervise.LauncherRecord) error {
	return wsllauncher.RemoveLauncherFiles(record)
}

// replaceFile is MoveFileEx with REPLACE_EXISTING and WRITE_THROUGH, retried
// briefly because a scanner may hold a new file for a moment.
func replaceFile(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil || attempt == 4 {
			return err
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// lxssKey is where WSL registers each distribution for the current user.
const lxssKey = `Software\Microsoft\Windows\CurrentVersion\Lxss`

// distroDiskFreeBytes is the free space, for this user, on the drive that
// holds distro's virtual disk.
func distroDiskFreeBytes(distro string) (uint64, error) {
	root, err := registry.OpenKey(registry.CURRENT_USER, lxssKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		key, err := registry.OpenKey(root, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		registered, _, nameErr := key.GetStringValue("DistributionName")
		base, _, baseErr := key.GetStringValue("BasePath")
		key.Close()
		if nameErr != nil || !strings.EqualFold(registered, distro) {
			continue
		}
		if baseErr != nil {
			return 0, fmt.Errorf("read %s's BasePath: %w", distro, baseErr)
		}
		base = strings.TrimPrefix(base, `\\?\`)
		pathPtr, err := windows.UTF16PtrFromString(base)
		if err != nil {
			return 0, err
		}
		var free uint64
		if err := windows.GetDiskFreeSpaceEx(pathPtr, &free, nil, nil); err != nil {
			return 0, fmt.Errorf("free space at %s: %w", base, err)
		}
		return free, nil
	}
	return 0, fmt.Errorf("%s is not registered under HKCU\\%s", distro, lxssKey)
}
