package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"agent-overflow/internal/eventchan"
)

// errNoSupervisor is the answer for an install whose backend nobody
// supervises: a desktop app, a `serve` started by hand, a harness. It names
// the remedy rather than the missing field, because the person reading it
// installs a service, they do not wire a callback.
var errNoSupervisor = errors.New(
	"this backend has no supervisor, so it cannot update itself: install it as a service " +
		"with `agent-overflow service install`, or update it in place with " +
		"`agent-overflow service update`")

// The activation gate.
//
// A backend booted as a supervisor TRIAL has to prove it works before anything
// commits to it: it opens the store, runs migrations, binds the transport and
// answers read-only health probes. Both client requests and unattended work
// wait for commit, because a trial that is rolled back
// has its database restored and nothing else: a `git fetch` it ran, a
// credential it refreshed, an ACME order it placed, a retention sweep that
// deleted attachment files, a workflow turn it started: none of those are
// inside the snapshot boundary, and several of them cost real money or a real
// provider login.
//
// The rule for a boot step: if it ran and the update were rolled back, would
// restoring the database undo it? A step that answers no runs behind this
// gate. Database writes may stay where boot ordering needs them; file
// deletions, process effects and network effects wait. The one exception is
// a package constructor's sweep of its own scratch files (attachment upload
// temporaries, shell capture files, remote job scripts, log rotation): the
// restored database references none of them and every version redoes the
// sweep at boot.
//
// There is ONE gate: `Start` hands the boot's deferred steps to
// `activation.Run`, and the transport enters through WaitForActivation. No
// flag per subsystem: a second boolean is how the eleventh subsystem gets
// added without one. Work a service's boot rebuild owes outside the
// database, such as the workflow engine's wakes and auto-dispositions or a
// push notification, waits in waitForBootSteps at the port that performs it.
//
// Run takes the deferred steps in two halves. The first is what must finish
// before any client: a boot step that acts outside the database but has to
// precede every request, such as the sweep of the last run's prompt files
// before a session can start. The transport's admission waits for it on a
// trial, and waitForBootSteps waits for it on every boot that began, because
// a workflow wake the engine's rebuild queued must not start a session before
// that sweep on an ordinary boot either. The second half is the unattended
// set, which runs alongside clients.
//
// The zero value is OPEN. Only a supervisor trial parks it; every other boot
// has `Run` call both halves inline, in Start's own goroutine, before the
// transport is marked ready, so a startup failure is still a boot failure
// and clients still arrive after both.

// activation is closed while a trial waits for its commit.
type activation struct {
	mu sync.Mutex
	// parked is non-nil only while the gate is closed, and is closed (the
	// channel) to open it. nil means open, which is the zero value.
	parked chan struct{}
	// admitted is non-nil until clients may enter: on a trial, until the
	// first half of Run finished after the commit. nil means admitted.
	admitted chan struct{}
	// pending is true while a Run waits for the commit, so Open leaves
	// admission to it.
	pending bool
	// booting is non-nil from Start until the first half of Run finished,
	// on every boot. nil means no boot is in progress, which is what a test
	// App that never starts sees.
	booting chan struct{}
	// deferred is the goroutine a parked Run started, which stop joins.
	deferred *deferredRun
}

// beginBoot holds waitForBootSteps until Run's first half finished. Start
// calls it before anything it starts can wait.
func (g *activation) beginBoot() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.booting == nil {
		g.booting = make(chan struct{})
	}
}

// bootStepsPending reports whether a boot began and its steps before clients have
// not finished.
func (g *activation) bootStepsPending() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.booting != nil
}

func (g *activation) bootStepsDoneLocked() {
	if g.booting != nil {
		close(g.booting)
		g.booting = nil
	}
}

// waitForBootSteps returns once the boot's steps before clients have run,
// which on a trial is after its commit. A boot that fails or shuts down
// before then never releases it; ctx ends the wait.
func (a *App) waitForBootSteps(ctx context.Context) error {
	a.activation.mu.Lock()
	booting := a.activation.booting
	a.activation.mu.Unlock()
	if booting == nil {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-booting:
		return ctx.Err()
	}
}

