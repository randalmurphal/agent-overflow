package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/procutil"
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
// for assistants to be turned on (resumeAssistants).
func (a *App) recoverAgentWakesAtBoot() {
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

// agentWakeRegistered reports whether the supervisor runs something of the
// row.
func (a *App) agentWakeRegistered(id string) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	_, running := a.agentWakes.runs[id]
	return running
}

// errAgentWakeNotFired is a fire that did not start: its claim lost to a
// cancel, a re-registration or another run, or the app is shutting down.
var errAgentWakeNotFired = errors.New("the fire did not start")

// fireAgentWake claims one interval row and runs it off the sweep's
// goroutine: a watch script can take minutes, and a loop's delivery takes the
// thread's lock and can start its session. Nil is a started run,
// errAgentWakeNotFired a lost claim or a shutdown, and errAssistantsOff a
// paused assistant's row. The admission, the claim and the run's
// registration are one step under the wake lifecycle lock, so a cancel never
// lands between them, and the pause's stop pass, which takes the lock after
// the pause is set, either finds the run registered or was seen here.
func (a *App) fireAgentWake(row store.AgentWake, now time.Time, fire agentWakeFire) error {
	_, next := agentWakeElapsed(row.NextAt, now.UnixMilli(), row.IntervalSeconds*1000)
	if next <= now.UnixMilli() {
		next = now.UnixMilli() + row.IntervalSeconds*1000
	}
	a.agentWakes.lifecycle.Lock()
	defer a.agentWakes.lifecycle.Unlock()
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
		if outcome, wakes := result.outcome(agentWatchTimeout(row)); !wakes {
			a.retryHeldAgentWake(row)
			a.finishAgentWakeFire(run, row, outcome, "", fire.missed, store.AgentWakeNoWake)
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
	case a.lifeCtx().Err() != nil, errors.Is(err, store.ErrThreadDeleteClaimed):
		// The output never reached the queue, so the run is treated as not
		// run: due again at its own fire, which the next boot replays, or,
		// for a thread a delete claimed, the sweep once a refused delete
		// releases it (DueAgentWakes).
		if _, err := a.store.ReleaseUndeliveredAgentWake(row.ID, agentWakeOutcomeNotDelivered, kept, fire.at); err != nil {
			logThreadRequestSweep("release undelivered "+row.Kind+" "+row.ID, err)
		} else {
			a.emitAgentWakeChanged(row.ThreadID, row.ID)
		}
	default:
		log.Printf("agent wakes: %s %q on %s: %v", row.Kind, row.Label, row.ThreadID, err)
		a.finishAgentWakeFire(run, row, agentWakeOutcomeNotDelivered+": "+err.Error(), kept, fire.missed, store.AgentWakeLost)
	}
}

func (a *App) finishAgentWakeFire(
	run *agentWakeRun, row store.AgentWake, outcome, output string, missed int64, carry store.AgentWakeCarry,
) {
	if run.wasStopped() {
		return
	}
	if finished, applied, err := a.store.FinishAgentWake(row.ID, outcome, output, missed, carry); err != nil {
		logThreadRequestSweep("finish "+row.Kind+" "+row.ID, err)
	} else if applied {
		a.emitAgentWakeRow(row.ThreadID, finished)
	} else {
		a.emitAgentWakeChanged(row.ThreadID, row.ID)
	}
	// The row is armed again with its next fire, which may be sooner than
	// the sweep's sleep.
	a.nudgeThreadRequestSweep()
}

// agentWatchResult is what one interval watch run came to.
type agentWatchResult struct {
	stdout   cappedOutput
	stderr   cappedOutput
	err      error
	timedOut bool
}

// outcome is the row's outcome for a run that had timeout to finish, and
// whether the run wakes the thread: only a clean exit with output does.
func (r agentWatchResult) outcome(timeout time.Duration) (string, bool) {
	var exit *exec.ExitError
	switch {
	case r.timedOut:
		return fmt.Sprintf("error: timed out after %s", timeout) + stderrSummary(r.stderr), false
	case errors.As(r.err, &exit):
		return fmt.Sprintf("error: exit status %d", exit.ExitCode()) + stderrSummary(r.stderr), false
	case r.err != nil:
		return "error: " + r.err.Error(), false
	case r.stdout.total == 0:
		return agentWakeOutcomeNoOutput + stderrSummary(r.stderr), false
	}
	return "", true
}

// stderrSummary is the last line of a run's stderr, for the row's outcome.
func stderrSummary(stderr cappedOutput) string {
	text := strings.TrimSpace(string(stderr.data))
	if text == "" {
		return ""
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[i+1:])
	}
	const most = 200
	if len(text) > most {
		text = clipUTF8(text, most) + "…"
	}
	return "; stderr: " + text
}

