package app

import (
	"context"
	"errors"
	"log"
	"sync"

	"agent-overflow/internal/eventchan"
)

// A restart to update does not stop running work (docs/specs/app-update.md,
// Interlocks). RestartToUpdate uses serve's quiescence: it waits until
// nothing runs, closes work admission, and hands off to the updater with
// admission closed. The wait is cancelable until the handoff begins. A
// handoff that fails, or that the WSL launcher later abandons, reopens
// admission.

// ErrRestartUnderway refuses CancelRestartToUpdate once the handoff began.
var ErrRestartUnderway = errors.New("the update is already restarting")

// The phases of updater:restart.
const (
	// restartPhaseWaiting names the running work the restart waits for.
	restartPhaseWaiting = "waiting"
	// restartPhaseRestarting is the handoff: the updater owns the restart.
	restartPhaseRestarting = "restarting"
	// restartPhaseCanceled ends a wait the user canceled. The update stays
	// ready.
	restartPhaseCanceled = "canceled"
	// restartPhaseFailed ends a restart whose handoff failed, with Error.
	restartPhaseFailed = "failed"
	// restartPhaseIdle ends a restart the WSL launcher abandoned after the
	// handoff. updater:error carries why.
	restartPhaseIdle = "idle"
)

// RestartUpdateStatus is the payload of updater:restart.
type RestartUpdateStatus struct {
	Phase      string `json:"phase"`
	WaitingFor string `json:"waitingFor,omitempty"`
	Error      string `json:"error,omitempty"`
}

// restartHandoff is the updater's half of a restart: *appupdate.Service.
type restartHandoff interface {
	RestartReady() error
	RestartToUpdate(onAbandoned func()) error
}

type restartUpdateState struct {
	mu sync.Mutex
	// active runs from RestartToUpdate's claim until the restart is
	// canceled, fails or is abandoned. A successful handoff keeps it: the
	// process is quitting, or the launcher is about to stop it.
	active bool
	// cancel ends the wait. nil once the handoff has begun.
	cancel context.CancelFunc
	// canceled is set by CancelRestartToUpdate.
	canceled bool
	// waitingFor is the reason the restart waits, "" when it does not.
	waitingFor string
	// waitDone closes when the waiting goroutine stops waiting.
	waitDone chan struct{}
	// handoff overrides a.updater in tests.
	handoff restartHandoff
}

func (a *App) restartHandoff() restartHandoff {
	if a.restartUpdate.handoff != nil {
		return a.restartUpdate.handoff
	}
	return a.updater
}

// RestartToUpdate swaps in the staged update and relaunches. It spawns the
// detached swap helper and asks Wails to begin its normal shutdown, so the
// transport drains and stores flush before the process exits; the helper then
// replaces the binary (or .app bundle) and starts the new version. This quits
// the running app, so it is only ever wired to an explicit button.
//
// The WSL backend cannot do any of that: the executable being replaced is the
// Windows launcher's, on a filesystem this process only sees through /mnt/c.
// It hands the staged artifact to the launcher instead and lets the launcher
// kill it. See restartToUpdateWSL.
//
// Running work is never stopped for the restart. When the host is busy the
// call returns at once and the restart waits, publishing what it waits for
// on updater:restart; CancelRestartToUpdate ends the wait. When the host is
// idle the handoff runs in this call and its error is the call's.
//
//ao:scope host
//ao:route home
func (a *App) RestartToUpdate() error {
	if a.updater == nil {
		return ErrUpdatesUnsupported
	}
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	handoff := a.restartHandoff()
	// Refuse before waiting: a restart with nothing to install, or one the
	// updater is busy with, must not hold admission or show a wait.
	if err := handoff.RestartReady(); err != nil {
		return err
	}
	a.restartUpdate.mu.Lock()
	if a.restartUpdate.active {
		a.restartUpdate.mu.Unlock()
		return ErrUpdateBusy
	}
	ctx, cancel := context.WithCancel(a.lifeCtx())
	done := make(chan struct{})
	a.restartUpdate.active = true
	a.restartUpdate.cancel = cancel
	a.restartUpdate.canceled = false
	a.restartUpdate.waitingFor = ""
	a.restartUpdate.waitDone = done
	a.restartUpdate.mu.Unlock()

	reason, err := a.workAdmission.quiesce(a.updateWorkReason)
	if err != nil {
		close(done)
		cancel()
		a.endRestartUpdate()
		return err
	}
	if reason == "" {
		close(done)
		return a.handOffRestartToUpdate(ctx, cancel, handoff)
	}
	a.publishRestartWaiting(reason)
	go a.waitThenRestartToUpdate(ctx, cancel, handoff, done)
	return nil
}

