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

// StallRule judges a start by its progress rather than by one fixed budget.
type StallRule struct {
	// Window is how long the start may go without observed progress, or
	// without a sign of life (startupprogress.StallWatch).
	Window time.Duration
	// Ceiling bounds the whole run, whatever it reports.
	Ceiling time.Duration
}

const (
	// TrialStallWindow is the window every judge of a starting backend
	// applies.
	TrialStallWindow = startupprogress.StallWindow
	// TrialCeiling bounds a trial that keeps reporting progress, so a loop
	// that reports forever still ends.
	TrialCeiling = 30 * time.Minute
)

// DefaultTrialStallRule is the rule an update's trial runs under.
func DefaultTrialStallRule() StallRule {
	return StallRule{Window: TrialStallWindow, Ceiling: TrialCeiling}
}

// StallWatch arms a StallRule over a run's progress reports: a timer at the
// earliest moment startupprogress.StallWatch could judge the run stalled,
// and the ceiling from NewStallWatch. Both channels are nil once disarmed,
// so a select naming them never fires on a disarmed timer.
type StallWatch struct {
	judge   *startupprogress.StallWatch
	window  *time.Timer
	ceiling *time.Timer
}

// NewStallWatch starts both timers.
func NewStallWatch(rule StallRule) *StallWatch {
	return &StallWatch{
		judge:   startupprogress.NewStallWatch(rule.Window, time.Now()),
		window:  time.NewTimer(rule.Window),
		ceiling: time.NewTimer(rule.Ceiling),
	}
}

