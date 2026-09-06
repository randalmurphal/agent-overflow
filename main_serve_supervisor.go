package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	appservice "agent-overflow/internal/app"
	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/supervise"
)

// The backend's half of the supervisor protocol
// (docs/specs/remote-access.md §7; the supervisor's half is
// internal/supervise, and the operator's copy is
// docs/architecture/serve-mode.md).
//
// A supervised `serve` inherits one pipe pair and learns three things over
// it: whether this boot is a TRIAL, when an update it asked for was
// accepted, and when that update committed. It reports two: what it is
// (hello), and that a trial booted fully with every unattended subsystem
// parked (prepared).
//
// Everything here is optional by construction. `agent-overflow serve`
// started from a terminal has no channel, and every function below reads
// as "there is no supervisor" rather than as a failure — which is what
// keeps the mode this file extends exactly as usable as it was before.

// supervisorAnswerTimeout bounds the wait for the supervisor's answer to a
// request-update frame. It is generous because the supervisor may be running
// the target's preflight in its own process when the frame arrives, and it
// exists at all because the alternative is unbounded: the answer arrives on a
// goroutine that ends when the channel does, so a supervisor whose loop is
// wedged while its process lives would never end the wait on its own.
const supervisorAnswerTimeout = 30 * time.Second

// serveSupervisor is this process's end of the channel, plus the small
// amount of state a request needs to be answered.
type serveSupervisor struct {
	conn         *supervise.Conn
	ownsDataRoot bool
	// trial is whether this boot must report prepared and wait for a commit.
	trial bool
	// updateID is the update this boot is the outcome of, empty when this
	// boot follows no update.
	updateID string
	// outcome and reason are that update's settled state, on a boot that
	// FOLLOWS one. A rolled-back update's outcome can only be reported by the
	// version that came back, which is this one.
	outcome string
	reason  string
	// target is the version that update was aiming AT, which on a rollback is
	// NOT the version running: this process is the one that came back. The
	// client is told both, because "the update to X was rolled back, running
	// Y" is the only phrasing that names the version that actually failed.
	target string

	// answerTimeout bounds the wait for an answer to a request-update frame.
	// A field rather than the constant so a test can describe a supervisor
	// that never answers without waiting out the real budget.
	answerTimeout time.Duration

	// pending is the caller waiting on a request-update answer. One at a
	// time: the supervisor refuses a second while one is in flight, so a
	// second waiter would be waiting on an answer that already said so.
	mu        sync.Mutex
	pending   chan supervisorUpdateReply
	uncertain error
	restart   chan struct{}
	// committed is closed when the supervisor commits this trial.
	committed chan struct{}
}

// attachServeSupervisor opens the channel, reads the opening frame, and says
// hello. It returns nil when this process has no supervisor.
//
// The opening frame is read synchronously, before the App exists, because its
// answer decides whether the App boots with its activation gate closed. The
// supervisor writes that frame at spawn, so the read finds it already in the
// pipe.
func attachServeSupervisor() (*serveSupervisor, error) {
	conn, err := supervise.OpenChildChannel(os.LookupEnv, os.Unsetenv)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, nil
	}
	opening, err := conn.Receive()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read the supervisor's opening frame: %w", err)
	}
	if opening.Type != supervise.MsgActivate {
		conn.Close()
		return nil, fmt.Errorf("the supervisor opened with %q, not %q", opening.Type, supervise.MsgActivate)
	}
	sup := &serveSupervisor{
		conn: conn, trial: opening.Trial, updateID: opening.UpdateID,
		ownsDataRoot: opening.OwnsDataRoot,
		outcome:      opening.Outcome, reason: opening.Reason,
		target:        opening.TargetVersion,
		answerTimeout: supervisorAnswerTimeout,
		committed:     make(chan struct{}),
		restart:       make(chan struct{}),
	}
	if err := conn.Send(supervise.Message{
		Type:            supervise.MsgHello,
		ProtocolVersion: supervise.ProtocolVersion,
		Version:         version,
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("greet the supervisor: %w", err)
	}
	go sup.read()
	return sup, nil
}

// read routes the supervisor's later frames. One goroutine, because a channel
// with one reader needs one.
func (s *serveSupervisor) read() {
	for {
		msg, err := s.conn.Receive()
		if err != nil {
			// The supervisor's end went away. Nothing to do about it here:
			// this process is the child, and a supervisor that died will be
			// restarted by the service manager, which will restart us too.
			s.mu.Lock()
			waiting := s.pending != nil
			s.mu.Unlock()
			if waiting {
				s.failPending(errors.New("the supervisor closed the channel"))
			}
			return
		}
		switch msg.Type {
		case supervise.MsgCommit:
			s.closeCommitted()
		case supervise.MsgUpdateAccepted, supervise.MsgUpdateRefused:
			s.deliver(msg)
		}
	}
}

