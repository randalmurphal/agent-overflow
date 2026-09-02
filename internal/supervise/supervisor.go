package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/procutil"
)

// Supervisor is the stable process the service manager selects. It owns which
// version runs and it is the only runtime writer of the state file; the
// backend it starts is its child and speaks to it over one inherited pipe pair.
//
// Everything it does happens on ONE goroutine. Child exits, IPC frames and
// timers all arrive on the same select, and the linear work between selects —
// stop, snapshot, restore — runs on that same goroutine with nothing else
// scheduled. That is the rule t3code states and the reason there is no lock in
// this file: two state transitions cannot overlap because there is only ever
// one thing running.

// Config describes one supervised install.
type Config struct {
	// DataDir is the app data directory (<configRoot>/agent-overflow), the one
	// holding agent-overflow.db. Absolute.
	DataDir string
	// SelfExecutable is the supervisor's own binary, adopted as the active
	// version on a fresh install.
	SelfExecutable string
	// SelfVersion is that binary's build version.
	SelfVersion string
	// ChildArgs are the arguments after the binary path — `serve` plus the
	// boot flags the operator passed through.
	ChildArgs []string
	// Env is the base environment for children. Empty means os.Environ().
	Env []string
	// Stdout and Stderr are where a child's output goes. Empty means this
	// process's own, which is what a service manager's journal collects.
	Stdout, Stderr *os.File

	// Log receives one line per transition. Required in production; a nil Log
	// is silent, which is what a test wants.
	Log func(format string, args ...any)
	// Now is the clock. nil means time.Now.
	Now func() time.Time

	// TrialBudget is how long a trial has to report prepared. Zero takes
	// DefaultTrialBudget.
	TrialBudget time.Duration
	// ResponseGrace is how long an accepted child gets to flush its answer
	// before it is stopped. Zero takes DefaultResponseGrace.
	ResponseGrace time.Duration
	// StopTimeout bounds a graceful stop before the process group is killed.
	// Zero takes DefaultStopTimeout.
	StopTimeout time.Duration
}

const (
	// DefaultTrialBudget is the hard ceiling on a trial reaching prepared. The
	// number is t3code's, and it is a ceiling rather than an estimate: a trial
	// runs migrations against a database this host actually has, so a slow one
	// is not wrong — but a trial that has not bound a listener in two minutes
	// is not going to.
	DefaultTrialBudget = 120 * time.Second
	// DefaultResponseGrace is how long a child that just had an update
	// accepted gets before it is stopped. It exists so the RPC that asked can
	// return its update id to the client that will correlate on it; a stop
	// that raced the answer would leave the caller with no id at all.
	DefaultResponseGrace = 500 * time.Millisecond
	// DefaultStopTimeout bounds the graceful stop. The backend closes provider
	// sessions, flushes SQLite and drains the transport on SIGTERM, and the
	// snapshot cannot be taken until it has; long enough for that, short
	// enough that a wedged process does not hold an update open.
	DefaultStopTimeout = 30 * time.Second
)

// New validates a config and resolves the layout.
func New(config Config) (*Supervisor, error) {
	layout, err := NewLayout(config.DataDir)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.SelfExecutable) == "" {
		return nil, errors.New("supervise: the supervisor's own executable path is required")
	}
	if err := ValidVersion(config.SelfVersion); err != nil {
		return nil, fmt.Errorf("supervise: this binary's version: %w", err)
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.TrialBudget <= 0 {
		config.TrialBudget = DefaultTrialBudget
	}
	if config.ResponseGrace <= 0 {
		config.ResponseGrace = DefaultResponseGrace
	}
	if config.StopTimeout <= 0 {
		config.StopTimeout = DefaultStopTimeout
	}
	return &Supervisor{config: config, layout: layout}, nil
}

// Supervisor runs one install.
type Supervisor struct {
	config Config
	layout Layout
}

// Layout exposes where this supervisor keeps its state.
func (s *Supervisor) Layout() Layout { return s.layout }