// Park closes the gate. Must be called before Start.
func (g *activation) Park() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked == nil {
		g.parked = make(chan struct{})
		g.admitted = make(chan struct{})
	}
}

// Open releases everything waiting. Idempotent: a commit that arrives twice
// must not panic on a second close. Clients are admitted here unless a Run is
// waiting, which admits them once its first half is done.
func (g *activation) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked != nil {
		close(g.parked)
		g.parked = nil
	}
	if !g.pending {
		g.admitLocked()
	}
}

func (g *activation) admitLocked() {
	g.pending = false
	if g.admitted != nil {
		close(g.admitted)
		g.admitted = nil
	}
}

// Parked reports whether the gate is currently closed.
func (g *activation) Parked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parked != nil
}

// bootStep is one of the boot's deferred steps. Its error is one that fails
// an ordinary boot; a failure that does not reports through bootPhaseFailed
// itself and returns nil.
type bootStep func() error

// Run performs the boot's deferred steps once the gate is open: first
// beforeClients, then work. It checks ctx before each step, so once ctx
// ends no further step begins; the step already running finishes.
//
// Open (the ordinary case): inline, in the caller's goroutine. Every step of
// beforeClients runs and their failures are returned together, before work
// begins; work stops at its first failure and returns it. Either way a
// failure still fails the boot exactly as it did before, and a cancellation
// is returned as one.
//
// Parked: on a goroutine the gate owns, which waits for the commit, runs
// beforeClients, admits clients, then runs work. stop cancels and joins it.
// Either way waitForBootSteps returns once beforeClients finished, and on an
// ordinary boot only if it succeeded. An error can no longer fail a boot that
// already succeeded, so each goes to report, which is the honest consequence
// of deferring work past the moment a caller could act on it, and it only
// ever applies to a trial. A boot stopped before its commit runs neither and
// admits nobody; one stopped during beforeClients admits nobody either.
func (g *activation) Run(ctx context.Context, beforeClients, work []bootStep, report func(error)) error {
	g.mu.Lock()
	parked := g.parked
	if parked == nil {
		g.mu.Unlock()
		if failed, stopped := runBootSteps(ctx, beforeClients, false); failed != nil || stopped != nil {
			return errors.Join(failed, stopped)
		}
		g.mu.Lock()
		g.bootStepsDoneLocked()
		g.mu.Unlock()
		failed, stopped := runBootSteps(ctx, work, true)
		return errors.Join(failed, stopped)
	}
	g.pending = true
	runCtx, cancel := context.WithCancel(ctx)
	run := &deferredRun{cancel: cancel, done: make(chan struct{})}
	g.deferred = run
	g.mu.Unlock()
	go func() {
		defer close(run.done)
		defer cancel()
		select {
		case <-runCtx.Done():
			return
		case <-parked:
		}
		failed, _ := runBootSteps(runCtx, beforeClients, false)
		if failed != nil {
			report(failed)
		}
		if runCtx.Err() != nil {
			return
		}
		g.mu.Lock()
		g.bootStepsDoneLocked()
		g.admitLocked()
		g.mu.Unlock()
		if failed, _ := runBootSteps(runCtx, work, true); failed != nil {
			report(failed)
		}
	}()
	return nil
}

// runBootSteps runs steps in order and checks ctx before each, reporting
// what failed and, apart from it, whether ctx stopped a step from starting.
// stopAtFailure ends at the first failure; otherwise every step runs and
// the failures are joined.
func runBootSteps(ctx context.Context, steps []bootStep, stopAtFailure bool) (failed, stopped error) {
	var errs []error
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return errors.Join(errs...), fmt.Errorf("startup stopped before its deferred steps finished: %w", err)
		}
		if err := step(); err != nil {
			errs = append(errs, err)
			if stopAtFailure {
				break
			}
		}
	}
	return errors.Join(errs...), nil
}

