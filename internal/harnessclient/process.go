package harnessclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

// pollInterval is how often liveness is re-probed while waiting for a
// signal to take effect.
const pollInterval = 50 * time.Millisecond

// KillConfirmWindow is how long a termination waits for a delivered
// SIGKILL to actually remove the process before reporting that it
// survived. A package var so a test does not have to spend it.
var KillConfirmWindow = 2 * time.Second

// processAlive is the liveness probe, indirected so a test can act out
// the case this function exists to report — a process that outlives
// SIGKILL — which no cooperating child can be made to do on demand.
var processAlive = instanceinfo.ProcessAlive

// verifyProcessIdentity is indirected so lifecycle tests can exercise a PID
// recycling race without depending on the host scheduler to recycle a real
// process ID during the grace window.
var verifyProcessIdentity = instanceinfo.VerifyProcessIdentity

// exitCustody reports the members of a launch's process group that its
// launcher recorded when the launch's root exited, before reaping it, once
// it has (Launched.custody). A process the root started after a group signal never
// received it and is missing from the proof captured before the signal, and
// once the root is gone it carries nothing else that ties it to the launch.
type exitCustody func() (proof ownedGroupProof, recorded bool, err error)

// processControl is everything TerminateProcessVerified asks of the
// operating system: whether a pid is alive, whether it is still the
// process the caller expects, and a handle to signal it. It is one value so
// a test supplies all three together, and no real signal reaches a pid the
// test does not own.
type processControl struct {
	alive  func(pid int) bool
	verify func(pid int, expected instanceinfo.ProcessIdentity) error
	// open binds a handle to pid. The identity is checked again after the
	// handle exists, so where the handle pins the process (a pidfd) the
	// signal cannot reach a process that took the pid since.
	open func(pid int) (processSignaler, error)
}

// processSignaler is a handle on one process: the polite stop and the kill.
type processSignaler interface {
	stop() error
	kill() error
}

type osProcess struct{ proc *os.Process }

func (p osProcess) stop() error { return requestStop(p.proc) }
func (p osProcess) kill() error { return requestKill(p.proc) }

// osProcessControl is the real one, reading the package's probes at call
// time.
func osProcessControl() processControl {
	return processControl{
		alive:  processAlive,
		verify: verifyProcessIdentity,
		open: func(pid int) (processSignaler, error) {
			proc, err := os.FindProcess(pid)
			if err != nil {
				return nil, err
			}
			return osProcess{proc: proc}, nil
		},
	}
}

// verifyProcessIfAlive closes the normal-exit race between a liveness probe
// and identity capture. A process that disappears during verification is
// already safe. A process that remains, including a recycled PID, must still
// match the complete expected identity before a caller may signal it.
func verifyProcessIfAlive(pid int, expected instanceinfo.ProcessIdentity) (bool, error) {
	return osProcessControl().verifyIfAlive(pid, expected)
}

func (c processControl) verifyIfAlive(pid int, expected instanceinfo.ProcessIdentity) (bool, error) {
	if !c.alive(pid) {
		return false, nil
	}
	if err := c.verify(pid, expected); err != nil {
		if !c.alive(pid) {
			return false, nil
		}
		return true, err
	}
	return true, nil
}

// processDone reports a signal refused because the process it was sent
// to has exited: proof of exit for that process, whatever a pid probe
// says afterwards about a process that may since hold the pid.
func processDone(err error) bool { return errors.Is(err, os.ErrProcessDone) }

// WaitForExit confirms a process actually disappeared after an authenticated
// shutdown request. A successful RPC only proves the intended backend heard
// the request, not that its cleanup has finished.
func WaitForExit(ctx context.Context, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("wait for exit: %d is not a pid", pid)
	}
	for processAlive(pid) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for pid %d to exit: %w", pid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
	return nil
}

// TerminateProcessVerified is the destructive fallback for a backend that
// does not expose authenticated shutdown. It rechecks process birth,
// executable, and namespace immediately before signalling and again before
// escalation. Callers should prefer ShutdownAuthenticated.
func TerminateProcessVerified(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration) error {
	return terminateVerified(ctx, pid, expected, grace, osProcessControl())
}

func terminateVerified(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration, control processControl) error {
	if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
		return fmt.Errorf("terminate %d: incomplete process identity evidence", pid)
	}
	if expected.Namespace != "" && expected.Namespace != instanceinfo.CurrentPIDNamespace() {
		return fmt.Errorf("terminate %d: pid namespace %q is not this CLI's %q", pid, expected.Namespace, instanceinfo.CurrentPIDNamespace())
	}
	alive, err := control.verifyIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed: %w", pid, err)
	}
	if !alive {
		return nil
	}
	proc, err := control.open(pid)
	if err != nil {
		return fmt.Errorf("terminate %d: %w", pid, err)
	}
	alive, err = control.verifyIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed before signal: %w", pid, err)
	}
	if !alive {
		return nil
	}
	if err := proc.stop(); err != nil {
		if processDone(err) || !control.alive(pid) {
			return nil
		}
		return fmt.Errorf("terminate %d: %w", pid, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !control.alive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if !control.alive(pid) {
		return nil
	}
	// Do not escalate against a recycled pid: the birth is checked again
	// after the grace.
	alive, err = control.verifyIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed before kill: %w", pid, err)
	}
	if !alive {
		return nil
	}
	if err := proc.kill(); processDone(err) {
		return nil
	} else if err != nil && control.alive(pid) {
		return fmt.Errorf("kill %d after %s grace: %w", pid, grace, err)
	}
	killDeadline := time.Now().Add(KillConfirmWindow)
	for {
		if !control.alive(pid) {
			return nil
		}
		if !time.Now().Before(killDeadline) {
			return fmt.Errorf("pid %d survived SIGKILL after %s", pid, KillConfirmWindow)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// TerminateProcessTreeVerified reconciles the authenticated process and every
// descendant that belongs to its launch boundary. It returns only after the
// owned tree is gone, including the case where the backend leader already
// exited and left a child behind.
func TerminateProcessTreeVerified(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration) error {
	if expected.StartTime == "" || expected.Executable == "" || expected.Namespace == "" {
		return fmt.Errorf("terminate tree %d: incomplete process identity evidence", pid)
	}
	if expected.Namespace != instanceinfo.CurrentPIDNamespace() {
		return fmt.Errorf("terminate tree %d: pid namespace %q is not this CLI's %q", pid, expected.Namespace, instanceinfo.CurrentPIDNamespace())
	}
	return terminateOwnedProcess(ctx, pid, expected, grace, nil)
}