// Run supervises until the child exits on its own or the context is cancelled.
//
// The returned error is the child's: a supervisor whose backend exited
// non-zero exits non-zero itself, so `Restart=on-failure` means the same thing
// it meant before a supervisor existed. A clean child exit is the operator
// stopping the backend, and it ends the supervisor cleanly too.
func (s *Supervisor) Run(ctx context.Context) error {
	// Before anything reads the state file, let alone spawns a version: a
	// half-restored database must not be opened by either version.
	if marker, resumed, err := ResumeRestore(s.layout); err != nil {
		return fmt.Errorf("supervise: resume interrupted restore: %w", err)
	} else if resumed {
		s.config.Log("supervise: finished the restore left by update %s (%s)", marker.UpdateID, marker.Reason)
	}

	state, err := s.loadOrAdopt()
	if err != nil {
		return err
	}

	for {
		selection, err := state.Select()
		if err != nil {
			return err
		}
		if selection.Trial {
			if state.Update.Attempts >= TrialAttemptLimit {
				s.config.Log("supervise: update %s has been attempted %d times; rolling back",
					selection.UpdateID, state.Update.Attempts)
				state, err = s.rollBack(state, fmt.Sprintf(
					"the trial was interrupted %d times without reporting prepared", TrialAttemptLimit))
				if err != nil {
					return err
				}
				continue
			}
			// Count the attempt BEFORE it happens, and durably. A supervisor
			// killed by the very trial it is starting must find the attempt
			// recorded when it comes back, or the two of them loop forever.
			if state, err = state.Retry(); err != nil {
				return err
			}
			if err := SaveState(s.layout, state); err != nil {
				return err
			}
		}

		child, err := s.spawn(selection)
		if err != nil {
			if !selection.Trial {
				return err
			}
			// A trial that will not even start is an update that never
			// happened. Nothing of the target's has run, so there is nothing
			// to undo beyond putting the snapshot back.
			s.config.Log("supervise: trial %s did not start: %v", selection.UpdateID, err)
			state, err = s.settleFailure(state, err.Error())
			if err != nil {
				return err
			}
			continue
		}

		outcome := s.runChild(ctx, child, &state)
		switch outcome.kind {
		case outcomeShutdown:
			s.stopChild(child)
			return nil
		case outcomeExited:
			// Already gone; this closes the channel and reaps nothing.
			s.stopChild(child)
			return outcome.err
		case outcomeUpdate:
			state, err = s.beginTrial(state, child, outcome.target)
			if err != nil {
				return err
			}
		case outcomeTrialFailed:
			s.stopChild(child)
			state, err = s.rollBack(state, outcome.reason)
			if err != nil {
				return err
			}
		}
	}
}

// loadOrAdopt reads the durable state, or writes the fresh-install one.
//
// A fresh install has no versions directory at all, so the supervisor adopts
// ITS OWN binary: it stages a copy under versions/<its version>/ and records
// that as active. The copy is what makes the first update coherent — "previous"
// then names an immutable directory rather than the file an operator may
// replace at any moment, which is the whole reason versions are immutable.
func (s *Supervisor) loadOrAdopt() (State, error) {
	state, found, err := LoadState(s.layout)
	if err != nil {
		return State{}, err
	}
	if found {
		s.warnIfSelfIsUnselected(state)
		return state, nil
	}
	s.config.Log("supervise: no launch state yet; adopting this binary as version %s", s.config.SelfVersion)
	if err := s.stageSelf(); err != nil {
		return State{}, err
	}
	state, err = Adopt(s.config.SelfVersion)
	if err != nil {
		return State{}, err
	}
	if err := SaveState(s.layout, state); err != nil {
		return State{}, err
	}
	return state, nil
}

// warnIfSelfIsUnselected says so when the file on disk is not what runs.
//
// An operator who replaces the binary and restarts the service expects the new
// one. Under a supervisor it is the STATE that selects, so the replaced file
// supervises and the previously selected version serves — which is correct
// (it is what keeps a committed update committed) and is also exactly the
// surprise worth naming out loud, with the one command that resolves it.
func (s *Supervisor) warnIfSelfIsUnselected(state State) {
	selection, err := state.Select()
	if err != nil || selection.Trial || selection.Version == s.config.SelfVersion {
		return
	}
	s.config.Log("supervise: this binary is version %s and the selected backend is %s. "+
		"To serve this binary, stop the service and run `agent-overflow service update`.",
		s.config.SelfVersion, selection.Version)
}

