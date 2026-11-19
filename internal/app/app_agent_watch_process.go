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
	"agent-overflow/internal/notify"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// A watch's script and process: an interval watch's run to its exit, and a
// long-running watch's process, supervised and restarted with backoff, its
// stdout lines batched into wakes.

// agentWatchResult is what one interval watch run came to.
type agentWatchResult struct {
	stdout   cappedOutput
	stderr   cappedOutput
	err      error
	timedOut bool
	// notStarted says err kept the process from starting at all.
	notStarted bool
}

// failure is how a run that had timeout to finish failed, for the watch's
// failure episode; false for a clean exit, with or without output.
func (r agentWatchResult) failure(timeout time.Duration) (notify.WatchFailure, bool) {
	var exit *exec.ExitError
	switch {
	case r.notStarted:
		return notify.WatchFailure{Cause: notify.WatchNotStarted}, true
	case r.timedOut:
		return notify.WatchFailure{Cause: notify.WatchTimedOut, Timeout: timeout}, true
	case errors.As(r.err, &exit):
		return notify.WatchFailure{Cause: notify.WatchExitStatus, ExitCode: exit.ExitCode()}, true
	case r.err != nil:
		return notify.WatchFailure{Cause: notify.WatchErrored}, true
	}
	return notify.WatchFailure{}, false
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
		return agentWatchResult{err: err, notStarted: true}
	}
	run.runIn(command.dir)
	defer run.runIn("")
	runCtx, cancel := context.WithTimeout(ctx, agentWatchTimeout(row))
	defer cancel()
	stdout := newHeadOutput(agentWakeOutputCap)
	stderr := newTailOutput(agentWakeStderrCap)
	// In-memory writers never block, so the group's drain ends as soon as the
	// script and anything it left behind have closed the pipes.
	group, err := procutil.StartGroup(runCtx, a.groupReaper(agentWatchOwner(row)), a.agentWatchGroup(row, command, stdout, stderr))
	if err != nil {
		return agentWatchResult{err: err, notStarted: true}
	}
	outcome, err := group.Wait()
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
		outcome, failure := a.runAgentWatchProcess(ctx, run, row)
		if ctx.Err() != nil {
			return
		}
		// Every end of the process is a failed attempt: it is meant to keep
		// running. A stop takes the run out under the lifecycle lock first.
		a.agentWakes.lifecycle.Lock()
		if !run.wasStopped() {
			a.noteAgentWatchFailed(row, failure, time.Now())
		}
		a.agentWakes.lifecycle.Unlock()
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
// returns its outcome and how it failed. Each stdout line is an event, and
// the first one ends the watch's failure episode; lines that arrive within
// agentWakeBatchWindow of the first are one wake. A batch whose previous wake
// is still queued keeps collecting until it can be queued.
func (a *App) runAgentWatchProcess(ctx context.Context, run *agentWakeRun, row store.AgentWake) (string, notify.WatchFailure) {
	notStarted := notify.WatchFailure{Cause: notify.WatchNotStarted}
	command, err := a.agentWatchCommand(row)
	if err != nil {
		return "error: " + err.Error(), notStarted
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
		return "error: could not start: " + err.Error(), notStarted
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
	printed := false
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
		case errors.Is(err, store.ErrAgentWakeStillQueued), ctx.Err() != nil:
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
			return "", notify.WatchFailure{}
		case line, open := <-lines:
			if !open {
				lines = nil
				if batch.pending() && flush == nil {
					flush = time.After(0)
				}
				continue
			}
			if !printed {
				printed = true
				a.endAgentWatchFailure(row.ID)
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
		return fmt.Sprintf("error: exit status %d%s", exit.ExitCode(), tail),
			notify.WatchFailure{Cause: notify.WatchExitStatus, ExitCode: exit.ExitCode()}
	case err != nil:
		return "error: " + err.Error() + tail, notify.WatchFailure{Cause: notify.WatchErrored}
	}
	return "exited" + tail, notify.WatchFailure{Cause: notify.WatchExited}
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
