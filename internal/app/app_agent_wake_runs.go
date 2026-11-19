package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// The wake supervisor's clock and processes. The thread request sweep calls
// fireDueAgentWakes on every pass and sleeps until NextAgentWakeDue;
// recoverAgentWakesAtBoot runs once before its first pass.
//
// Every state change is a conditional store write (internal/store
// agent_wakes.go), so a run that finds its row cancelled or replaced has lost
// a race and writes nothing more.

const (
	// agentWakeDefaultScriptTimeout bounds one interval watch run when the
	// watch names no timeout_seconds of its own. A run that outlives its
	// interval holds the row `running`, and the fires it overtakes are
	// counted missed when it finishes.
	agentWakeDefaultScriptTimeout = threadtools.DefaultWakeTimeoutSeconds * time.Second
	// agentWakeRunFanOut is how many interval runs execute at once, the
	// bound the delivery fan-out uses for the same reason.
	agentWakeRunFanOut = 4
	// agentWakeDueBatch bounds one pass. Rows past it are due on the next.
	agentWakeDueBatch = 32
	// agentWakeBatchWindow is how long a long-running watch's first line
	// waits for the lines after it, so a burst is one wake.
	agentWakeBatchWindow = time.Second
	// agentWakeRestartBase and agentWakeRestartCap pace the restart of a
	// long-running watch that exited; a process that ran for
	// agentWakeHealthyRun before exiting restarts at the base again.
	agentWakeRestartBase = time.Second
	agentWakeRestartCap  = 5 * time.Minute
	agentWakeHealthyRun  = time.Minute
	// agentWakeStopWait bounds how long a replaced long-running watch's
	// process gets to exit before its successor starts. The kill is a group
	// SIGKILL, so it normally exits at once.
	agentWakeStopWait = 5 * time.Second
)

// Outcomes recorded on a row. A skipped one is read back from `undelivered`:
// the fire that carries it adds the fires it left uncounted. The boot pass
// keeps `not delivered` over a cut run.
const (
	agentWakeOutcomeDelivered    = "delivered"
	agentWakeOutcomeNoOutput     = "no output"
	agentWakeOutcomeSkipped      = "skipped: the previous wake is still queued"
	agentWakeOutcomeCutByQuit    = "cancelled-by-quit"
	agentWakeOutcomeNotDelivered = "not delivered"
	// A wake the session ended before taking in, and one a Stop or a
	// rollback threw out of the queue.
	agentWakeOutcomeSessionEnded = agentWakeOutcomeNotDelivered + ": the session ended before it took the wake in"
	agentWakeOutcomeDropped      = agentWakeOutcomeNotDelivered + ": the queue was cleared"
	agentWakeOutcomeWithdrawn    = agentWakeOutcomeNotDelivered + ": the user withdrew it"
	agentWakeOutcomeCancelled    = "cancelled"
)

// agentWatchTimeout is how long one run of an interval watch may take.
func agentWatchTimeout(row store.AgentWake) time.Duration {
	if row.TimeoutSeconds > 0 {
		return time.Duration(row.TimeoutSeconds) * time.Second
	}
	return agentWakeDefaultScriptTimeout
}

// agentWakeFire is one fire of an interval row.
type agentWakeFire struct {
	// at is the fire's time, Unix milliseconds; it names the wake's message.
	at int64
	// missed is how many fires this one stands for besides itself.
	missed int64
	// closed says the missed fires came due while the app was not running.
	closed bool
	// rerun says the previous run of the row was cut by a quit.
	rerun bool
}

// agentWakeElapsed reports how many fires of an interval row have come due by
// now and the next fire after them. A row not yet due has none.
func agentWakeElapsed(nextAt, now, intervalMillis int64) (elapsed, next int64) {
	if intervalMillis < 1 {
		intervalMillis = 1
	}
	if nextAt > now {
		return 0, nextAt
	}
	elapsed = (now-nextAt)/intervalMillis + 1
	return elapsed, nextAt + elapsed*intervalMillis
}

// carriedMissed is what an earlier fire left uncounted: the fires of a row
// whose skipped fire no wake has carried to the thread yet. A run that woke
// nothing since, such as a watch run with no output, leaves them.
func carriedMissed(row store.AgentWake) int64 {
	if row.Undelivered == agentWakeOutcomeSkipped {
		return row.Missed
	}
	return 0
}

