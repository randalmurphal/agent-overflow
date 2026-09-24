package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/eventchan"
)

// fakeRestartHandoff stands in for the updater's half of a restart.
type fakeRestartHandoff struct {
	mu          sync.Mutex
	readyErr    error
	restartErr  error
	restarts    int
	onAbandoned func()
}

func (f *fakeRestartHandoff) RestartReady() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readyErr
}

func (f *fakeRestartHandoff) RestartToUpdate(onAbandoned func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	f.onAbandoned = onAbandoned
	return f.restartErr
}

func (f *fakeRestartHandoff) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

type restartRig struct {
	t       *testing.T
	app     *App
	handoff *fakeRestartHandoff
	mu      sync.Mutex
	frames  []RestartUpdateStatus
}

// newRestartRig is an App whose lifetime context is never canceled, so only
// the restart's own cancellation can end its wait.
func newRestartRig(t *testing.T) *restartRig {
	t.Helper()
	rig := &restartRig{t: t, handoff: &fakeRestartHandoff{}}
	app := &App{version: "1.0.0", updater: appupdate.New("1.0.0", appupdate.Deps{})}
	app.restartUpdate.handoff = rig.handoff
	app.testEmitHook = func(name string, data any) {
		if eventchan.Channel(name) != eventchan.UpdaterRestart {
			return
		}
		status, ok := data.(RestartUpdateStatus)
		if !ok {
			t.Errorf("updater:restart carried %T, not RestartUpdateStatus", data)
			return
		}
		rig.mu.Lock()
		rig.frames = append(rig.frames, status)
		rig.mu.Unlock()
	}
	rig.app = app
	t.Cleanup(app.stopRestartUpdate)
	return rig
}

func (r *restartRig) snapshot() []RestartUpdateStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RestartUpdateStatus(nil), r.frames...)
}

// await returns the frames once the last one has phase.
func (r *restartRig) await(phase string) []RestartUpdateStatus {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frames := r.snapshot()
		if len(frames) > 0 && frames[len(frames)-1].Phase == phase {
			return frames
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.t.Fatalf("updater:restart never reached %q: %+v", phase, r.snapshot())
	return nil
}

func phasesOf(frames []RestartUpdateStatus) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, frame.Phase)
	}
	return out
}

func equalPhases(frames []RestartUpdateStatus, want ...string) bool {
	got := phasesOf(frames)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// holdWork takes a work admission lease, which the idle check reads as
// running work.
func (r *restartRig) holdWork() func() {
	r.t.Helper()
	release, err := r.app.workAdmission.begin(context.Background())
	if err != nil {
		r.t.Fatalf("begin work: %v", err)
	}
	return release
}

// admissionOpen reports whether new work is admitted within a short wait.
func (r *restartRig) admissionOpen() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	release, err := r.app.workAdmission.begin(ctx)
	if err != nil {
		return false
	}
	release()
	return true
}

func TestRestartToUpdateHandsOffAtOnceWhenIdle(t *testing.T) {
	rig := newRestartRig(t)
	if err := rig.app.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	if n := rig.handoff.restartCount(); n != 1 {
		t.Fatalf("handoffs = %d, want 1 within the call", n)
	}
	if frames := rig.snapshot(); !equalPhases(frames, restartPhaseRestarting) {
		t.Fatalf("frames = %+v, want restarting", frames)
	}
	if rig.admissionOpen() {
		t.Fatal("a handed-off restart left work admission open")
	}
	if err := rig.app.CancelRestartToUpdate(); !errors.Is(err, ErrRestartUnderway) {
		t.Fatalf("cancel after the handoff = %v, want ErrRestartUnderway", err)
	}
	if err := rig.app.RestartToUpdate(); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("second RestartToUpdate = %v, want ErrUpdateBusy", err)
	}
}

func TestRestartToUpdateRefusesBeforeWaiting(t *testing.T) {
	rig := newRestartRig(t)
	rig.handoff.readyErr = ErrUpdateNotReady
	release := rig.holdWork()
	defer release()

	if err := rig.app.RestartToUpdate(); !errors.Is(err, ErrUpdateNotReady) {
		t.Fatalf("RestartToUpdate = %v, want ErrUpdateNotReady", err)
	}
	if frames := rig.snapshot(); len(frames) != 0 {
		t.Fatalf("a refused restart published %+v", frames)
	}
	release()
	if !rig.admissionOpen() {
		t.Fatal("a refused restart closed work admission")
	}
	if n := rig.handoff.restartCount(); n != 0 {
		t.Fatalf("handoffs = %d, want 0", n)
	}
}

func TestRestartToUpdateWaitsForRunningWork(t *testing.T) {
	rig := newRestartRig(t)
	release := rig.holdWork()

	if err := rig.app.RestartToUpdate(); err != nil {
		release()
		t.Fatalf("RestartToUpdate: %v", err)
	}
	frames := rig.await(restartPhaseWaiting)
	reason := frames[len(frames)-1].WaitingFor
	if reason == "" {
		release()
		t.Fatalf("the waiting frame names nothing: %+v", frames)
	}
	if n := rig.handoff.restartCount(); n != 0 {
		release()
		t.Fatalf("handoffs = %d while work runs, want 0", n)
	}
	// A page that reloads during the wait learns of it from the check.
	availability, err := rig.app.CheckForUpdate()
	if err != nil {
		release()
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if availability.RestartWaitingFor != reason {
		release()
		t.Fatalf("RestartWaitingFor = %q, want %q", availability.RestartWaitingFor, reason)
	}
	if err := rig.app.RestartToUpdate(); !errors.Is(err, ErrUpdateBusy) {
		release()
		t.Fatalf("second RestartToUpdate = %v, want ErrUpdateBusy", err)
	}

	release()
	frames = rig.await(restartPhaseRestarting)
	if !equalPhases(frames, restartPhaseWaiting, restartPhaseRestarting) {
		t.Fatalf("frames = %+v, want waiting then restarting", frames)
	}
	if n := rig.handoff.restartCount(); n != 1 {
		t.Fatalf("handoffs = %d, want 1", n)
	}
	if rig.admissionOpen() {
		t.Fatal("the handoff ran with work admission open")
	}
	availability, err = rig.app.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate after the handoff: %v", err)
	}
	if availability.RestartWaitingFor != "" {
		t.Fatalf("RestartWaitingFor = %q after the handoff", availability.RestartWaitingFor)
	}
}