// runAgentWatchScript runs one interval watch to its exit, as the leader of
// its own process group the orphan reaper watches (agentWatchGroup), so what
// the script starts ends with it, on cancellation and when the backend dies.
func (a *App) runAgentWatchScript(ctx context.Context, run *agentWakeRun, row store.AgentWake) agentWatchResult {
	command, err := a.agentWatchCommand(row)
	if err != nil {
		return agentWatchResult{err: err}
	}
	run.runIn(command.dir)
	defer run.runIn("")
	runCtx, cancel := context.WithTimeout(ctx, agentWatchTimeout(row))
	defer cancel()
	stdout := newHeadOutput(agentWakeOutputCap)
	stderr := newTailOutput(agentWakeStderrCap)
	// In-memory writers never block, so the group's drain ends as soon as the
	// script and anything it left behind have closed the pipes.
	outcome, err := procutil.RunGroup(runCtx, a.groupReaper(agentWatchOwner(row)), a.agentWatchGroup(row, command, stdout, stderr))
	if err == nil {
		err = outcome.OutputErr()
	}
	result := agentWatchResult{stdout: stdout.take(), stderr: stderr.take(), err: err}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		result.timedOut = true
	}
	return result
}

// agentWatchGroup is a watch's process: `/bin/sh -c` leading its own process
// group, stopped with the group on cancellation and swept with what it left
// running when it exits. stdout and stderr stay apart: stdout is the wake,
// stderr the row's summary tail.
func (a *App) agentWatchGroup(row store.AgentWake, command agentWatchCommandSpec, stdout, stderr io.Writer) procutil.GroupCommand {
	return procutil.GroupCommand{
		Dir:    command.dir,
		Env:    command.env,
		Argv:   []string{"/bin/sh", "-c", row.Command},
		Output: stdout,
		Stderr: stderr,
	}
}

// agentWatchOwner names a watch's process group to the orphan reaper.
func agentWatchOwner(row store.AgentWake) string {
	return "agent-wake:" + row.ID
}

// agentWatchCommand is where and how a watch runs.
type agentWatchCommandSpec struct {
	dir string
	env []string
}

// agentWatchCommand resolves a watch's directory and environment: the
// thread's workspace unless the watch names its own directory, and the app's
// environment without anything of its own (no `AO_` variable, so no
// credential) plus the agent's home in AO_AGENT_HOME.
func (a *App) agentWatchCommand(row store.AgentWake) (agentWatchCommandSpec, error) {
	thread, err := a.store.GetThread(row.ThreadID)
	if err != nil {
		return agentWatchCommandSpec{}, fmt.Errorf("read the thread: %w", err)
	}
	agent, standing, err := a.store.GetStandingAgent(row.ThreadID)
	if err != nil {
		return agentWatchCommandSpec{}, fmt.Errorf("read the standing agent: %w", err)
	}
	if !standing {
		return agentWatchCommandSpec{}, errors.New("the thread is no longer a standing agent")
	}
	dir := agentWatchDir(row, thread.WorkspacePath)
	if err := a.requireIsolatedWorkspace(dir); err != nil {
		return agentWatchCommandSpec{}, err
	}
	if info, err := os.Stat(dir); err != nil {
		return agentWatchCommandSpec{}, fmt.Errorf("cwd: %w", err)
	} else if !info.IsDir() {
		return agentWatchCommandSpec{}, fmt.Errorf("cwd %s is not a directory", dir)
	}
	return agentWatchCommandSpec{dir: dir, env: agentWatchEnvironment(agent.Home)}, nil
}

func agentWatchEnvironment(home string) []string {
	base := appimage.Scrub(os.Environ())
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if !strings.HasPrefix(entry, "AO_") {
			env = append(env, entry)
		}
	}
	return append(env, "AO_AGENT_HOME="+home)
}

// startAgentWatchProcess supervises one long-running watch: its process,
// restarted with backoff when it exits, and its stdout lines batched into
// wakes. A row whose process is already supervised is left alone.
func (a *App) startAgentWatchProcess(row store.AgentWake) {
	a.startAgentWakeRun(row, func(ctx context.Context, run *agentWakeRun) {
		a.superviseAgentWatch(ctx, run, row.ID)
	})
}