// Report records a progress report and rearms the window at the next moment
// the run could be judged stalled. It reports whether p was progress.
func (w *StallWatch) Report(p startupprogress.Progress) bool {
	now := time.Now()
	progressed := w.judge.Report(p, now)
	if w.window != nil {
		w.window.Reset(w.judge.Deadline().Sub(now))
	}
	return progressed
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

// Stalled fires when the window may have passed without progress. Stall
// says whether it did.
func (w *StallWatch) Stalled() <-chan time.Time {
	if w.window == nil {
		return nil
	}
	return w.window.C
}

// Stall judges the run now. silent is true when nothing was reported within
// the window of the start; stall is the judgement once something was. Both
// are empty when the run is not stalled, and the window is rearmed.
func (w *StallWatch) Stall() (stall *startupprogress.Stall, silent bool) {
	now := time.Now()
	if w.judge.Silent(now) {
		return nil, true
	}
	if stall := w.judge.Check(now); stall != nil {
		return stall, false
	}
	if w.window != nil {
		w.window.Reset(w.judge.Deadline().Sub(now))
	}
	return nil, false
}

// Last is the last report, if any.
func (w *StallWatch) Last() (startupprogress.Progress, bool) { return w.judge.Last() }

// Expired fires at the ceiling.
func (w *StallWatch) Expired() <-chan time.Time { return w.ceiling.C }

// Stop disarms both timers.
func (w *StallWatch) Stop() {
	if w.window != nil {
		w.window.Stop()
	}
	w.ceiling.Stop()
}

// trialJudge decides when a starting trial has failed. The one-shot trial
// (RunTrial) and the serve supervisor's trial (Supervisor.runChild) share
// it: the stall rule over the progress the child reports, or one fixed
// budget for a child whose hello does not say it reports progress.
type trialJudge struct {
	rule         StallRule
	legacyBudget time.Duration
	watch        *StallWatch
	legacy       bool
}

// newTrialJudge arms the rule from now. A zero rule takes
// DefaultTrialStallRule; a zero budget takes DefaultTrialBudget.
func newTrialJudge(rule StallRule, legacyBudget time.Duration) *trialJudge {
	if rule == (StallRule{}) {
		rule = DefaultTrialStallRule()
	}
	if legacyBudget <= 0 {
		legacyBudget = DefaultTrialBudget
	}
	return &trialJudge{rule: rule, legacyBudget: legacyBudget, watch: NewStallWatch(rule)}
}

// hello applies the child's hello. One without ReportsProgress gets the
// legacy budget from now, because it sends no progress to judge.
func (j *trialJudge) hello(msg Message) {
	if msg.ReportsProgress || j.legacy {
		return
	}
	j.legacy = true
	j.watch.Budget(j.legacyBudget)
}

// progress records a report. Only observed progress (a changed UpdatedAt)
// moves the stall window; a heartbeat alone only proves the child alive.
func (j *trialJudge) progress(p startupprogress.Progress) {
	if !j.legacy {
		j.watch.Report(p)
	}
}

// stalled fires when the child may have stalled; stall says whether it did.
// A nil judge never fires.
func (j *trialJudge) stalled() <-chan time.Time {
	if j == nil {
		return nil
	}
	return j.watch.Stalled()
}

// expired fires at the ceiling, or at the legacy budget. A nil judge never
// fires.
func (j *trialJudge) expired() <-chan time.Time {
	if j == nil {
		return nil
	}
	return j.watch.Expired()
}

// stall judges the child once stalled fired: failed with the reason, or not
// stalled with the window rearmed. A stall is worded as every judge of a
// starting backend words it (startupprogress.Stall).
func (j *trialJudge) stall() (reason string, failed bool) {
	stall, silent := j.watch.Stall()
	switch {
	case silent:
		return fmt.Sprintf("the new version reported no progress within %s of starting", j.rule.Window), true
	case stall != nil:
		return "the new version did not finish starting: " + stall.Error(), true
	}
	return "", false
}

// ceilingReason is the reason once expired fired.
func (j *trialJudge) ceilingReason() string {
	if j.legacy {
		return fmt.Sprintf("the trial did not report prepared within %s", j.legacyBudget)
	}
	if step := j.lastStep(); step != "" {
		return fmt.Sprintf("the new version did not finish starting within %s (last step: %s)", j.rule.Ceiling, step)
	}
	return fmt.Sprintf("the new version did not finish starting within %s", j.rule.Ceiling)
}

func (j *trialJudge) lastStep() string {
	last, _ := j.watch.Last()
	if last.Detail != "" {
		return last.Detail
	}
	return last.Phase
}

// stop disarms the judge. Safe on nil and more than once.
func (j *trialJudge) stop() {
	if j != nil {
		j.watch.Stop()
	}
}

// failedReason is the reason a trial's failed frame gives.
func failedReason(msg Message) string {
	if reason := strings.TrimSpace(msg.Reason); reason != "" {
		return reason
	}
	return "the new version failed to start"
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
	// OnProgress receives every progress frame, heartbeats included.
	OnProgress func(p startupprogress.Progress)
	// OnStopping is called before the trial is asked to stop, which can take
	// StopTimeout, so a caller judged by its own progress can report it.
	OnStopping func()
	// Log receives one line per transition. nil is silent.
	Log func(format string, args ...any)
}

// TrialFailedError is a trial that did not reach prepared. Reason is the
// sentence the update record keeps and the user reads. Step is the last
// step the trial reported (a progress report's Detail), empty when it
// reported none.
type TrialFailedError struct{ Reason, Step string }

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
	t := &trialRun{cfg: cfg, child: c, judge: newTrialJudge(cfg.Rule, cfg.LegacyBudget)}
	defer t.judge.stop()
	return t.run(ctx)
}

type trialRun struct {
	cfg   TrialConfig
	child *child
	judge *trialJudge
	// step is the last Detail the trial reported (TrialFailedError.Step).
	step string
}

// fail is the trial's failure with reason, naming its last step.
func (t *trialRun) fail(reason string) *TrialFailedError {
	return &TrialFailedError{Reason: reason, Step: t.step}
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
			return t.fail(exitReason(c.exitErr))

		case <-t.judge.stalled():
			reason, failed := t.judge.stall()
			if !failed {
				continue
			}
			t.stop()
			return t.fail(reason)

		case <-t.judge.expired():
			t.stop()
			return t.fail(t.judge.ceilingReason())

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
		t.judge.hello(msg)
	case MsgProgress:
		if msg.Progress == nil {
			return nil, false
		}
		if msg.Progress.Detail != "" {
			t.step = msg.Progress.Detail
		}
		if t.cfg.OnProgress != nil {
			t.cfg.OnProgress(*msg.Progress)
		}
		t.judge.progress(*msg.Progress)
	case MsgFailed:
		return t.fail(failedReason(msg)), true
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
