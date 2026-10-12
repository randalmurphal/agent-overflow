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
		a.fireAgentWake(row, now, agentWakeFire{at: now.UnixMilli(), missed: elapsed - 1 + carriedMissed(row)})
	}
}

// recoverAgentWakesAtBoot rebuilds the supervisor from the rows, by the
// spec's restart rules: a row a quit left `running` is recorded
// `cancelled-by-quit` and runs again now, an interval row whose fire came due
// while the app was closed fires once with how many it missed, and every
// long-running watch starts from scratch.
func (a *App) recoverAgentWakesAtBoot() {
	rows, err := a.store.ListLiveAgentWakes()
	if err != nil {
		logThreadRequestSweep("read loops and watches at boot", err)
		return
	}
	now := time.Now()
	for _, row := range rows {
		cut := row.State == store.AgentWakeRunning
		if cut {
			outcome := agentWakeOutcomeCutByQuit
			if row.LastOutcome == agentWakeOutcomeNotDelivered {
				// What was lost is the more useful thing to show.
				outcome = row.LastOutcome
			}
			if _, err := a.store.RecoverAgentWake(row.ID, outcome); err != nil {
				logThreadRequestSweep("recover "+row.Kind+" "+row.ID, err)
				continue
			}
			a.emitThreadWakesChanged(row.ThreadID)
			row.State, row.LastOutcome = store.AgentWakeArmed, outcome
		}
		if row.LongRunning {
			a.startAgentWatchProcess(row)
			continue
		}
		elapsed, _ := agentWakeElapsed(row.NextAt, now.UnixMilli(), row.IntervalSeconds*1000)
		if !cut && elapsed == 0 {
			continue
		}
		a.fireAgentWake(row, now, agentWakeFire{
			at: now.UnixMilli(), missed: elapsed + carriedMissed(row), closed: elapsed > 0, rerun: cut,
		})
	}
}

