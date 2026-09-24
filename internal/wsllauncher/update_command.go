package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// The launcher runs each WSL step of an in-app update as a command of a
// backend binary through wsl.exe (docs/specs/app-update.md). A command reports
// on stdout with supervise.UpdateEventPrefix and ends with one result report.

const (
	// UpdateCommandStallWindow is how long a command may go without a real
	// progress report. It is the trial's own window plus room for the two
	// announced steps that then wait without reporting: waiting up to
	// supervise.UpdateLockWait for the previous backend's lock, and stopping
	// the trial within supervise.DefaultStopTimeout.
	UpdateCommandStallWindow = supervise.TrialStallWindow + 15*time.Second
	// UpdateCommandCeiling bounds one command whatever it reports: the
	// trial's ceiling plus a snapshot or restore of the same order.
	UpdateCommandCeiling = 2 * supervise.TrialCeiling
	// updateCommandTermGrace is how long a command has to finish after
	// SIGTERM. The trial-run command stops its trial on SIGTERM, which takes
	// up to supervise.DefaultStopTimeout, and then reports its result.
	updateCommandTermGrace = supervise.DefaultStopTimeout + 15*time.Second
	// updateCommandKillGrace is how long SIGKILL, and then killing wsl.exe,
	// may take to end the command.
	updateCommandKillGrace = 10 * time.Second
	// updateSignalTimeout bounds the wsl.exe call that delivers a signal.
	updateSignalTimeout = 15 * time.Second
)

// UpdateCommandRunner runs update commands in one distro.
type UpdateCommandRunner struct {
	Distro string
	// Command substitutes for exec.CommandContext("wsl.exe", ...) in tests.
	Command CommandRunner
	// Rule is the command's stall rule. Zero means UpdateCommandStallWindow
	// and UpdateCommandCeiling.
	Rule supervise.StallRule
	// TermGrace and KillGrace override the stop escalation's waits.
	TermGrace time.Duration
	KillGrace time.Duration
	Logf      func(string, ...any)
}

// UpdateCommandStoppedError is a command the launcher stopped because it
// stopped reporting progress or ran past its ceiling. Result is the result
// the command reported while it stopped, if it reported one.
type UpdateCommandStoppedError struct {
	Reason string
	Result *supervise.UpdateEvent
}

func (e *UpdateCommandStoppedError) Error() string { return e.Reason }