func TestCancelRestartToUpdateEndsTheWait(t *testing.T) {
	rig := newRestartRig(t)
	release := rig.holdWork()
	defer release()

	if err := rig.app.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rig.await(restartPhaseWaiting)
	if err := rig.app.CancelRestartToUpdate(); err != nil {
		t.Fatalf("CancelRestartToUpdate: %v", err)
	}
	frames := rig.await(restartPhaseCanceled)
	if !equalPhases(frames, restartPhaseWaiting, restartPhaseCanceled) {
		t.Fatalf("frames = %+v, want waiting then canceled", frames)
	}
	release()
	if !rig.admissionOpen() {
		t.Fatal("a canceled restart left work admission closed")
	}
	time.Sleep(20 * time.Millisecond)
	if n := rig.handoff.restartCount(); n != 0 {
		t.Fatalf("a canceled restart handed off %d times", n)
	}
	if err := rig.app.CancelRestartToUpdate(); err != nil {
		t.Fatalf("cancel with nothing waiting = %v, want nil", err)
	}
	// The update is still ready, so the user can ask again.
	if err := rig.app.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate after a cancel: %v", err)
	}
	if n := rig.handoff.restartCount(); n != 1 {
		t.Fatalf("handoffs = %d, want 1", n)
	}
}

func TestRestartToUpdateHandoffFailureReopensAdmission(t *testing.T) {
	boom := errors.New("the swap helper could not start")
	t.Run("idle", func(t *testing.T) {
		rig := newRestartRig(t)
		rig.handoff.restartErr = boom
		if err := rig.app.RestartToUpdate(); !errors.Is(err, boom) {
			t.Fatalf("RestartToUpdate = %v, want the handoff error", err)
		}
		frames := rig.snapshot()
		if !equalPhases(frames, restartPhaseRestarting, restartPhaseFailed) || frames[1].Error != boom.Error() {
			t.Fatalf("frames = %+v, want restarting then failed with the error", frames)
		}
		if !rig.admissionOpen() {
			t.Fatal("a failed handoff left work admission closed")
		}
		rig.handoff.restartErr = nil
		if err := rig.app.RestartToUpdate(); err != nil {
			t.Fatalf("retry after a failed handoff: %v", err)
		}
	})
	t.Run("after waiting", func(t *testing.T) {
		rig := newRestartRig(t)
		rig.handoff.restartErr = boom
		release := rig.holdWork()
		if err := rig.app.RestartToUpdate(); err != nil {
			release()
			t.Fatalf("RestartToUpdate: %v", err)
		}
		rig.await(restartPhaseWaiting)
		release()
		frames := rig.await(restartPhaseFailed)
		if frames[len(frames)-1].Error != boom.Error() {
			t.Fatalf("failed frame = %+v, want the handoff error", frames[len(frames)-1])
		}
		if !rig.admissionOpen() {
			t.Fatal("a failed handoff left work admission closed")
		}
	})
}

func TestRestartToUpdateAbandonedHandoffReopensAdmission(t *testing.T) {
	rig := newRestartRig(t)
	if err := rig.app.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rig.handoff.mu.Lock()
	onAbandoned := rig.handoff.onAbandoned
	rig.handoff.mu.Unlock()
	if onAbandoned == nil {
		t.Fatal("the handoff was given no abandonment callback")
	}
	if rig.admissionOpen() {
		t.Fatal("work admission is open during the handoff")
	}

	onAbandoned()
	frames := rig.snapshot()
	if !equalPhases(frames, restartPhaseRestarting, restartPhaseIdle) {
		t.Fatalf("frames = %+v, want restarting then idle", frames)
	}
	if !rig.admissionOpen() {
		t.Fatal("an abandoned handoff left work admission closed")
	}
	onAbandoned()
	if frames := rig.snapshot(); len(frames) != 2 {
		t.Fatalf("a repeated callback published again: %+v", frames)
	}
	if err := rig.app.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate after an abandoned handoff: %v", err)
	}
}

func TestShutdownJoinsAWaitingRestart(t *testing.T) {
	rig := newRestartRig(t)
	release := rig.holdWork()
	defer release()
	if err := rig.app.RestartToUpdate(); err != nil {
		t.Fatalf("RestartToUpdate: %v", err)
	}
	rig.await(restartPhaseWaiting)
	rig.app.restartUpdate.mu.Lock()
	done := rig.app.restartUpdate.waitDone
	rig.app.restartUpdate.mu.Unlock()

	if err := rig.app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Shutdown returned while the restart was still waiting")
	}
	release()
	time.Sleep(20 * time.Millisecond)
	if n := rig.handoff.restartCount(); n != 0 {
		t.Fatalf("a restart ended by shutdown handed off %d times", n)
	}
	for _, frame := range rig.snapshot() {
		if frame.Phase == restartPhaseCanceled || frame.Phase == restartPhaseFailed {
			t.Fatalf("shutdown published %+v", frame)
		}
	}
}