func (s *serveSupervisor) closeCommitted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.committed:
	default:
		close(s.committed)
	}
}

func (s *serveSupervisor) deliver(msg supervise.Message) {
	s.mu.Lock()
	waiter := s.pending
	s.pending = nil
	s.mu.Unlock()
	if waiter != nil {
		waiter <- supervisorUpdateReply{message: msg}
	}
}

type supervisorUpdateReply struct {
	message supervise.Message
	err     error
}

func (s *serveSupervisor) failPending(err error) error {
	s.mu.Lock()
	if s.uncertain == nil {
		s.uncertain = fmt.Errorf("%w: %v", supervise.ErrUpdateOutcomeUnknown, err)
		if s.restart != nil {
			close(s.restart)
		}
	}
	failure := s.uncertain
	waiter := s.pending
	s.pending = nil
	s.mu.Unlock()
	if waiter != nil {
		waiter <- supervisorUpdateReply{err: failure}
	}
	return failure
}

// restartRequested is nil for an unsupervised host. An ambiguous reply asks
// for ordinary ordered shutdown; the supervisor settles its durable selection
// and boots a fresh backend before any more work can be accepted.
func (s *serveSupervisor) restartRequested() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.restart
}

// RequestUpdate asks the supervisor to run an already-staged version and
// returns the update id it minted.
//
// The step-up-gated App RPC calls this only after staging and quiescence.
// A missing reply is ambiguous: keep the admission fence and ask the main
// serve loop for ordered shutdown. Never recycle the uncorrelated reply slot
// while an older answer can still arrive.
func (s *serveSupervisor) RequestUpdate(target string) (string, error) {
	answer := make(chan supervisorUpdateReply, 1)
	s.mu.Lock()
	if s.uncertain != nil {
		err := s.uncertain
		s.mu.Unlock()
		return "", err
	}
	if s.pending != nil {
		s.mu.Unlock()
		return "", errors.New("an update request is already waiting for an answer")
	}
	s.pending = answer
	s.mu.Unlock()

	if err := s.conn.Send(supervise.Message{
		Type: supervise.MsgRequestUpdate, TargetVersion: target,
	}); err != nil {
		return "", s.failPending(err)
	}
	var reply supervisorUpdateReply
	select {
	case reply = <-answer:
	case <-time.After(s.answerTimeout):
		// A slow preflight may still accept after this deadline. Restart
		// through the supervisor instead of treating silence as refusal.
		err := fmt.Errorf("the supervisor did not answer the update request within %s", s.answerTimeout)
		return "", s.failPending(err)
	}
	if reply.err != nil {
		return "", reply.err
	}
	msg := reply.message
	if msg.Type != supervise.MsgUpdateAccepted {
		reason := msg.Reason
		if reason == "" {
			reason = "the supervisor refused it"
		}
		return "", errors.New(reason)
	}
	if msg.UpdateID == "" {
		return "", s.failPending(errors.New("the accepted update has no receipt"))
	}
	return msg.UpdateID, nil
}

// reportPrepared tells the supervisor this trial booted fully, then waits for
// the commit.
//
// Nothing about the wait is optional or bounded here: the supervisor owns the
// budget, and a trial that gave up on its own would open its activation gate
// on an update nobody committed. It ends one of two ways — a commit frame, or
// the SIGTERM the supervisor sends when it rolls back, which this process
// handles as the ordinary shutdown it is.
func (s *serveSupervisor) reportPrepared() error {
	return s.conn.Send(supervise.Message{Type: supervise.MsgPrepared, UpdateID: s.updateID})
}

// awaitCommit blocks until the supervisor commits.
func (s *serveSupervisor) awaitCommit() { <-s.committed }

// activationOutcome is what this boot publishes when its gate opens.
//
// Version is what actually booted and Target is what the update was aiming
// at. They are the same on a commit and different on a rollback, which is
// the one case where naming only one of them misleads.
func (s *serveSupervisor) activationOutcome(outcome, reason string) appservice.ServiceUpdateOutcome {
	target := s.target
	if target == "" {
		target = version
	}
	return appservice.ServiceUpdateOutcome{
		UpdateID: s.updateID,
		Outcome:  outcome,
		Version:  version,
		Target:   target,
		Reason:   reason,
	}
}