// waitThenRestartToUpdate owns a restart that waits for running work.
func (a *App) waitThenRestartToUpdate(ctx context.Context, cancel context.CancelFunc, handoff restartHandoff, done chan struct{}) {
	err := a.waitForUpdateIdle(ctx, a.publishRestartWaiting)
	close(done)
	if err == nil {
		// The error already reached updater:restart; this call has no
		// caller to return it to.
		_ = a.handOffRestartToUpdate(ctx, cancel, handoff)
		return
	}
	cancel()
	a.restartUpdate.mu.Lock()
	canceled := a.restartUpdate.canceled
	a.restartUpdate.mu.Unlock()
	a.endRestartUpdate()
	switch {
	case canceled:
		a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseCanceled})
	case errors.Is(err, context.Canceled):
		// Shutdown ended the wait; nothing is left to tell.
	default:
		log.Printf("updater: waiting to restart: %v", err)
		a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseFailed, Error: err.Error()})
	}
}

// handOffRestartToUpdate runs with work admission closed. It claims the
// handoff under the same lock cancellation takes, so a canceled wait never
// restarts, then hands off.
func (a *App) handOffRestartToUpdate(ctx context.Context, cancel context.CancelFunc, handoff restartHandoff) error {
	defer cancel()
	a.restartUpdate.mu.Lock()
	if err := ctx.Err(); err != nil {
		canceled := a.restartUpdate.canceled
		a.restartUpdate.mu.Unlock()
		a.workAdmission.reopen()
		a.endRestartUpdate()
		if canceled {
			a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseCanceled})
			return nil
		}
		return err
	}
	a.restartUpdate.cancel = nil
	a.restartUpdate.waitingFor = ""
	a.restartUpdate.mu.Unlock()

	a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseRestarting})
	if err := handoff.RestartToUpdate(a.restartToUpdateAbandoned); err != nil {
		log.Printf("updater: restart to update: %v", err)
		a.workAdmission.reopen()
		a.endRestartUpdate()
		a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseFailed, Error: err.Error()})
		return err
	}
	return nil
}

// restartToUpdateAbandoned is the updater's callback for a WSL handoff the
// launcher failed, refused or went silent on.
func (a *App) restartToUpdateAbandoned() {
	a.restartUpdate.mu.Lock()
	active := a.restartUpdate.active
	a.restartUpdate.mu.Unlock()
	if !active {
		return
	}
	a.workAdmission.reopen()
	a.endRestartUpdate()
	a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseIdle})
}

// endRestartUpdate releases the restart claim.
func (a *App) endRestartUpdate() {
	a.restartUpdate.mu.Lock()
	a.restartUpdate.active = false
	a.restartUpdate.cancel = nil
	a.restartUpdate.waitingFor = ""
	a.restartUpdate.waitDone = nil
	a.restartUpdate.mu.Unlock()
}

func (a *App) publishRestartWaiting(reason string) {
	a.restartUpdate.mu.Lock()
	a.restartUpdate.waitingFor = reason
	a.restartUpdate.mu.Unlock()
	a.emit(eventchan.UpdaterRestart, RestartUpdateStatus{Phase: restartPhaseWaiting, WaitingFor: reason})
}

// restartWaitingFor is what a waiting restart waits for, or "".
func (a *App) restartWaitingFor() string {
	a.restartUpdate.mu.Lock()
	defer a.restartUpdate.mu.Unlock()
	if a.restartUpdate.cancel == nil {
		return ""
	}
	return a.restartUpdate.waitingFor
}

// CancelRestartToUpdate ends a restart that is waiting for running work.
// The update stays ready. It is a no-op when no restart waits, and refused
// once the handoff began.
//
//ao:scope host
//ao:route home
func (a *App) CancelRestartToUpdate() error {
	a.restartUpdate.mu.Lock()
	defer a.restartUpdate.mu.Unlock()
	if !a.restartUpdate.active {
		return nil
	}
	if a.restartUpdate.cancel == nil {
		return ErrRestartUnderway
	}
	a.restartUpdate.canceled = true
	a.restartUpdate.cancel()
	return nil
}

// stopRestartUpdate ends a waiting restart and joins its goroutine, which
// reads the stores, before shutdown closes them. A restart past its wait
// has handed off and is not joined: the handoff may be what is shutting
// the process down.
func (a *App) stopRestartUpdate() {
	a.restartUpdate.mu.Lock()
	cancel, done := a.restartUpdate.cancel, a.restartUpdate.waitDone
	a.restartUpdate.mu.Unlock()
	if cancel == nil || done == nil {
		return
	}
	cancel()
	<-done
}