func (a *App) superviseAgentWatch(ctx context.Context, run *agentWakeRun, wakeID string) {
	backoff := agentWakeRestartBase
	for {
		row, found, err := a.store.GetAgentWake(wakeID)
		if err != nil {
			logThreadRequestSweep("read long-running watch "+wakeID, err)
			return
		}
		if !found || row.State == store.AgentWakeCancelled || !row.LongRunning {
			return
		}
		if started, err := a.admitAgentWatchProcess(run, row); err != nil || !started {
			if err != nil && !errors.Is(err, errAssistantsOff) {
				logThreadRequestSweep("start long-running watch "+wakeID, err)
			}
			if errors.Is(err, errAssistantsOff) && a.agentWatchRefusedForTest != nil {
				a.agentWatchRefusedForTest(wakeID)
			}
			return
		}
		a.emitAgentWakeChanged(row.ThreadID, row.ID)
		startedAt := time.Now()
		outcome := a.runAgentWatchProcess(ctx, run, row)
		if ctx.Err() != nil {
			return
		}
		if time.Since(startedAt) >= agentWakeHealthyRun {
			backoff = agentWakeRestartBase
		}
		a.noteAgentWakeOutcome(run, row.ID, fmt.Sprintf("%s; restarting in %s", outcome, backoff), store.AgentWakeNoWake)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, agentWakeRestartCap)
	}
}

// admitAgentWatchProcess marks a long-running watch's process started unless
// its run was stopped or its thread is a paused assistant's, under the wake
// lifecycle lock the pause's stop pass takes: a start the pause did not
// refuse is a run it stops. A refused restart arms the row with the pause's
// outcome, as the stop pass does, and gives up the run's registration in the
// same step, so turning assistants on, which recovers under the same lock,
// finds either a supervisor that goes on or none and starts one.
func (a *App) admitAgentWatchProcess(run *agentWakeRun, row store.AgentWake) (bool, error) {
	a.agentWakes.lifecycle.Lock()
	defer a.agentWakes.lifecycle.Unlock()
	if run.wasStopped() {
		return false, nil
	}
	if err := a.refusePausedAssistantThread(row.ThreadID); err != nil {
		if errors.Is(err, errAssistantsOff) {
			a.unregisterAgentWakeRun(row.ID, run)
			if row.State == store.AgentWakeRunning {
				if _, armErr := a.store.PauseAgentWakeRun(row.ID, agentWakeOutcomePaused); armErr != nil {
					return false, armErr
				}
				a.emitAgentWakeChanged(row.ThreadID, row.ID)
			}
		}
		return false, err
	}
	return a.store.StartAgentWakeProcess(row.ID, time.Now().UnixMilli())
}

func (a *App) noteAgentWakeOutcome(run *agentWakeRun, wakeID, outcome string, carry store.AgentWakeCarry) {
	if run.wasStopped() {
		return
	}
	if _, err := a.store.NoteAgentWakeOutcome(wakeID, outcome, carry); err != nil {
		logThreadRequestSweep("note outcome of "+wakeID, err)
		return
	}
	a.emitAgentWakeChanged(run.threadID, wakeID)
}

// noteAgentWakeStderr keeps the tail of what a watch's run wrote to stderr
// on its row, for the activity rail. An interval run's stderr replaces the
// last one even when empty; a long-running process passes only what it
// wrote, since a batch with no stderr does not mean it stopped writing.
func (a *App) noteAgentWakeStderr(run *agentWakeRun, row store.AgentWake, stderr cappedOutput) {
	if run.wasStopped() || row.LongRunning && len(stderr.data) == 0 {
		return
	}
	if _, err := a.store.NoteAgentWakeStderr(row.ID, string(stderr.data)); err != nil {
		logThreadRequestSweep("note stderr of "+row.ID, err)
		return
	}
	a.emitAgentWakeChanged(row.ThreadID, row.ID)
}

// agentWatchLine is one stdout line of a long-running watch, clipped to the
// cap with how much of it was dropped.
type agentWatchLine struct {
	data    []byte
	dropped int64
}