// fireDueAgentWakes fires every interval row whose time has come.
func (a *App) fireDueAgentWakes(now time.Time) {
	rows, err := a.store.DueAgentWakes(now.UnixMilli(), agentWakeDueBatch)
	if err != nil {
		logThreadRequestSweep("read due loops and watches", err)
		return
	}
	for _, row := range rows {
		elapsed, _ := agentWakeElapsed(row.NextAt, now.UnixMilli(), row.IntervalSeconds*1000)
		// The fire being served is one of the elapsed ones; the rest were
		// overtaken: the computer slept, or a run outlived its interval.
		err := a.fireAgentWake(row, now, agentWakeFire{at: now.UnixMilli(), missed: elapsed - 1 + carriedMissed(row)})
		if err != nil && !errors.Is(err, errAgentWakeNotFired) && !errors.Is(err, errAssistantsOff) {
			logThreadRequestSweep("fire "+row.Kind+" "+row.ID, err)
		}
	}
}

// recoverAgentWakesAtBoot rebuilds the supervisor from the rows, by the
// spec's restart rules: a row a quit left `running` is recorded
// `cancelled-by-quit` and runs again now, an interval row whose fire came due
// while the app was closed fires once with how many it missed, and every
// long-running watch starts from scratch. The rows of paused assistants wait
// for assistants to be turned on (resumeAssistants). A watch whose failure
// notice a previous process raised resumes its episode first, so its next
// clean run withdraws the notice.
func (a *App) recoverAgentWakesAtBoot() {
	if err := a.loadAgentWatchFailures(); err != nil {
		logThreadRequestSweep("read the watches whose failure was raised", err)
	}
	a.agentWakes.lifecycle.Lock()
	rows, err := a.store.ListLiveAgentWakes()
	if err != nil {
		a.agentWakes.lifecycle.Unlock()
		logThreadRequestSweep("read loops and watches at boot", err)
		return
	}
	recovered := a.recoverOrphanedAgentWakesLocked(rows)
	a.agentWakes.lifecycle.Unlock()
	a.restartAgentWakes(recovered, true)
}

// recoveredAgentWake is a live row as orphan recovery left it, and whether
// recovery recorded a cut run on it.
type recoveredAgentWake struct {
	row store.AgentWake
	cut bool
}

// recoverOrphanedAgentWakesLocked records `cancelled-by-quit` on each of
// rows left `running` by a run that is gone, and arms it again with the fire
// it was for due (store.RecoverAgentWake), so the replay it is owed outlasts
// whatever happens before a fire claims it. A row a registered run holds is
// live, not an orphan: a run gives up its registration in the same step as
// the pause refuses it (admitAgentWatchProcess), so a registered one goes on.
// A row whose recovery does not apply changed after it was read, so
// whatever changed it owns it. Both are left out. The caller holds the wake
// lifecycle lock across its read of rows and this, so no run registers,
// starts or gives up between them.
func (a *App) recoverOrphanedAgentWakesLocked(rows []store.AgentWake) []recoveredAgentWake {
	recovered := make([]recoveredAgentWake, 0, len(rows))
	for _, row := range rows {
		if a.agentWakeRegistered(row.ID) {
			continue
		}
		cut := row.State == store.AgentWakeRunning
		if cut {
			outcome := agentWakeOutcomeCutByQuit
			if row.LastOutcome == agentWakeOutcomeNotDelivered {
				// What was lost is the more useful thing to show.
				outcome = row.LastOutcome
			}
			armed, applied, err := a.store.RecoverAgentWake(row.ID, outcome)
			if err != nil {
				logThreadRequestSweep("recover "+row.Kind+" "+row.ID, err)
				continue
			}
			if !applied {
				continue
			}
			a.emitAgentWakeRow(row.ThreadID, armed)
			row = armed
		}
		recovered = append(recovered, recoveredAgentWake{row: row, cut: cut})
	}
	return recovered
}

// restartAgentWakes starts what orphan recovery left: every long-running
// watch's process, and the run of each row whose run was cut. Only at boot
// does an interval row whose fire came due fire here, as one that stands in
// for the fires missed while the app was closed; otherwise the sweep serves
// it as any due fire, counting what it missed. A cut run's row is due at the
// fire it was for, so that fire is one of its elapsed ones, not a missed
// one; if it does not start here, the sweep serves it. A fire the sweep
// claimed first loses its claim here, so the row runs once.
func (a *App) restartAgentWakes(recovered []recoveredAgentWake, boot bool) {
	now := time.Now()
	for _, r := range recovered {
		row := r.row
		if row.LongRunning {
			a.startAgentWatchProcess(row)
			continue
		}
		elapsed, _ := agentWakeElapsed(row.NextAt, now.UnixMilli(), row.IntervalSeconds*1000)
		if !r.cut && (elapsed == 0 || !boot) {
			continue
		}
		missed := elapsed
		if r.cut && missed > 0 {
			missed--
		}
		if err := a.fireAgentWake(row, now, agentWakeFire{
			at: now.UnixMilli(), missed: missed + carriedMissed(row), closed: missed > 0, rerun: r.cut,
		}); err != nil && !errors.Is(err, errAgentWakeNotFired) && !errors.Is(err, errAssistantsOff) {
			logThreadRequestSweep("fire "+row.Kind+" "+row.ID, err)
		}
	}
}