// stageSelf copies the running supervisor into its own version directory.
// Idempotent: an existing directory for this version is the same bytes by
// definition, because a version names one build.
func (s *Supervisor) stageSelf() error {
	binary, err := s.layout.VersionBinary(s.config.SelfVersion)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(binary); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("supervise: stat %s: %w", binary, statErr)
	}
	return StageBinary(s.layout, s.config.SelfVersion, s.config.SelfExecutable)
}

type outcomeKind int

const (
	outcomeShutdown outcomeKind = iota
	outcomeExited
	outcomeUpdate
	outcomeTrialFailed
)

type outcome struct {
	kind   outcomeKind
	err    error
	target string
	reason string
}

// runChild is the one select. Every event that can move this install's state
// arrives here and is handled to completion before the next one is read.
func (s *Supervisor) runChild(ctx context.Context, c *child, state *State) outcome {
	var (
		trial       = c.trial
		trialTimer  = newIdleTimer()
		graceTimer  = newIdleTimer()
		acceptedFor string
	)
	defer trialTimer.Stop()
	defer graceTimer.Stop()

	if trial {
		trialTimer.Reset(s.config.TrialBudget)
	}

	for {
		select {
		case <-ctx.Done():
			return outcome{kind: outcomeShutdown}

		case <-c.exited:
			if trial {
				return outcome{kind: outcomeTrialFailed, reason: trialExitReason(c.exitErr)}
			}
			return outcome{kind: outcomeExited, err: c.exitErr}

		case <-trialTimer.C():
			return outcome{kind: outcomeTrialFailed, reason: fmt.Sprintf(
				"the trial did not report prepared within %s", s.config.TrialBudget)}

		case <-graceTimer.C():
			return outcome{kind: outcomeUpdate, target: acceptedFor}

		case msg, ok := <-c.messages:
			if !ok {
				// The channel closing and the process exiting are the SAME
				// event, arriving on two channels in whichever order the
				// scheduler picks. Never settle a trial on this one: the exit
				// STATUS is the better description of what happened and it is
				// only a moment behind, so disable this arm and let the exit —
				// or, if the process somehow lingers with its pipe shut, the
				// trial budget — supply the reason. Returning here made a
				// crashed trial's durably recorded reason a coin flip between
				// "closed its channel" and the exit code an operator needs.
				c.messages = nil
				continue
			}
			switch msg.Type {
			case MsgHello:
				s.config.Log("supervise: backend %s speaking update protocol %d",
					msg.Version, msg.ProtocolVersion)
				if msg.ProtocolVersion > ProtocolVersion {
					s.config.Log("supervise: the running backend speaks a newer update protocol " +
						"than this supervisor; over-the-wire updates are unavailable until the " +
						"supervisor is replaced with `agent-overflow service update`")
				}

			case MsgPrepared:
				if !trial {
					continue
				}
				trialTimer.Stop()
				next, err := s.commit(*state, c)
				if err != nil {
					// The commit is what makes the trial's work durable; a
					// commit that could not be written must not be announced,
					// so this trial rolls back like any other failure.
					return outcome{kind: outcomeTrialFailed,
						reason: fmt.Sprintf("the commit could not be recorded: %v", err)}
				}
				*state = next
				trial = false
				c.trial = false

			case MsgRequestUpdate:
				if trial || acceptedFor != "" {
					s.refuse(c, "an update is already in flight")
					continue
				}
				next, err := s.acceptUpdate(*state, msg.TargetVersion)
				if err != nil {
					s.config.Log("supervise: refusing update to %q: %v", msg.TargetVersion, err)
					s.refuse(c, err.Error())
					continue
				}
				*state = next
				acceptedFor = msg.TargetVersion
				if err := c.conn.Send(Message{
					Type: MsgUpdateAccepted, UpdateID: next.Update.ID,
				}); err != nil {
					s.config.Log("supervise: telling the backend its update was accepted: %v", err)
				}
				s.config.Log("supervise: update %s accepted: %s -> %s",
					next.Update.ID, next.Update.From, next.Update.To)
				graceTimer.Reset(s.config.ResponseGrace)
			}
		}
	}
}