// fireAgentWake claims one interval row and runs it off the sweep's
// goroutine: a watch script can take minutes, and a loop's delivery takes the
// thread's lock and can start its session. It reports whether the run
// started: false is a lost claim, a failed one (logged) or a shutdown.
func (a *App) fireAgentWake(row store.AgentWake, now time.Time, fire agentWakeFire) bool {
	_, next := agentWakeElapsed(row.NextAt, now.UnixMilli(), row.IntervalSeconds*1000)
	if next <= now.UnixMilli() {
		next = now.UnixMilli() + row.IntervalSeconds*1000
	}
	claimed, err := a.store.ClaimAgentWake(row.ID, now.UnixMilli(), next, fire.missed)
	if err != nil {
		logThreadRequestSweep("claim "+row.Kind+" "+row.ID, err)
		return false
	}
	if !claimed {
		return false
	}
	a.emitThreadWakesChanged(row.ThreadID)
	row.State, row.LastRunAt, row.NextAt, row.Missed = store.AgentWakeRunning, now.UnixMilli(), next, fire.missed
	ctx, run, ok := a.beginAgentWakeRun(row)
	if !ok {
		// Shutting down. The row stays `running`, and the next boot records
		// the cut and runs it again.
		return false
	}
	go func() {
		defer a.endAgentWakeRun(row.ID, run)
		select {
		case a.agentWakeSlots() <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-a.agentWakeSlots() }()
		a.runAgentWakeFire(ctx, run, row, fire)
	}()
	return true
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
	var body string
	switch row.Kind {
	case store.AgentWakeLoop:
		body = renderAgentLoopWake(row, fire)
	case store.AgentWakeWatch:
		result := a.runAgentWatchScript(ctx, row)
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
		body = renderAgentWatchWake(a.currentAgentWake(row), fire, result.stdout, result.stderr)
	default:
		a.finishAgentWakeFire(run, row, "error: unknown kind "+row.Kind, "", fire.missed, store.AgentWakeNoWake)
		return
	}
	err := a.deliverAgentWake(ctx, row, fire.at, body)
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
	case a.lifeCtx().Err() != nil:
		// The output never reached the queue, so the run is treated as not
		// run: due again at its own fire, which the next boot replays.
		if _, err := a.store.ReleaseUndeliveredAgentWake(row.ID, agentWakeOutcomeNotDelivered, kept, fire.at); err != nil {
			logThreadRequestSweep("release undelivered "+row.Kind+" "+row.ID, err)
		} else {
			a.emitThreadWakesChanged(row.ThreadID)
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
	if _, err := a.store.FinishAgentWake(row.ID, outcome, output, missed, carry); err != nil {
		logThreadRequestSweep("finish "+row.Kind+" "+row.ID, err)
	} else {
		a.emitThreadWakesChanged(row.ThreadID)
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

// runAgentWatchScript runs one interval watch to its exit.
func (a *App) runAgentWatchScript(ctx context.Context, row store.AgentWake) agentWatchResult {
	command, err := a.agentWatchCommand(row)
	if err != nil {
		return agentWatchResult{err: err}
	}
	runCtx, cancel := context.WithTimeout(ctx, agentWatchTimeout(row))
	defer cancel()
	stdout := newHeadOutput(agentWakeOutputCap)
	stderr := newTailOutput(agentWakeStderrCap)
	cmd := a.agentWatchExec(runCtx, row, command)
	// In-memory writers never block, so the WaitDelay ConfigureGroup sets
	// only cuts short a descendant that keeps the pipes open after the
	// script exits; what the script wrote has been read by then.
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return agentWatchResult{err: fmt.Errorf("could not start: %w", err)}
	}
	a.watchSessionProcess(cmd.Process.Pid, "agent-wake:"+row.ID)
	err = waitAgentWatch(cmd)
	a.releaseSessionProcess(cmd.Process.Pid)
	result := agentWatchResult{stdout: stdout.take(), stderr: stderr.take(), err: err}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		result.timedOut = true
	}
	return result
}

// agentWatchExec builds a watch's process: `/bin/sh -c` in its own process
// group, killed with the group on cancellation and, where the platform
// allows, when this app dies. The caller registers the started process with
// the orphan reaper, which covers macOS.
func (a *App) agentWatchExec(ctx context.Context, row store.AgentWake, command agentWatchCommandSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", row.Command)
	cmd.Dir, cmd.Env = command.dir, command.env
	procutil.ConfigureGroup(cmd)
	procutil.KillWithParent(cmd)
	return cmd
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
	ctx, run, ok := a.beginAgentWakeRun(row)
	if !ok {
		return
	}
	go func() {
		defer a.endAgentWakeRun(row.ID, run)
		a.superviseAgentWatch(ctx, run, row.ID)
	}()
}

func (a *App) superviseAgentWatch(ctx context.Context, run *agentWakeRun, wakeID string) {
	backoff := agentWakeRestartBase
	for {
		row, found, err := a.store.GetAgentWake(wakeID)
		if err != nil {
			logThreadRequestSweep("read long-running watch "+wakeID, err)
			return
		}
		if !found || row.State == store.AgentWakeCancelled || !row.LongRunning || run.wasStopped() {
			return
		}
		started, err := a.store.StartAgentWakeProcess(row.ID, time.Now().UnixMilli())
		if err != nil || !started {
			if err != nil {
				logThreadRequestSweep("start long-running watch "+wakeID, err)
			}
			return
		}
		a.emitThreadWakesChanged(row.ThreadID)
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

func (a *App) noteAgentWakeOutcome(run *agentWakeRun, wakeID, outcome string, carry store.AgentWakeCarry) {
	if run.wasStopped() {
		return
	}
	if _, err := a.store.NoteAgentWakeOutcome(wakeID, outcome, carry); err != nil {
		logThreadRequestSweep("note outcome of "+wakeID, err)
		return
	}
	a.emitThreadWakesChanged(run.threadID)
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
	a.emitThreadWakesChanged(row.ThreadID)
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
	cmd := a.agentWatchExec(ctx, row, command)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "error: " + err.Error()
	}
	stderr := newTailOutput(agentWakeStderrCap)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "error: could not start: " + err.Error()
	}
	pid := cmd.Process.Pid
	a.watchSessionProcess(pid, "agent-wake:"+row.ID)
	defer a.releaseSessionProcess(pid)
	lines := make(chan agentWatchLine, 64)
	go readAgentWatchLines(stdout, lines)

	batch := newHeadOutput(agentWakeOutputCap)
	var flush <-chan time.Time
	render := func(stderrPart cappedOutput) string {
		return renderAgentWatchWake(a.currentAgentWake(row), agentWakeFire{}, batch.peek(), stderrPart)
	}
	deliver := func() bool {
		// The stderr taken with a batch goes back if the batch has to wait,
		// so a held wake still carries what the script said beside it.
		taken := stderr.take()
		body := render(taken)
		err := a.deliverAgentWake(ctx, row, time.Now().UnixMilli(), body)
		switch {
		case err == nil:
			batch.take()
			a.noteAgentWakeStderr(run, row, taken)
			a.noteAgentWakeOutcome(run, row.ID, agentWakeOutcomeDelivered, store.AgentWakeCarried)
			return true
		case errors.Is(err, store.ErrAgentWakeStillQueued), ctx.Err() != nil:
			stderr.restore(taken)
			return false
		default:
			log.Printf("agent wakes: watch %q on %s: %v", row.Label, row.ThreadID, err)
			batch.take()
			if !run.wasStopped() {
				a.noteAgentWakeUndelivered(row.ThreadID, row.ID, agentWakeOutcomeNotDelivered+": "+err.Error(), body)
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
				a.noteAgentWakeUndelivered(row.ThreadID, row.ID, agentWakeOutcomeNotDelivered, render(stderr.take()))
			}
			if lines != nil {
				// The reader may be blocked handing over a line; let it reach
				// EOF, which the group kill brings.
				go func() {
					for range lines {
					}
				}()
			}
			_ = waitAgentWatch(cmd)
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
	err = waitAgentWatch(cmd)
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

func waitAgentWatch(cmd *exec.Cmd) error {
	err := cmd.Wait()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	return err
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

// beginAgentWakeRun registers a run for one row. False when one is already
// registered for it or the app is shutting down.
func (a *App) beginAgentWakeRun(row store.AgentWake) (context.Context, *agentWakeRun, bool) {
	a.agentWakes.mu.Lock()
	defer a.agentWakes.mu.Unlock()
	if a.shuttingDown.Load() || a.lifeCtx().Err() != nil {
		return nil, nil, false
	}
	if _, running := a.agentWakes.runs[row.ID]; running {
		return nil, nil, false
	}
	if a.agentWakes.runs == nil {
		a.agentWakes.runs = make(map[string]*agentWakeRun)
	}
	ctx, cancel := context.WithCancel(a.lifeCtx())
	run := &agentWakeRun{threadID: row.ThreadID, cancel: cancel, done: make(chan struct{})}
	a.agentWakes.runs[row.ID] = run
	// Joined at shutdown with the sweep, before the store closes: a run
	// writes its row when it ends.
	a.threadRequestsWG.Add(1)
	return ctx, run, true
}

func (a *App) endAgentWakeRun(wakeID string, run *agentWakeRun) {
	a.agentWakes.mu.Lock()
	if a.agentWakes.runs[wakeID] == run {
		delete(a.agentWakes.runs, wakeID)
	}
	a.agentWakes.mu.Unlock()
	run.cancel()
	close(run.done)
	a.threadRequestsWG.Done()
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