// errAgentWakeNotFired is a fire that did not start: its claim lost to a
// cancel, a re-registration or another run, or the app is shutting down.
var errAgentWakeNotFired = errors.New("the fire did not start")

// fireAgentWake claims one interval row and runs it off the sweep's
// goroutine: a watch script can take minutes, and a loop's delivery takes the
// thread's lock and can start its session. Nil is a started run;
// errAgentWakeNotFired a run of the row still registered, a lost claim or a
// shutdown; errAssistantsOff a paused assistant's row. The admission, the
// claim and the run's registration are one step under the wake lifecycle
// lock, so a cancel never lands between them, and the pause's stop pass,
// which takes the lock after the pause is set, either finds the run
// registered or was seen here.
//
// Registration is checked before the claim rather than undone after it:
// runs are added only under this lock, so a row with no run registered at
// the check still has none at the registration, and only a shutdown can
// refuse it, which leaves the row `running` for the next boot. A previous
// run unregisters in the step that arms its row (retireAgentWakeRun), so a
// row the sweep read armed has no run; the check keeps any other path from
// leaving a row `running` with no run to end it.
func (a *App) fireAgentWake(row store.AgentWake, now time.Time, fire agentWakeFire) error {
	_, next := agentWakeElapsed(row.NextAt, now.UnixMilli(), row.IntervalSeconds*1000)
	if next <= now.UnixMilli() {
		next = now.UnixMilli() + row.IntervalSeconds*1000
	}
	a.agentWakes.lifecycle.Lock()
	defer a.agentWakes.lifecycle.Unlock()
	if !a.agentWakeRunRegistrableLocked(row.ID) {
		return errAgentWakeNotFired
	}
	if err := a.refusePausedAssistantThread(row.ThreadID); err != nil {
		return err
	}
	claimedRow, claimed, err := a.store.ClaimAgentWake(row, now.UnixMilli(), next, fire.missed)
	if err != nil {
		return fmt.Errorf("claim %s %s: %w", row.Kind, row.ID, err)
	}
	if !claimed {
		return errAgentWakeNotFired
	}
	a.emitAgentWakeRow(row.ThreadID, claimedRow)
	row = claimedRow
	if a.agentWakeClaimedForTest != nil {
		a.agentWakeClaimedForTest(row)
	}
	// Not registered when shutting down: the row stays `running`, and the
	// next boot records the cut and runs it again.
	if !a.registerAgentWakeRunLocked(row, func(ctx context.Context, run *agentWakeRun) {
		select {
		case a.agentWakeSlots() <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-a.agentWakeSlots() }()
		a.runAgentWakeFire(ctx, run, row, fire)
	}) {
		return errAgentWakeNotFired
	}
	return nil
}

// currentAgentWake reads a row as it stands now, for what its previous wake
// came to: a run can outlive the wake before it. A failed read keeps the row
// the run started with.
func (a *App) currentAgentWake(row store.AgentWake) store.AgentWake {
	fresh, found, err := a.store.GetAgentWake(row.ID)
	if err != nil {
		logThreadRequestSweep("read "+row.Kind+" "+row.ID, err)
		return row
	}
	if !found {
		return row
	}
	return fresh
}

// runAgentWakeFire runs one claimed fire to its outcome.
func (a *App) runAgentWakeFire(ctx context.Context, run *agentWakeRun, row store.AgentWake, fire agentWakeFire) {
	var message usermessage.AgentMessage
	var current store.AgentWake
	var err error
	switch row.Kind {
	case store.AgentWakeLoop:
		message, err = renderAgentLoopWake(row, fire)
		current = a.currentAgentWake(row)
	case store.AgentWakeWatch:
		result := a.runAgentWatchScript(ctx, run, row)
		if ctx.Err() != nil {
			// Stopped: the row is not this run's any more. Shut down: the
			// row stays `running` and the next boot reruns it.
			return
		}
		a.noteAgentWakeStderr(run, row, result.stderr)
		failure, failed := result.failure(agentWatchTimeout(row))
		if !failed {
			a.endAgentWatchFailure(row.ID)
		}
		if outcome, wakes := result.outcome(agentWatchTimeout(row)); !wakes {
			a.retryHeldAgentWake(ctx, row)
			a.retireAgentWakeRun(run, row, func() {
				a.writeAgentWakeFinish(row, outcome, "", fire.missed, store.AgentWakeNoWake)
				if failed {
					a.noteAgentWatchFailed(row, failure, time.Now())
				}
			})
			return
		}
		current = a.currentAgentWake(row)
		message, err = renderAgentWatchWake(current, fire, result.stdout, result.stderr)
	default:
		a.finishAgentWakeFire(run, row, "error: unknown kind "+row.Kind, "", fire.missed, store.AgentWakeNoWake)
		return
	}
	if err != nil {
		a.finishAgentWakeFire(run, row, "error: "+err.Error(), "", fire.missed, store.AgentWakeNoWake)
		return
	}
	a.settleAgentWakeDelivery(run, row, fire, message.Text(), a.deliverAgentWake(ctx, current, fire.at, message))
}