// acceptUpdate validates a target and writes the pending record.
//
// The order is the safety boundary: the target's directory and binary must
// exist, and the binary must answer a preflight this supervisor can talk to,
// BEFORE anything durable is written. A pending record naming a version that
// cannot run is a rollback the operator did not need to pay for.
func (s *Supervisor) acceptUpdate(state State, target string) (State, error) {
	if err := ValidVersion(target); err != nil {
		return State{}, err
	}
	binary, err := s.layout.VersionBinary(target)
	if err != nil {
		return State{}, err
	}
	info, err := os.Stat(binary)
	if err != nil {
		return State{}, fmt.Errorf("version %s is not staged: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("version %s is staged as something other than a file", target)
	}
	answer, err := s.preflight(binary)
	if err != nil {
		return State{}, err
	}
	if err := CheckPreflight(answer); err != nil {
		return State{}, err
	}
	next, err := state.Begin(newUpdateID(s.config.Now()), target, s.config.Now())
	if err != nil {
		return State{}, err
	}
	if err := SaveState(s.layout, next); err != nil {
		return State{}, err
	}
	return next, nil
}

// PreflightBinary asks a binary what it is, in its own process, bounded.
//
// Exported because TWO processes have to ask that question and must ask it the
// same way. The supervisor asks before it writes a pending update record. The
// backend asks before it stages a downloaded artifact into a version
// directory, so a file that is not an Agent Overflow binary this host can run
// is refused while it is still a temp file. A second implementation of "run it
// and read its answer" is how one of them ends up accepting what the other
// would refuse, and the refusal that matters most — CheckPreflight's protocol
// rule — is only correct if both saw the same answer.
//
// The binary inherits THIS process's environment, runs in its own process
// group, and gets preflightTimeout to print one line.
func PreflightBinary(ctx context.Context, binary string) (Preflight, error) {
	return preflightBinary(ctx, binary, nil)
}

// preflightBinary is the one implementation. A nil env inherits this process's
// environment, which is what PreflightBinary wants; the supervisor passes its
// configured child environment instead, so the answer comes from a process
// started the way the child would be.
func preflightBinary(ctx context.Context, binary string, env []string) (Preflight, error) {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, PreflightSubcommand)
	command.Env = env
	procutil.ConfigureGroup(command)
	output, err := command.Output()
	if err != nil {
		return Preflight{}, fmt.Errorf("the staged binary did not answer %s: %w", PreflightSubcommand, err)
	}
	return ParsePreflight(string(output))
}

// preflight asks a staged binary what it is, with the environment this
// supervisor gives its children.
func (s *Supervisor) preflight(binary string) (Preflight, error) {
	return preflightBinary(context.Background(), binary, s.childEnv())
}

// preflightTimeout bounds the preflight. It prints one line and exits, so a
// binary still thinking after five seconds is one that will not boot either.
const preflightTimeout = 5 * time.Second

// beginTrial performs the linear middle of an update: stop the child, snapshot
// the database while nothing holds it, and leave the state selecting the
// target as a trial. It runs between selects, on the loop's own goroutine.
func (s *Supervisor) beginTrial(state State, c *child, target string) (State, error) {
	s.config.Log("supervise: stopping the backend to snapshot the database")
	s.stopChild(c)
	if _, err := TakeSnapshot(s.layout, s.config.DataDir, s.config.Now()); err != nil {
		s.config.Log("supervise: could not snapshot the database: %v", err)
		return s.settleFailure(state, fmt.Sprintf("the database could not be snapshotted: %v", err))
	}
	s.config.Log("supervise: starting version %s as a trial", target)
	return state, nil
}

