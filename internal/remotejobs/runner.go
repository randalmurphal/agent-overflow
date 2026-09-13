package remotejobs

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"agent-overflow/internal/procutil"
)

// Outcome is what a runner learned from one command. Leftovers reports that
// processes remained in the command's group after the command itself exited:
// they were stopped, and the receipt says so, but the exit code stays the
// command's own.
type Outcome struct {
	ExitCode  int
	Leftovers bool
}

type Run func(context.Context, string, []string, io.Writer) (Outcome, error)

// terminateGrace is how long a group gets to exit after SIGTERM before it is
// killed, on cancellation, timeout, lease expiry and the leftover sweep alike.
const terminateGrace = 5 * time.Second

// drainGrace bounds waiting for output after the group is gone. A process that
// escaped the group with setsid can still hold the pipe open; its output is
// not the command's, and the log must settle.
const drainGrace = time.Second

// ProcessRunner owns the command's process group and its output pipe. The
// pipe is AO's, not os/exec's: a child the command left behind keeps writing
// into the log until the sweep stops it, and nothing here reports the command
// finished a second after it exits. Environment belongs to the destination; a
// requesting frontend or agent never supplies credentials or environment
// overrides.
func ProcessRunner(environment func() []string) Run {
	return func(ctx context.Context, cwd string, argv []string, output io.Writer) (Outcome, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = cwd
		cmd.Env = environment()
		procutil.ConfigureGroup(cmd)
		// os/exec refuses a Cancel hook without CommandContext, and this runner
		// drives cancellation itself so SIGTERM can precede the group kill.
		cmd.Cancel = nil
		reader, writer, err := os.Pipe()
		if err != nil {
			return Outcome{ExitCode: -1}, err
		}
		cmd.Stdout, cmd.Stderr = writer, writer
		if err := cmd.Start(); err != nil {
			reader.Close()
			writer.Close()
			return Outcome{ExitCode: -1}, err
		}
		writer.Close()
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_, _ = io.Copy(output, reader)
		}()
		exited := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			select {
			case <-ctx.Done():
				stopGroup(cmd)
			case <-exited:
			}
		}()
		waitErr := cmd.Wait()
		close(exited)
		<-stopped
		leftovers := procutil.ConfiguredGroupAlive(cmd)
		if leftovers {
			stopGroup(cmd)
		}
		select {
		case <-drained:
		case <-time.After(drainGrace):
			reader.Close()
			<-drained
		}
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return Outcome{ExitCode: code, Leftovers: leftovers}, waitErr
	}
}

// stopGroup terminates the group politely, then kills whatever ignored it.
// The grace runs on group emptiness, not on the leader: a shell wrapper
// exits at once on SIGTERM while the server it started still needs its
// moment to shut down.
func stopGroup(cmd *exec.Cmd) {
	if err := procutil.TerminateConfiguredGroup(cmd); errors.Is(err, os.ErrProcessDone) {
		return
	}
	deadline := time.NewTimer(terminateGrace)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			_ = procutil.KillConfiguredGroup(cmd)
			return
		case <-poll.C:
			if !procutil.ConfiguredGroupAlive(cmd) {
				return
			}
		}
	}
}
