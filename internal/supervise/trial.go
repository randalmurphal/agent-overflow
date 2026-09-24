package supervise

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/startupprogress"
)

// The one-shot trial of an in-app update (docs/specs/app-update.md): the new
// version boots against the snapshotted database as a child, reports
// prepared, and is stopped. It never becomes the live backend; the version
// published after the commit boots once more with nothing left to migrate.

// StallRule judges a trial by its progress rather than by one fixed budget.
type StallRule struct {
	// Window is how long the trial may go without a real progress report.
	// Heartbeats do not count: they prove the process runs, not that the
	// step it is in moves.
	Window time.Duration
	// Ceiling bounds the whole trial, whatever it reports.
	Ceiling time.Duration
}

const (
	// TrialStallWindow is the limit the launcher's loading page applies to a
	// starting backend, applied here to real progress only.
	TrialStallWindow = 30 * time.Second
	// TrialCeiling bounds a trial that keeps reporting progress, so a loop
	// that reports forever still ends.
	TrialCeiling = 30 * time.Minute
)

// DefaultTrialStallRule is the rule an update's trial runs under.
func DefaultTrialStallRule() StallRule {
	return StallRule{Window: TrialStallWindow, Ceiling: TrialCeiling}
}

// StallWatch arms a StallRule's two timers. Progress restarts the window;
// the ceiling runs from NewStallWatch. Both channels are nil once disarmed,
// so a select naming them never fires on a disarmed timer.
type StallWatch struct {
	window  *time.Timer
	ceiling *time.Timer
	rule    StallRule
}

// NewStallWatch starts both timers.
func NewStallWatch(rule StallRule) *StallWatch {
	return &StallWatch{
		window:  time.NewTimer(rule.Window),
		ceiling: time.NewTimer(rule.Ceiling),
		rule:    rule,
	}
}

// Progress records a real step: the window starts again.
func (w *StallWatch) Progress() {
	if w.window != nil {
		w.window.Reset(w.rule.Window)
	}
}

// Budget replaces the rule with one fixed budget from now, for a child that
// reports no progress to judge.
func (w *StallWatch) Budget(d time.Duration) {
	if w.window != nil {
		w.window.Stop()
		w.window = nil
	}
	w.ceiling.Reset(d)
}

// Stalled fires when the window passes without progress.
func (w *StallWatch) Stalled() <-chan time.Time {
	if w.window == nil {
		return nil
	}
	return w.window.C
}

// Expired fires at the ceiling.
func (w *StallWatch) Expired() <-chan time.Time { return w.ceiling.C }

// Stop disarms both timers.
func (w *StallWatch) Stop() {
	if w.window != nil {
		w.window.Stop()
	}
	w.ceiling.Stop()
}

// TrialConfig describes one one-shot trial.
type TrialConfig struct {
	// Binary and Args start the new version in its trial mode.
	Binary string
	Args   []string
	// Env is the child's environment. Empty means os.Environ().
	Env []string
	// Stdout and Stderr receive the child's output. nil means this
	// process's stderr for both: a trial's stdout is never a protocol.
	Stdout, Stderr *os.File
	// Lock is the data root's backend lock, inherited at InheritedLockFD so
	// the trial keeps the data root locked until it exits even if this
	// process dies first. nil passes nothing.
	Lock *os.File
	// UpdateID and TargetVersion ride the activate frame.
	UpdateID      string
	TargetVersion string
	// Rule judges a child that reports progress. Zero takes
	// DefaultTrialStallRule.
	Rule StallRule
	// LegacyBudget bounds a child that does not. Zero takes
	// DefaultTrialBudget.
	LegacyBudget time.Duration
	// StopTimeout bounds each graceful stop. Zero takes DefaultStopTimeout.
	StopTimeout time.Duration
	// OnProgress receives every progress frame; liveness marks a heartbeat.
	OnProgress func(p startupprogress.Progress, liveness bool)
	// OnStopping is called before the trial is asked to stop, which can take
	// StopTimeout, so a caller judged by its own progress can report it.
	OnStopping func()
	// Log receives one line per transition. nil is silent.
	Log func(format string, args ...any)
}

// TrialFailedError is a trial that did not reach prepared. Reason is the
// sentence the update record keeps and the user reads.
type TrialFailedError struct{ Reason string }

func (e *TrialFailedError) Error() string { return e.Reason }

// trialDrainGrace is how long a trial that exited is given to finish
// delivering the frames it wrote before it went, so a failed frame names the
// cause rather than the exit status.
const trialDrainGrace = time.Second