// settleAgentWakeDelivery records on its row what delivering one fire's wake
// came to.
func (a *App) settleAgentWakeDelivery(run *agentWakeRun, row store.AgentWake, fire agentWakeFire, body string, err error) {
	// A watch's wake that did not reach the agent stays on its row.
	kept := keptAgentWakeOutput(body)
	switch {
	case err == nil:
		a.finishAgentWakeFire(run, row, agentWakeOutcomeDelivered, "", fire.missed, store.AgentWakeCarried)
	case errors.Is(err, store.ErrAgentWakeStillQueued):
		// One wake per row waits at a time. This fire counts as missed and
		// the next one that reaches the queue says so.
		a.finishAgentWakeFire(run, row, agentWakeOutcomeSkipped, kept, fire.missed+1, store.AgentWakeLost)
	case run.wasStopped():
		// A delivery a cancel cut short is no failure to log. Only an early
		// exit: a stopped run has lost its row, so retirement writes
		// nothing for it anyway (retireAgentWakeRun).
	case a.lifeCtx().Err() != nil:
		// The output never reached the queue, so the run is treated as not
		// run: due again at its own fire, which the next boot replays. A
		// watch's run is not repeated by its replay, which runs the script
		// again: its output stays on the row, and the outcome is what makes
		// the replay's wake say so.
		a.retireAgentWakeRun(run, row, func() {
			if _, err := a.store.ReleaseUndeliveredAgentWake(row.ID, agentWakeOutcomeNotDelivered, kept, fire.at); err != nil {
				logThreadRequestSweep("release undelivered "+row.Kind+" "+row.ID, err)
				return
			}
			a.emitAgentWakeChanged(row.ThreadID, row.ID)
		})
	default:
		log.Printf("agent wakes: %s %q on %s: %v", row.Kind, row.Label, row.ThreadID, err)
		a.finishAgentWakeFire(run, row, agentWakeOutcomeNotDelivered+": "+err.Error(), kept, fire.missed, store.AgentWakeLost)
	}
}

func (a *App) finishAgentWakeFire(
	run *agentWakeRun, row store.AgentWake, outcome, output string, missed int64, carry store.AgentWakeCarry,
) {
	a.retireAgentWakeRun(run, row, func() { a.writeAgentWakeFinish(row, outcome, output, missed, carry) })
}

// writeAgentWakeFinish is a finish's write, run inside retireAgentWakeRun.
func (a *App) writeAgentWakeFinish(row store.AgentWake, outcome, output string, missed int64, carry store.AgentWakeCarry) {
	if finished, applied, err := a.store.FinishAgentWake(row.ID, outcome, output, missed, carry); err != nil {
		logThreadRequestSweep("finish "+row.Kind+" "+row.ID, err)
	} else if applied {
		a.emitAgentWakeRow(row.ThreadID, finished)
	} else {
		a.emitAgentWakeChanged(row.ThreadID, row.ID)
	}
}

// retireAgentWakeRun ends an interval run and publishes the row it leaves
// behind as one step under the wake lifecycle lock, the lock a fire claims
// and registers under (fireAgentWake) and a cancel stops a run under
// (cancelAgentWake): the run is unregistered, then write puts the row back
// to armed. A pass that reads the row armed therefore finds no run of it
// registered and can start the next one. Only a run still registered for
// the row writes: one a cancel or a stop took out has lost the row, which a
// later registration and fire may have made another run's, and writes
// nothing. Every write that ends a run goes through here: a finish
// (FinishAgentWake) and a shutdown's release (ReleaseUndeliveredAgentWake).
//
// The sweep is nudged once the lock is released: the row's next fire may
// already be due (a run that outlived its interval), and a pass that read
// the row while it was running sleeps as if it had none.
func (a *App) retireAgentWakeRun(run *agentWakeRun, row store.AgentWake, write func()) {
	if hook := a.agentWakeRetiringForTest; hook != nil {
		hook(row, false)
	}
	a.agentWakes.lifecycle.Lock()
	if !a.unregisterAgentWakeRun(row.ID, run) {
		a.agentWakes.lifecycle.Unlock()
		return
	}
	write()
	if hook := a.agentWakeRetiringForTest; hook != nil {
		hook(row, true)
	}
	a.agentWakes.lifecycle.Unlock()
	a.nudgeThreadRequestSweep()
}