// configureServeSupervision applies the supervisor's answer to the App before
// Start: a trial boots with its activation gate closed, and every boot gets
// the update-request callback so the wave that adds the RPC has something to
// call.
func configureServeSupervision(appService *App, sup *serveSupervisor) {
	if sup == nil {
		return
	}
	appservice.SetServiceUpdateRequester(appService.App, sup.RequestUpdate)
	configureServeRemoteUpdate(appService)
	if sup.trial {
		appservice.ParkUnattendedWork(appService.App)
		log.Printf("serve: booting as a trial for update %s; unattended work is parked until it commits",
			sup.updateID)
	}
}

// configureServeRemoteUpdate gives the App the release source, the layout and
// the preflight the remote update trigger needs.
//
// Called only from configureServeSupervision, so only a SUPERVISED serve host
// gets it: an unsupervised `serve` and every desktop boot leave the seam
// unconfigured, and the status RPC answers Supervised:false there. It is a
// second guard rather than a redundant one — the trigger's last step asks the
// supervisor to run a staged version, so a host with a source and no
// supervisor would download and stage an artifact nothing could ever select.
//
// Two more reasons it can decline, and each becomes a sentence rather than a
// missing button:
//
//   - This binary is not one the release feed publishes for a supervised host
//     (serviceArtifactPlatform is ""). Windows uses the WSL launcher; it is
//     not a supervised serve host.
//   - The data directory cannot be resolved, which is the same condition
//     `supervise` itself refuses to start on, so this is only reachable in an
//     unsupervised boot that got here by some other route.
func configureServeRemoteUpdate(appService *App) {
	platform := serviceArtifactPlatform()
	if platform == "" {
		log.Printf("serve: this build has no release artifact a supervisor can install, " +
			"so updates over the wire are unavailable; use `agent-overflow service update`")
		return
	}
	dataDir := bootSettingsDir()
	if dataDir == "" {
		log.Printf("serve: cannot determine the data directory, so updates over the wire are unavailable")
		return
	}
	layout, err := supervise.NewLayout(dataDir)
	if err != nil {
		log.Printf("serve: %v; updates over the wire are unavailable", err)
		return
	}
	// No global client timeout: the same client streams a tens-of-MB release
	// binary, and http.Client.Timeout caps the WHOLE exchange including the
	// body read, so a fixed cap would abort a download on a slow link. The
	// flow bounds each phase with a context deadline instead.
	source, err := appupdate.NewReleaseSource(appupdate.Config{
		CurrentVersion: version,
		Platform:       platform,
		Arch:           runtime.GOARCH,
		HTTPClient:     &http.Client{},
	})
	if err != nil {
		log.Printf("serve: %v; updates over the wire are unavailable", err)
		return
	}
	appservice.ConfigureServiceUpdates(appService.App, appservice.ServiceUpdateDeps{
		Source: source,
		Layout: layout,
		// The supervisor's own preflight, not a copy: the answer this asks a
		// downloaded file for is the same answer the supervisor will ask the
		// staged one for a moment later, and two implementations would be two
		// verdicts on one binary.
		Preflight: supervise.PreflightBinary,
		Log:       log.Printf,
	})
	log.Printf("serve: updates over the wire are available (artifact %s, version %s)", platform, version)
}

// finishServeSupervision runs after the transport is bound and the store is
// open — the moment "prepared" describes.
//
// An ordinary supervised boot opens its gate immediately (Start already ran
// the unattended set inline, so this only publishes the outcome of whatever
// update preceded it). A TRIAL reports prepared, waits, and opens the gate on
// the commit frame. Health probes can verify the listener while all client
// requests and unattended actions remain outside the rollback boundary.
func finishServeSupervision(appService *App, sup *serveSupervisor) {
	if sup == nil {
		return
	}
	if !sup.trial {
		// The gate was never closed on this boot, so opening it is a no-op;
		// what this call does is publish the outcome of whatever update
		// preceded us — including a rollback, whose outcome only the version
		// that came back can report.
		appservice.ActivateUnattendedWork(appService.App, sup.activationOutcome(sup.outcome, sup.reason))
		return
	}
	if err := sup.reportPrepared(); err != nil {
		// The supervisor will time this trial out and roll it back, which is
		// the correct outcome for a trial that cannot report. Say so rather
		// than opening the gate on an update nobody committed.
		log.Printf("serve: could not report prepared to the supervisor: %v", err)
		return
	}
	log.Printf("serve: prepared; waiting for update %s to commit", sup.updateID)
	sup.awaitCommit()
	log.Printf("serve: update %s committed; resuming unattended work", sup.updateID)
	appservice.ActivateUnattendedWork(appService.App, sup.activationOutcome(string(supervise.UpdateCommitted), ""))
}