// RunTrial boots the trial and stops it.
//
// nil means the trial reported prepared and has been stopped. A
// *TrialFailedError means the new version could not run on this database and
// the caller rolls back. ctx's error means the caller asked to stop: the
// trial was stopped and nothing is known about the outcome, so the caller
// must leave the update to recovery rather than record one.
func RunTrial(ctx context.Context, cfg TrialConfig) error {
	if cfg.Rule == (StallRule{}) {
		cfg.Rule = DefaultTrialStallRule()
	}
	if cfg.LegacyBudget <= 0 {
		cfg.LegacyBudget = DefaultTrialBudget
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = DefaultStopTimeout
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stderr
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	env := cfg.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	var extraFiles []*os.File
	if cfg.Lock != nil {
		extraFiles = []*os.File{cfg.Lock}
		env = append(append([]string(nil), env...), EnvInheritedLock+"="+strconv.Itoa(InheritedLockFD))
	}
	c, err := startChild(childSpec{
		binary:     cfg.Binary,
		args:       cfg.Args,
		env:        env,
		stdout:     cfg.Stdout,
		stderr:     cfg.Stderr,
		extraFiles: extraFiles,
		activate: Message{
			Type: MsgActivate, ProtocolVersion: ProtocolVersion,
			Trial: true, UpdateID: cfg.UpdateID, TargetVersion: cfg.TargetVersion,
			OwnsDataRoot: true,
		},
		version: cfg.TargetVersion,
		log:     cfg.Log,
	})
	if err != nil {
		return &TrialFailedError{Reason: "the new version did not start: " + err.Error()}
	}
	t := &trialRun{cfg: cfg, child: c, watch: NewStallWatch(cfg.Rule)}
	defer t.watch.Stop()
	return t.run(ctx)
}

type trialRun struct {
	cfg    TrialConfig
	child  *child
	watch  *StallWatch
	last   startupprogress.Progress
	legacy bool
}

func (t *trialRun) run(ctx context.Context) error {
	c := t.child
	for {
		select {
		case <-ctx.Done():
			t.stop()
			return ctx.Err()

		case <-c.exited:
			if outcome, decided := t.drainAfterExit(); decided {
				c.conn.Close()
				return outcome
			}
			c.conn.Close()
			return &TrialFailedError{Reason: exitReason(c.exitErr)}

		case <-t.watch.Stalled():
			t.stop()
			return &TrialFailedError{Reason: t.stallReason()}

		case <-t.watch.Expired():
			t.stop()
			return &TrialFailedError{Reason: t.ceilingReason()}

		case msg, ok := <-c.messages:
			if !ok {
				// The exit is the better description and is a moment behind;
				// see Supervisor.runChild.
				c.messages = nil
				continue
			}
			if outcome, decided := t.handle(msg); decided {
				if outcome == nil || isTrialFailure(outcome) {
					t.stop()
				}
				return outcome
			}
		}
	}
}

// handle applies one frame. decided is true when the frame ends the trial.
func (t *trialRun) handle(msg Message) (outcome error, decided bool) {
	switch msg.Type {
	case MsgHello:
		t.cfg.Log("supervise: trial of %s is version %s speaking update protocol %d (progress=%t)",
			t.cfg.TargetVersion, msg.Version, msg.ProtocolVersion, msg.ReportsProgress)
		if !msg.ReportsProgress {
			t.legacy = true
			t.watch.Budget(t.cfg.LegacyBudget)
		}
	case MsgProgress:
		if msg.Progress == nil {
			return nil, false
		}
		t.last = *msg.Progress
		if t.cfg.OnProgress != nil {
			t.cfg.OnProgress(*msg.Progress, msg.Liveness)
		}
		if !msg.Liveness && !t.legacy {
			t.watch.Progress()
		}
	case MsgFailed:
		reason := strings.TrimSpace(msg.Reason)
		if reason == "" {
			reason = "the new version failed to start"
		}
		return &TrialFailedError{Reason: reason}, true
	case MsgPrepared:
		t.cfg.Log("supervise: trial of %s reported prepared", t.cfg.TargetVersion)
		return nil, true
	}
	return nil, false
}

// drainAfterExit reads what an exited child wrote before it went. A failed or
// prepared frame there decides the outcome; the exit status is the fallback.
func (t *trialRun) drainAfterExit() (error, bool) {
	c := t.child
	if c.messages == nil {
		return nil, false
	}
	timer := time.NewTimer(trialDrainGrace)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-c.messages:
			if !ok {
				return nil, false
			}
			if outcome, decided := t.handle(msg); decided {
				if outcome == nil {
					// Prepared, then exited before being asked to stop.
					// The boot reached the state the trial proves.
					return nil, true
				}
				return outcome, true
			}
		case <-timer.C:
			return nil, false
		}
	}
}

func (t *trialRun) stop() {
	if t.cfg.OnStopping != nil {
		t.cfg.OnStopping()
	}
	stopChildProcess(t.child, t.cfg.StopTimeout, t.cfg.Log)
}

func (t *trialRun) lastStep() string {
	if t.last.Detail != "" {
		return t.last.Detail
	}
	return t.last.Phase
}

func (t *trialRun) stallReason() string {
	if step := t.lastStep(); step != "" {
		return fmt.Sprintf("the new version stopped making progress for %s (last step: %s)", t.cfg.Rule.Window, step)
	}
	return fmt.Sprintf("the new version reported no progress within %s of starting", t.cfg.Rule.Window)
}

func (t *trialRun) ceilingReason() string {
	if t.legacy {
		return fmt.Sprintf("the trial did not report prepared within %s", t.cfg.LegacyBudget)
	}
	if step := t.lastStep(); step != "" {
		return fmt.Sprintf("the new version did not finish starting within %s (last step: %s)", t.cfg.Rule.Ceiling, step)
	}
	return fmt.Sprintf("the new version did not finish starting within %s", t.cfg.Rule.Ceiling)
}

func isTrialFailure(err error) bool {
	_, ok := err.(*TrialFailedError)
	return ok
}

func exitReason(err error) string {
	if err == nil {
		return "the new version exited before it finished starting"
	}
	return "the new version exited before it finished starting: " + err.Error()
}
