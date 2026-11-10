package procutil

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

// DefaultTerminateGrace is how long a group gets to exit after SIGTERM before
// it is killed when GroupCommand.TerminateGrace is zero.
const DefaultTerminateGrace = 5 * time.Second

// groupDrainGrace bounds waiting for output after the group is gone. A process
// that escaped the group with setsid can still hold the pipe open; its output
// is not the command's, and the caller's sink must settle.
const groupDrainGrace = time.Second

// GroupCommand is one command RunGroup supervises. Stdin is the null device.
type GroupCommand struct {
	Dir  string
	Env  []string
	Argv []string
	// Output receives stdout and stderr interleaved, including what a child
	// the command left behind writes until the sweep stops it.
	Output io.Writer
	// TerminateGrace is how long the group gets between SIGTERM and SIGKILL on
	// cancellation and on the leftover sweep. Zero means DefaultTerminateGrace.
	TerminateGrace time.Duration
	// kill replaces KillConfiguredGroup in a test that needs a group no
	// signal ends.
	kill func(*exec.Cmd) error
}

// GroupOutcome is what RunGroup learned from one command. Leftovers reports
// that processes remained in the command's group after the command itself
// exited: they were stopped, but the exit code stays the command's own.
// ExitCode is -1 when the command did not exit on its own terms.
type GroupOutcome struct {
	ExitCode  int
	Leftovers bool
	// PipeErr is a failure of the output path: Output refused a write, or the
	// pipe did not close. The command still ran; what Output holds may be
	// incomplete.
	PipeErr error
}

// ErrGroupSurvived is returned when cancellation could not end the command:
// its leader ignored SIGKILL, which means it runs as another user (sudo).
// The command is abandoned still running, with its output pipe closed.
var ErrGroupSurvived = errors.New("procutil: the command could not be stopped and is still running")

// RunGroup runs a command as the leader of its own process group and returns
// once the whole group is gone and its output has settled.
//
// The output pipe belongs to this function, not to os/exec: a child the
// command left behind keeps writing into Output until the sweep stops it, and
// the run does not report finished while such a child still holds the pipe.
// Cancelling ctx stops the group with SIGTERM, then SIGKILL after the grace.
// The returned error is the start failure, what Wait reported, or
// ErrGroupSurvived when the kill had no effect.
func RunGroup(ctx context.Context, command GroupCommand) (GroupOutcome, error) {
	if len(command.Argv) == 0 {
		return GroupOutcome{ExitCode: -1}, errors.New("procutil: run group: empty argv")
	}
	grace := command.TerminateGrace
	if grace <= 0 {
		grace = DefaultTerminateGrace
	}
	kill := command.kill
	if kill == nil {
		kill = KillConfiguredGroup
	}
	cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	ConfigureGroup(cmd)
	// os/exec refuses a Cancel hook without CommandContext, and this function
	// drives cancellation itself so SIGTERM can precede the group kill.
	cmd.Cancel = nil
	reader, writer, err := os.Pipe()
	if err != nil {
		return GroupOutcome{ExitCode: -1}, err
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		return GroupOutcome{ExitCode: -1}, errors.Join(err, reader.Close(), writer.Close())
	}
	// The children hold the write end now. This copy would keep the drain
	// open until the grace below.
	pipeErr := writer.Close()
	drained := make(chan struct{})
	var copyErr error
	go func() {
		defer close(drained)
		if _, err := io.Copy(command.Output, reader); err != nil && !errors.Is(err, os.ErrClosed) {
			copyErr = err
			// Keep reading: a full pipe would stall the command on its next
			// write.
			_, _ = io.Copy(io.Discard, reader)
		}
	}()
	// settle closes the read end, which also ends a drain that a process
	// outside the group still feeds, and reports the output path's failures.
	settle := func() error {
		err := errors.Join(pipeErr, reader.Close())
		<-drained
		return errors.Join(err, copyErr)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		stopGroup(cmd, grace, kill)
		select {
		case waitErr = <-waited:
		case <-time.After(grace):
			return GroupOutcome{ExitCode: -1, PipeErr: settle()}, ErrGroupSurvived
		}
	}
	leftovers := ConfiguredGroupAlive(cmd)
	if leftovers {
		stopGroup(cmd, grace, kill)
	}
	select {
	case <-drained:
	case <-time.After(groupDrainGrace):
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return GroupOutcome{ExitCode: code, Leftovers: leftovers, PipeErr: settle()}, waitErr
}

// stopGroup terminates the group politely, then kills whatever ignored it.
// The grace runs on group emptiness, not on the leader: a shell wrapper
// exits at once on SIGTERM while the server it started still needs its
// moment to shut down.
func stopGroup(cmd *exec.Cmd, grace time.Duration, kill func(*exec.Cmd) error) {
	if err := TerminateConfiguredGroup(cmd); errors.Is(err, os.ErrProcessDone) {
		return
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			// A refused kill shows as a group that is still alive, which
			// every caller checks next.
			_ = kill(cmd)
			return
		case <-poll.C:
			if !ConfiguredGroupAlive(cmd) {
				return
			}
		}
	}
}