// Run starts payload's command in the distro and returns its result report.
// onProgress receives every progress report, heartbeats included; only real
// reports count against the stall rule. A command that exits without a
// result, or is stopped, is an error. Cancelling ctx stops the command and
// returns ctx.Err().
func (r UpdateCommandRunner) Run(ctx context.Context, payload, command string, args []string, onProgress func(startupprogress.Progress)) (supervise.UpdateEvent, error) {
	if r.Distro == "" || payload == "" {
		return supervise.UpdateEvent{}, errors.New("wsllauncher: an update command needs a distro and a payload")
	}
	argv := append([]string{"-d", r.Distro, "--exec", payload, command}, args...)
	cmd := r.command(argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return supervise.UpdateEvent{}, fmt.Errorf("wire %s stdout: %w", command, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return supervise.UpdateEvent{}, fmt.Errorf("wire %s stderr: %w", command, err)
	}
	if err := cmd.Start(); err != nil {
		return supervise.UpdateEvent{}, fmt.Errorf("start %s through wsl.exe: %w", command, err)
	}
	go drainStderr(stderr, func(line string) { r.logf("wsllauncher: %s stderr: %s", command, line) })

	// The reader owns Wait: Wait closes stdout, so it runs only after the
	// reader has seen EOF, or reports still in the pipe would be lost. The
	// exit status is sent before events closes.
	events := make(chan supervise.UpdateEvent, 16)
	exited := make(chan error, 1)
	go func() {
		defer close(events)
		scanner := newStreamScanner(stdout)
		for scanner.Scan() {
			event, ok, err := supervise.ParseUpdateEvent(scanner.Text())
			switch {
			case err != nil:
				r.logf("wsllauncher: %s: %v", command, err)
			case ok:
				events <- event
			default:
				r.logf("wsllauncher: %s stdout: %s", command, scanner.Text())
			}
		}
		drainStream(scanner, stdout, command+" stdout", nil)
		exited <- cmd.Wait()
	}()

	run := &updateCommandRun{runner: r, command: command, cmd: cmd, events: events, exited: exited, onProgress: onProgress}
	return run.watch(ctx)
}

func (r UpdateCommandRunner) command(args ...string) *exec.Cmd {
	runner := r.Command
	if runner == nil {
		runner = exec.CommandContext
	}
	// Not bound to the caller's context: a killed wsl.exe may leave its
	// Linux process running, so every stop goes through updateCommandRun.stop.
	cmd := runner(context.Background(), "wsl.exe", args...)
	hideConsole(cmd)
	return cmd
}

func (r UpdateCommandRunner) rule() supervise.StallRule {
	if r.Rule.Window > 0 && r.Rule.Ceiling > 0 {
		return r.Rule
	}
	return supervise.StallRule{Window: UpdateCommandStallWindow, Ceiling: UpdateCommandCeiling}
}

func (r UpdateCommandRunner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

type updateCommandRun struct {
	runner     UpdateCommandRunner
	command    string
	cmd        *exec.Cmd
	events     <-chan supervise.UpdateEvent
	exited     <-chan error
	onProgress func(startupprogress.Progress)

	pid     int
	result  *supervise.UpdateEvent
	last    string
	waitErr error
	done    bool
}

func (u *updateCommandRun) watch(ctx context.Context) (supervise.UpdateEvent, error) {
	watch := supervise.NewStallWatch(u.runner.rule())
	defer watch.Stop()
	for {
		select {
		case event, ok := <-u.events:
			if !ok {
				u.waitErr, u.done = <-u.exited, true
				return u.finish()
			}
			if u.handle(event) {
				watch.Progress()
			}
		case <-watch.Stalled():
			return u.stopFor(fmt.Sprintf("the %s step stopped making progress for %s%s",
				u.command, u.runner.rule().Window, u.lastStep()))
		case <-watch.Expired():
			return u.stopFor(fmt.Sprintf("the %s step did not finish within %s%s",
				u.command, u.runner.rule().Ceiling, u.lastStep()))
		case <-ctx.Done():
			u.stop()
			return supervise.UpdateEvent{}, ctx.Err()
		}
	}
}

// handle applies one report and says whether it was real progress.
func (u *updateCommandRun) handle(event supervise.UpdateEvent) bool {
	switch event.Type {
	case supervise.UpdateEventStarted:
		if event.PID > 0 {
			u.pid = event.PID
		}
		return true
	case supervise.UpdateEventProgress:
		u.last = event.Progress.Detail
		if u.onProgress != nil {
			u.onProgress(*event.Progress)
		}
		return !event.Liveness
	case supervise.UpdateEventResult:
		result := event
		u.result = &result
		return true
	}
	return false
}

func (u *updateCommandRun) finish() (supervise.UpdateEvent, error) {
	if u.result != nil {
		return *u.result, nil
	}
	if u.waitErr != nil {
		return supervise.UpdateEvent{}, fmt.Errorf("the %s step exited without a result: %w", u.command, u.waitErr)
	}
	return supervise.UpdateEvent{}, fmt.Errorf("the %s step exited without a result", u.command)
}

func (u *updateCommandRun) stopFor(reason string) (supervise.UpdateEvent, error) {
	u.runner.logf("wsllauncher: stopping %s: %s", u.command, reason)
	u.stop()
	return supervise.UpdateEvent{}, &UpdateCommandStoppedError{Reason: reason, Result: u.result}
}

// stop ends the command. Killing wsl.exe may leave its Linux process
// running, so the signals go to the Linux pid the command reported, through
// wsl.exe: SIGTERM, which lets a trial-run command stop its trial, then
// SIGKILL, then wsl.exe itself. Reports that arrive meanwhile still count.
func (u *updateCommandRun) stop() {
	termGrace := u.runner.TermGrace
	if termGrace <= 0 {
		termGrace = updateCommandTermGrace
	}
	killGrace := u.runner.KillGrace
	if killGrace <= 0 {
		killGrace = updateCommandKillGrace
	}
	if u.pid > 0 {
		u.signal("TERM")
		if u.drainUntilExit(termGrace) {
			return
		}
		u.signal("KILL")
		if u.drainUntilExit(killGrace) {
			return
		}
	}
	if err := u.cmd.Process.Kill(); err != nil {
		u.runner.logf("wsllauncher: kill %s's wsl.exe: %v", u.command, err)
	}
	if !u.drainUntilExit(killGrace) {
		u.runner.logf("wsllauncher: %s's wsl.exe did not exit after it was killed", u.command)
	}
}

// drainUntilExit reads reports until the command exits or within passes.
func (u *updateCommandRun) drainUntilExit(within time.Duration) bool {
	if u.done {
		return true
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-u.events:
			if !ok {
				u.waitErr, u.done = <-u.exited, true
				return true
			}
			u.handle(event)
		case <-deadline.C:
			return false
		}
	}
}

func (u *updateCommandRun) signal(name string) {
	cmd := u.runner.command("-d", u.runner.Distro, "--exec", "kill", "-"+name, strconv.Itoa(u.pid))
	if err := cmd.Start(); err != nil {
		u.runner.logf("wsllauncher: send SIG%s to %s (pid %d): %v", name, u.command, u.pid, err)
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(updateSignalTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			u.runner.logf("wsllauncher: send SIG%s to %s (pid %d): %v", name, u.command, u.pid, err)
		}
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		u.runner.logf("wsllauncher: sending SIG%s to %s (pid %d) did not finish within %s", name, u.command, u.pid, updateSignalTimeout)
	}
}

func (u *updateCommandRun) lastStep() string {
	if u.last == "" {
		return ""
	}
	return " (last step: " + u.last + ")"
}