// commit makes the trial durable, then tells it.
//
// Durable FIRST, and the ordering is the point: the child opens its activation
// gate on the commit frame, so a frame sent before the write would be a
// backend acting unattended on an update no restart would select.
func (s *Supervisor) commit(state State, c *child) (State, error) {
	next, err := state.Settle(UpdateCommitted, "", s.config.Now())
	if err != nil {
		return State{}, err
	}
	if err := SaveState(s.layout, next); err != nil {
		return State{}, err
	}
	if err := DiscardSnapshot(s.layout); err != nil {
		// The commit is already durable, so this is housekeeping that failed:
		// the snapshot is stale bytes, not a rollback anybody will take.
		s.config.Log("supervise: could not delete the snapshot after commit: %v", err)
	}
	if err := c.conn.Send(Message{Type: MsgCommit, UpdateID: next.Update.ID}); err != nil {
		s.config.Log("supervise: telling the backend its update committed: %v", err)
	}
	s.config.Log("supervise: update %s committed: now running %s", next.Update.ID, next.Update.To)
	s.pruneVersions(next)
	return next, nil
}

// rollBack restores the snapshot and records the outcome, marker first.
func (s *Supervisor) rollBack(state State, reason string) (State, error) {
	updateID := ""
	if state.Update != nil {
		updateID = state.Update.ID
	}
	s.config.Log("supervise: rolling back update %s: %s", updateID, reason)
	if err := RestoreSnapshot(s.layout, s.config.DataDir, updateID, reason, s.config.Now()); err != nil {
		// A restore that cannot complete is the one failure this supervisor
		// must not paper over: the database is the trial's, and starting the
		// previous version against it would be worse than not starting.
		return State{}, fmt.Errorf("supervise: restore the database snapshot: %w", err)
	}
	next, err := state.Settle(UpdateRolledBack, reason, s.config.Now())
	if err != nil {
		return State{}, err
	}
	if err := SaveState(s.layout, next); err != nil {
		return State{}, err
	}
	if err := DiscardSnapshot(s.layout); err != nil {
		s.config.Log("supervise: could not delete the snapshot after rollback: %v", err)
	}
	s.config.Log("supervise: restarting version %s", next.Update.From)
	return next, nil
}

// settleFailure records an update that never reached a trial.
func (s *Supervisor) settleFailure(state State, reason string) (State, error) {
	next, err := state.Settle(UpdateFailed, reason, s.config.Now())
	if err != nil {
		return State{}, err
	}
	if err := SaveState(s.layout, next); err != nil {
		return State{}, err
	}
	if err := DiscardSnapshot(s.layout); err != nil {
		s.config.Log("supervise: could not delete the snapshot after a failed update: %v", err)
	}
	return next, nil
}

// pruneVersions deletes staged versions that are neither running nor the one a
// rollback would return to. Disk on a serve host is not free, and a version
// directory nothing can select is bytes nobody will ever read.
func (s *Supervisor) pruneVersions(state State) {
	keep := map[string]bool{state.ActiveVersion: true}
	if state.Update != nil {
		keep[state.Update.From] = true
		keep[state.Update.To] = true
	}
	entries, err := os.ReadDir(s.layout.VersionsDir())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.config.Log("supervise: listing staged versions: %v", err)
		}
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || keep[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.layout.VersionsDir(), entry.Name())); err != nil {
			s.config.Log("supervise: removing staged version %s: %v", entry.Name(), err)
		}
	}
}

func (s *Supervisor) refuse(c *child, reason string) {
	if err := c.conn.Send(Message{Type: MsgUpdateRefused, Reason: reason}); err != nil {
		s.config.Log("supervise: refusing an update: %v", err)
	}
}

func trialExitReason(err error) string {
	if err == nil {
		return "the trial exited before reporting prepared"
	}
	return "the trial exited before reporting prepared: " + err.Error()
}

// newUpdateID mints the id a client correlates its reconnect against. Time
// plus randomness: the timestamp makes a journal readable in order, and the
// suffix makes two updates in the same millisecond distinct.
func newUpdateID(now time.Time) string {
	return fmt.Sprintf("upd-%d-%s", now.UnixMilli(), randomSuffix())
}
