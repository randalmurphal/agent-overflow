package appupdate

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// DesktopTrial is the macOS and Linux desktop's update with a trial
// (docs/specs/app-update.md, Sequence (b); supervise.DesktopHandoff). A
// downloaded target that applies updates as a helper is handed the update,
// which it applies after this process exits; any other target takes the
// framework's swap.
type DesktopTrial interface {
	// Check reports whether the target downloaded at path, of version,
	// takes the trial, or why it cannot be installed now.
	Check(ctx context.Context, path, version string) (bool, error)
	// HandOff stages the target, records the update and starts its helper,
	// which waits for this process to exit.
	HandOff(ctx context.Context, path, version string) error
	// RestartingTo is the target of the update this process handed off,
	// from its durable record, "" when there is none.
	RestartingTo() string
}

// desktopTrialMode is the desktop trial's state, guarded by
// appUpdaterState.mu.
type desktopTrialMode struct {
	trial DesktopTrial
	// quit begins the host's ordinary shutdown.
	quit func()
	// downloaded is the release DownloadAndInstall last put at the
	// updater's DownloadedPath. The updater exposes the path but not what
	// it holds, and a check after the download changes the pending release.
	// It names a restart target only while the path is set, which the
	// updater clears when a download starts.
	downloaded *updater.Release
	// handedOff is set once a helper has the update.
	handedOff bool
}

// ConfigureDesktopTrial gives the desktop's updates the trial. Call after
// Configure and before the RPCs are served.
func (a *Service) ConfigureDesktopTrial(trial DesktopTrial, quit func()) error {
	if a.updater.handle == nil {
		return ErrUpdatesUnsupported
	}
	if a.updater.wsl != nil {
		return errors.New("updater: the WSL backend's updates are applied by the Windows launcher")
	}
	if trial == nil || quit == nil {
		return errors.New("updater: the desktop trial needs its handoff and the host's quit")
	}
	a.updater.mu.Lock()
	a.updater.desktop = &desktopTrialMode{trial: trial, quit: quit}
	a.updater.mu.Unlock()
	return nil
}

// desktopTrialConfigured reports whether ConfigureDesktopTrial ran.
func (a *Service) desktopTrialConfigured() bool {
	a.updater.mu.Lock()
	defer a.updater.mu.Unlock()
	return a.updater.desktop != nil
}

// desktopDownloaded records the release a download put in place and returns
// the readiness event. The framework's own is suppressed in this mode
// (ForwardFrameworkEvent): it fires inside DownloadAndInstall, before the
// release is recorded and the download fence drops, and a client that
// restarts the instant it lands would be refused.
func (a *Service) desktopDownloaded(rel *updater.Release) (terminal func()) {
	if rel == nil {
		log.Printf("updater: the download reported success but names no release")
		return a.updaterErrorEmitter(updater.ErrorInfo{
			Stage: updater.StageInstall, Message: "the download names no release", Provider: "github",
		})
	}
	a.updater.mu.Lock()
	a.updater.desktop.downloaded = rel
	a.updater.mu.Unlock()
	return func() { a.emit(updaterReadyChannel, rel) }
}

// desktopRestartTargetLocked is the downloaded release a restart would
// install and its path, or why there is none now. Caller holds
// a.updater.mu.
func (a *Service) desktopRestartTargetLocked() (*updater.Release, string, error) {
	if a.updater.busy {
		return nil, "", ErrUpdateBusy
	}
	rel := a.updater.desktop.downloaded
	path := a.updater.handle.DownloadedPath()
	if rel == nil || path == "" {
		return nil, "", ErrUpdateNotReady
	}
	return rel, path, nil
}

// restartToUpdateDesktop hands a target that applies updates to its helper
// and quits; any other target takes the framework's swap. busy is held from
// here: a successful handoff or swap ends the process, and a failure
// releases it with the error.
func (a *Service) restartToUpdateDesktop() error {
	a.updater.mu.Lock()
	rel, path, err := a.desktopRestartTargetLocked()
	if err != nil {
		a.updater.mu.Unlock()
		return err
	}
	a.updater.busy = true
	mode := a.updater.desktop
	a.updater.mu.Unlock()
	release := func() {
		a.updater.mu.Lock()
		a.updater.busy = false
		a.updater.mu.Unlock()
	}

	ctx := a.context()
	trial, err := mode.trial.Check(ctx, path, rel.Version)
	if err != nil {
		release()
		return fmt.Errorf("restart to update: %w", err)
	}
	if !trial {
		log.Printf("updater: %s does not apply updates with a trial; replacing this version with it", rel.Version)
		if err := a.restartWithExitWatchdog(a.frameworkRestart); err != nil {
			release()
			return err
		}
		return nil
	}
	if err := mode.trial.HandOff(ctx, path, rel.Version); err != nil {
		release()
		return fmt.Errorf("restart to update: %w", err)
	}
	a.updater.mu.Lock()
	mode.handedOff = true
	a.updater.mu.Unlock()
	// The helper waits for this process to exit; a wedged shutdown must not
	// outlast its wait.
	a.armRestartExitWatchdog(a.restartWatchdogDelay())
	log.Printf("updater: handed the update to %s to its helper; quitting", rel.Version)
	mode.quit()
	return nil
}