// runAgentWatchProcess runs one long-running watch process to its exit and
// returns its outcome. Each stdout line is an event; lines that arrive within
// agentWakeBatchWindow of the first are one wake. A batch whose previous wake
// is still queued keeps collecting until it can be queued.
func (a *App) runAgentWatchProcess(ctx context.Context, run *agentWakeRun, row store.AgentWake) string {
	command, err := a.agentWatchCommand(row)
	if err != nil {
		return "error: " + err.Error()
	}
	run.runIn(command.dir)
	defer run.runIn("")
	stderr := newTailOutput(agentWakeStderrCap)
	// The group writes stdout into the pipe, and the reader turns it into
	// lines. The reader never stops reading, so the group's drain never
	// waits on it; the pipe closes once the group's Wait has returned, after
	// which nothing writes to it, and the reader sees EOF.
	stdout, stdoutSink := io.Pipe()
	group, err := procutil.StartGroup(ctx, a.groupReaper(agentWatchOwner(row)), a.agentWatchGroup(row, command, stdoutSink, stderr))
	if err != nil {
		_ = stdoutSink.Close()
		return "error: could not start: " + err.Error()
	}
	type groupEnd struct {
		outcome procutil.GroupOutcome
		err     error
	}
	ended := make(chan groupEnd, 1)
	go func() {
		outcome, err := group.Wait()
		_ = stdoutSink.Close()
		ended <- groupEnd{outcome: outcome, err: err}
	}()
	lines := make(chan agentWatchLine, 64)
	go readAgentWatchLines(stdout, lines)

	batch := newHeadOutput(agentWakeOutputCap)
	var flush <-chan time.Time
	render := func(current store.AgentWake, stderrPart cappedOutput) (usermessage.AgentMessage, error) {
		return renderAgentWatchWake(current, agentWakeFire{}, batch.peek(), stderrPart)
	}
	deliver := func() bool {
		// The stderr taken with a batch goes back if the batch has to wait,
		// so a held wake still carries what the script said beside it.
		taken := stderr.take()
		current := a.currentAgentWake(row)
		message, err := render(current, taken)
		if err == nil {
			err = a.deliverAgentWake(ctx, current, time.Now().UnixMilli(), message)
		}
		switch {
		case err == nil:
			batch.take()
			a.noteAgentWakeStderr(run, row, taken)
			a.noteAgentWakeOutcome(run, row.ID, agentWakeOutcomeDelivered, store.AgentWakeCarried)
			return true
		case errors.Is(err, store.ErrAgentWakeStillQueued), errors.Is(err, store.ErrThreadDeleteClaimed), ctx.Err() != nil:
			// Held: a wake still queued, or a delete deciding whether the
			// thread stays.
			stderr.restore(taken)
			return false
		default:
			log.Printf("agent wakes: watch %q on %s: %v", row.Label, row.ThreadID, err)
			batch.take()
			if !run.wasStopped() {
				a.noteAgentWakeUndelivered(row.ThreadID, row.ID, agentWakeOutcomeNotDelivered+": "+err.Error(), message.Text())
			}
			return true
		}
	}
	for lines != nil || batch.pending() {
		select {
		case <-ctx.Done():
			if batch.pending() && !run.wasStopped() {
				// Output that never reached the queue is not replayed; the
				// script's own cursor decides whether its next run repeats
				// it. The row keeps it for the agent to read.
				if message, err := render(a.currentAgentWake(row), stderr.take()); err != nil {
					log.Printf("agent wakes: watch %q on %s: %v", row.Label, row.ThreadID, err)
				} else {
					a.noteAgentWakeUndelivered(row.ThreadID, row.ID, agentWakeOutcomeNotDelivered, message.Text())
				}
			}
			if lines != nil {
				// The reader may be blocked handing over a line; let it reach
				// EOF, which the end of the group brings.
				go func() {
					for range lines {
					}
				}()
			}
			<-ended
			return ""
		case line, open := <-lines:
			if !open {
				lines = nil
				if batch.pending() && flush == nil {
					flush = time.After(0)
				}
				continue
			}
			batch.writeLine(line)
			if flush == nil {
				flush = time.After(agentWakeBatchWindow)
			}
		case <-flush:
			flush = nil
			if !deliver() {
				flush = time.After(agentWakeBatchWindow)
			}
		}
	}
	end := <-ended
	err = end.err
	if err == nil {
		err = end.outcome.OutputErr()
	}
	rest := stderr.take()
	a.noteAgentWakeStderr(run, row, rest)
	tail := stderrSummary(rest)
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return fmt.Sprintf("error: exit status %d%s", exit.ExitCode(), tail)
	case err != nil:
		return "error: " + err.Error() + tail
	}
	return "exited" + tail
}

// readAgentWatchLines splits a watch's stdout into lines, each clipped to the
// output cap, and closes lines at EOF. It never stops reading, so a watch
// whose wakes are held up never blocks on its pipe.
func readAgentWatchLines(stdout io.Reader, lines chan<- agentWatchLine) {
	defer close(lines)
	reader := bufio.NewReaderSize(stdout, 32<<10)
	var line agentWatchLine
	for {
		chunk, err := reader.ReadSlice('\n')
		if room := agentWakeOutputCap - len(line.data); room > 0 {
			keep := min(room, len(chunk))
			line.data = append(line.data, chunk[:keep]...)
			line.dropped += int64(len(chunk) - keep)
		} else {
			line.dropped += int64(len(chunk))
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line.data) > 0 || line.dropped > 0 {
			line.data = bytes.TrimSuffix(line.data, []byte("\n"))
			lines <- line
			line = agentWatchLine{}
		}
		if err != nil {
			return
		}
	}
}