// deferredRun is a parked Run's goroutine.
type deferredRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stop cancels a parked Run and waits for it: one still waiting for its
// commit returns at once, and one running a step returns when that step
// does. Shutdown calls it before it tears down anything a step uses.
// Idempotent, and a no-op when no Run was parked.
func (g *activation) stop() {
	g.mu.Lock()
	run := g.deferred
	g.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	<-run.done
}

// ParkUnattendedWork closes the activation gate. Bootstrap-boundary function
// rather than a method, for the reason every input in bootstrap.go is one: an
// exported method on App is a wire RPC by construction, and a caller that
// could park this backend's unattended work over the wire is a denial of
// service with a friendly name.
func ParkUnattendedWork(a *App) { a.activation.Park() }

// WaitForActivation is the transport admission seam. A trial may prove its
// listener healthy, but client credentials and work must wait for commit and
// for the boot steps that precede every client.
// Waiting also preserves older clients which mistake HTTP 503 for revoked auth.
func WaitForActivation(a *App, ctx context.Context) error {
	a.activation.mu.Lock()
	admitted := a.activation.admitted
	a.activation.mu.Unlock()
	if admitted == nil {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-admitted:
		return ctx.Err()
	}
}

// ActivateUnattendedWork opens the gate and publishes the update's outcome.
//
// The event is what closes the loop for the client that asked: it holds the
// update id RequestServiceUpdate published on `service:update-status`, and it
// reconnects to a backend it cannot otherwise
// tell apart from a restart, and this is the frame that names which update
// finished and how. It is published here rather than at the transport because
// the gate and the announcement are one moment: a backend that said "committed"
// and had not yet resumed acting would be lying about what it is.
func ActivateUnattendedWork(a *App, outcome ServiceUpdateOutcome) {
	a.activation.Open()
	if outcome.UpdateID == "" && outcome.Outcome == "" {
		return
	}
	a.emit(eventchan.ServiceUpdateOutcome, outcome)
}

// ServiceUpdateOutcome is what a boot says about the update that produced it.
//
// Flat and additive, like every other wire shape in this tree: the client
// matches UpdateID against the id its own request returned, and reads Outcome
// to know whether to celebrate or to show the reason. Version is what actually
// booted, which is the half that makes "committed" mean the new version
// ANSWERED rather than that the old one stopped; Target is what it was asked
// to become, which is the half a rollback needs.
type ServiceUpdateOutcome struct {
	UpdateID string `json:"updateId"`
	Outcome  string `json:"outcome"`
	Version  string `json:"version"`
	// Target is the version the update was aiming at. It equals Version on a
	// commit and names the version that FAILED on a rollback, where Version
	// is the one that came back. Both travel because a client told only one
	// of them cannot phrase a rollback without naming the wrong build.
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SetServiceUpdateRequester installs the call that asks this backend's
// supervisor to run an already-staged version.
//
// Injected from the boot, because the supervisor channel belongs to package
// main and nothing here may open one. Absent — every boot that is not a
// supervised `serve` — the request is refused with a sentence saying why,
// which is the answer a desktop install should give.
func SetServiceUpdateRequester(a *App, request func(target string) (string, error)) {
	a.serviceUpdate.mu.Lock()
	defer a.serviceUpdate.mu.Unlock()
	a.serviceUpdate.request = request
}

// serviceUpdateRequest asks the supervisor to run target, returning the update
// id the supervisor minted.
//
// Deliberately unexported and deliberately not bound: an exported method here
// IS a wire RPC, and the trigger that reaches this is
// RequestServiceUpdate (app_service_update.go), which sits behind a step-up
// proof and only reaches this call after the target has been downloaded,
// verified against its published checksum, preflighted and staged.
func (a *App) serviceUpdateRequest(target string) (string, error) {
	a.serviceUpdate.mu.Lock()
	request := a.serviceUpdate.request
	a.serviceUpdate.mu.Unlock()
	if request == nil {
		return "", errNoSupervisor
	}
	return request(target)
}
