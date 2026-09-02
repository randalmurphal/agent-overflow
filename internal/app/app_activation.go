package app

import (
	"context"
	"errors"
	"log"
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
// answers RPCs — and it must do all of that WITHOUT taking a single action of
// its own. Serving is what "prepared" means, so serving is correct. What has
// to wait is everything self-initiated, because a trial that is rolled back
// has its database restored and nothing else: a `git fetch` it ran, a
// credential it refreshed, an ACME order it placed, a retention sweep that
// deleted attachment files, a workflow turn it started — none of those are
// inside the snapshot boundary, and several of them cost real money or a real
// provider login.
//
// So there is ONE gate, and one place that waits on it: `Start` hands the
// whole unattended set to `activation.Run`. Not a flag per subsystem — a
// second boolean is how the eleventh subsystem gets added without one.
//
// The zero value is OPEN. Every boot mode except a supervisor trial never
// touches this file, and on those boots `Run` calls its function inline, in
// Start's own goroutine, in the same order as before the gate existed — so a
// startup failure is still a boot failure and nothing about the ordinary boot
// moved.

// activation is closed while a trial waits for its commit.
type activation struct {
	mu sync.Mutex
	// parked is non-nil only while the gate is closed, and is closed (the
	// channel) to open it. nil means open, which is the zero value.
	parked chan struct{}
}

// Park closes the gate. Must be called before Start.
func (g *activation) Park() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked == nil {
		g.parked = make(chan struct{})
	}
}

// Open releases everything waiting. Idempotent: a commit that arrives twice,
// or a shutdown that opens the gate to let a waiter exit, must not panic on a
// second close.
func (g *activation) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parked != nil {
		close(g.parked)
		g.parked = nil
	}
}

// Parked reports whether the gate is currently closed.
func (g *activation) Parked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.parked != nil
}

// Run performs work once the gate is open.
//
// Open (the ordinary case): inline, returning the error, so a failure still
// fails the boot exactly as it did before.
//
// Parked: on a goroutine that waits. The error can no longer fail a boot that
// already succeeded, so it is logged — which is the honest consequence of
// deferring work past the moment a caller could act on it, and it only ever
// applies to a trial.
func (g *activation) Run(ctx context.Context, work func() error) error {
	g.mu.Lock()
	parked := g.parked
	g.mu.Unlock()
	if parked == nil {
		return work()
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-parked:
		}
		if err := work(); err != nil {
			log.Printf("app: starting unattended work after activation: %v", err)
		}
	}()
	return nil
}

// ParkUnattendedWork closes the activation gate. Bootstrap-boundary function
// rather than a method, for the reason every input in bootstrap.go is one: an
// exported method on App is a wire RPC by construction, and a caller that
// could park this backend's unattended work over the wire is a denial of
// service with a friendly name.
func ParkUnattendedWork(a *App) { a.activation.Park() }

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
// ANSWERED rather than that the old one stopped.
type ServiceUpdateOutcome struct {
	UpdateID string `json:"updateId"`
	Outcome  string `json:"outcome"`
	Version  string `json:"version"`
	Reason   string `json:"reason,omitempty"`
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