// startAgentWakeRun registers a run for one row and runs body on its own
// goroutine, joined at shutdown with the sweep before the store closes: a
// run writes its row when it ends. False, with nothing run, when one is
// already registered for it or the app is shutting down.
func (a *App) startAgentWakeRun(row store.AgentWake, body func(ctx context.Context, run *agentWakeRun)) bool {
	a.agentWakes.lifecycle.Lock()
	defer a.agentWakes.lifecycle.Unlock()
	return a.registerAgentWakeRunLocked(row, body)
}

// registerAgentWakeRunLocked is startAgentWakeRun for a caller that holds
// the wake lifecycle lock.
func (a *App) registerAgentWakeRunLocked(row store.AgentWake, body func(ctx context.Context, run *agentWakeRun)) bool {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.shuttingDown.Load() || a.lifeCtx().Err() != nil {
		return false
	}
	if _, running := a.agentWakes.runs[row.ID]; running {
		return false
	}
	if a.agentWakes.runs == nil {
		a.agentWakes.runs = make(map[string]*agentWakeRun)
	}
	ctx, cancel := context.WithCancel(a.lifeCtx())
	run := &agentWakeRun{threadID: row.ThreadID, cancel: cancel, done: make(chan struct{})}
	a.agentWakes.runs[row.ID] = run
	if !a.threadRequestsWG.Go(func() {
		defer a.endAgentWakeRun(row.ID, run)
		body(ctx, run)
	}) {
		delete(a.agentWakes.runs, row.ID)
		cancel()
		return false
	}
	return true
}

func (a *App) endAgentWakeRun(wakeID string, run *agentWakeRun) {
	a.unregisterAgentWakeRun(wakeID, run)
	run.cancel()
	close(run.done)
}

// unregisterAgentWakeRun removes run from the rows the supervisor runs, if
// it still holds wakeID.
func (a *App) unregisterAgentWakeRun(wakeID string, run *agentWakeRun) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.agentWakes.runs[wakeID] == run {
		delete(a.agentWakes.runs, wakeID)
	}
}

// stopAgentWakeRun stops the run of one row, if any, without waiting.
func (a *App) stopAgentWakeRun(wakeID string) *agentWakeRun {
	a.agentWakes.mu.Lock()
	run := a.agentWakes.runs[wakeID]
	delete(a.agentWakes.runs, wakeID)
	a.agentWakes.mu.Unlock()
	if run != nil {
		run.mu.Lock()
		run.stopped = true
		run.mu.Unlock()
		run.cancel()
	}
	return run
}

// stopLongRunningAgentWatch stops a row's long-running process and waits a
// bounded time for it to exit, so a replacement never runs beside it. An
// interval run in flight is left to finish: its row stays `running` and the
// new registration applies from the next run.
func (a *App) stopLongRunningAgentWatch(wakeID string) {
	a.agentWakes.mu.Lock()
	run := a.agentWakes.runs[wakeID]
	a.agentWakes.mu.Unlock()
	if run == nil {
		return
	}
	row, found, err := a.store.GetAgentWake(wakeID)
	if err == nil && found && row.State == store.AgentWakeRunning && !row.LongRunning {
		return
	}
	if stopped := a.stopAgentWakeRun(wakeID); stopped != nil {
		select {
		case <-stopped.done:
		case <-time.After(agentWakeStopWait):
			log.Printf("agent wakes: the process of %s did not exit within %s", wakeID, agentWakeStopWait)
		}
	}
}

// stopAgentWakeRunsForThread stops every run of one thread.
func (a *App) stopAgentWakeRunsForThread(threadID string) {
	a.agentWakes.mu.Lock()
	var ids []string
	for id, run := range a.agentWakes.runs {
		if run.threadID == threadID {
			ids = append(ids, id)
		}
	}
	a.agentWakes.mu.Unlock()
	for _, id := range ids {
		a.stopAgentWakeRun(id)
	}
}

func (a *App) agentWakeSlots() chan struct{} {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.agentWakes.slots == nil {
		a.agentWakes.slots = make(chan struct{}, agentWakeRunFanOut)
	}
	return a.agentWakes.slots
}
