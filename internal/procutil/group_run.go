package procutil

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// DefaultTerminateGrace is how long a group gets to exit after SIGTERM before
// it is killed when GroupCommand.TerminateGrace is zero.
const DefaultTerminateGrace = 5 * time.Second

// groupDrainGrace bounds waiting for output after the group is gone. A kept
// leftover, or a process that escaped the group with setsid, can still hold
// the pipe open; its output is not the command's, and the caller's sink must
// settle.
const groupDrainGrace = time.Second

// GroupCommand is one command StartGroup supervises. Stdin is the null device.
type GroupCommand struct {
	Dir  string
	Env  []string
	Argv []string
	// Output receives stdout and stderr interleaved, including what a child
	// the command left behind writes until the sweep stops it. It is written
	// from one goroutine, and never after Wait returns.
	Output io.Writer
	// TerminateGrace is how long the group gets between SIGTERM and SIGKILL on
	// cancellation and on the leftover sweep. Zero means DefaultTerminateGrace.
	TerminateGrace time.Duration
	// KeepLeftovers leaves running the processes still in the group when the
	// command exits on its own, instead of sweeping them. Cancellation still
	// stops the whole group. A worktree setup command or a workflow tool
	// command may start a service that is meant to outlive it.
	KeepLeftovers bool
	// Terminal, when set, is the command's controlling terminal. Stdin and
	// Output are unchanged. The caller closes it after Wait returns.
	Terminal *Terminal
	// kill replaces KillConfiguredGroup in a test that needs a group no
	// signal ends.
	kill func(*exec.Cmd) error
}

// GroupOutcome is what Wait learned from one command. Leftovers reports that
// processes remained in the command's group after the command itself exited:
// they were stopped unless KeepLeftovers was set, and the exit code stays the
// command's own. ExitCode is -1 when the command left no exit status: it did
// not start, died to a signal or was abandoned.
type GroupOutcome struct {
	ExitCode  int
	Leftovers bool
	// Stopped reports that ctx ended while the command ran, so the group was
	// signaled. An exit status is then the command's answer to the signal,
	// not its result.
	Stopped bool
	// OutputHeld reports that the output pipe was still open when the drain
	// grace ended: a kept leftover, or a process that left the group, holds
	// it. What that process writes later is lost.
	OutputHeld bool
	// PipeErr is a failure of the output path: Output refused a write, or the
	// pipe did not close. The command still ran; what Output holds may be
	// incomplete.
	PipeErr error
}

// ErrOutputHeld is OutputErr's error for a command whose output pipe a process
// it left running still held when the drain grace ended.
var ErrOutputHeld = errors.New("procutil: a process the command left running still holds its output")

// OutputErr is the output path's failure, for a caller that fails a command
// whose output did not arrive whole: PipeErr, and ErrOutputHeld when
// OutputHeld.
func (o GroupOutcome) OutputErr() error {
	if o.OutputHeld {
		return errors.Join(o.PipeErr, ErrOutputHeld)
	}
	return o.PipeErr
}

// Reaper ends the process groups it watches when this process dies without
// stopping them: the app's orphan reaper. A signal to the leader alone, such
// as Linux's parent-death signal, would leave running what a shell started.
// StartGroup has it watch every group, by the leader's pid, before StartGroup
// returns, and Wait releases the group once it has ended or only leftovers
// KeepLeftovers keeps remain. A group the kill could not end stays watched.
type Reaper interface {
	Watch(pgid int)
	Release(pgid int)
}

// ErrNoReaper is StartGroup's error for a command given no Reaper: no group
// starts without one to end it with this process.
var ErrNoReaper = errors.New("procutil: run group: no reaper watches the group")

// ErrGroupSurvived is returned when cancellation could not end the command:
// its leader ignored SIGKILL, which means it runs as another user (sudo).
// The command is abandoned still running, with its output pipe closed.
var ErrGroupSurvived = errors.New("procutil: the command could not be stopped and is still running")

// Group is a started command that leads its own process group. Wait must be
// called to release it.
type Group struct {
	ctx           context.Context
	cmd           *exec.Cmd
	reaper        Reaper
	grace         time.Duration
	keepLeftovers bool
	kill          func(*exec.Cmd) error

	reader  *os.File
	pipeErr error
	// drained closes when the copy into Output has ended; copyErr is set
	// before that.
	drained chan struct{}
	copyErr error

	once    sync.Once
	outcome GroupOutcome
	err     error
}

// StartGroup starts a command as the leader of its own process group,
// watched by reaper, and returns once it runs. A ctx that is already done
// starts nothing and its error is returned; otherwise the error is the start
// failure.
//
// The output pipe belongs to the Group, not to os/exec: a child the command
// left behind keeps writing into Output, which is drained from the moment the
// command starts.
func StartGroup(ctx context.Context, reaper Reaper, command GroupCommand) (*Group, error) {
	if reaper == nil {
		return nil, ErrNoReaper
	}
	if len(command.Argv) == 0 {
		return nil, errors.New("procutil: run group: empty argv")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	group := &Group{
		ctx: ctx, reaper: reaper, grace: command.TerminateGrace, keepLeftovers: command.KeepLeftovers,
		kill: command.kill, drained: make(chan struct{}),
	}
	if group.grace <= 0 {
		group.grace = DefaultTerminateGrace
	}
	if group.kill == nil {
		group.kill = KillConfiguredGroup
	}
	argv := command.Argv
	if command.Terminal != nil {
		var err error
		if argv, err = command.Terminal.command(argv, command.Dir); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	ConfigureGroup(cmd)
	// os/exec refuses a Cancel hook without CommandContext, and Wait drives
	// cancellation itself so SIGTERM can precede the group kill.
	cmd.Cancel = nil
	if command.Terminal != nil {
		command.Terminal.attach(cmd)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, reader.Close(), writer.Close())
	}
	group.cmd, group.reader = cmd, reader
	// The children hold the write end now. This copy would keep the drain
	// open until the grace in Wait.
	group.pipeErr = writer.Close()
	go func() {
		defer close(group.drained)
		if _, err := io.Copy(command.Output, reader); err != nil && !errors.Is(err, os.ErrClosed) {
			group.copyErr = err
			// Keep reading: a full pipe would stall the command on its next
			// write.
			_, _ = io.Copy(io.Discard, reader)
		}
	}()
	reaper.Watch(cmd.Process.Pid)
	return group, nil
}

// Wait returns once the whole group is gone and its output has settled. The
// run does not report finished while a child the sweep stops still holds the
// pipe. Cancelling the ctx given to StartGroup stops the group with SIGTERM,
// then SIGKILL after the grace. The returned error is what the command's own
// Wait reported, or ErrGroupSurvived when the kill had no effect. Every call
// returns the first call's result.
func (g *Group) Wait() (GroupOutcome, error) {
	g.once.Do(func() { g.outcome, g.err = g.supervise() })
	return g.outcome, g.err
}

func (g *Group) supervise() (GroupOutcome, error) {
	waited := make(chan error, 1)
	go func() { waited <- g.cmd.Wait() }()
	var waitErr error
	stopped := false
	select {
	case waitErr = <-waited:
	case <-g.ctx.Done():
		stopped = true
		stopGroup(g.cmd, g.grace, g.kill)
		select {
		case waitErr = <-waited:
		case <-time.After(g.grace):
			return GroupOutcome{ExitCode: -1, Stopped: true, PipeErr: g.settle()}, ErrGroupSurvived
		}
	}
	leftovers := ConfiguredGroupAlive(g.cmd)
	ending := true
	if leftovers && !g.keepLeftovers {
		ending = stopGroup(g.cmd, g.grace, g.kill)
	}
	if ending {
		g.reaper.Release(g.cmd.Process.Pid)
	}
	held := false
	select {
	case <-g.drained:
	case <-time.After(groupDrainGrace):
		held = true
	}
	code := -1
	if g.cmd.ProcessState != nil {
		code = g.cmd.ProcessState.ExitCode()
	}
	return GroupOutcome{
		ExitCode: code, Leftovers: leftovers, Stopped: stopped, OutputHeld: held, PipeErr: g.settle(),
	}, waitErr
}

// settle closes the read end, which also ends a drain that a process outside
// the sweep still feeds, and reports the output path's failures.
func (g *Group) settle() error {
	err := errors.Join(g.pipeErr, g.reader.Close())
	<-g.drained
	return errors.Join(err, g.copyErr)
}

// RunGroup starts a command with StartGroup and waits for it. ExitCode is -1
// when the command did not start.
func RunGroup(ctx context.Context, reaper Reaper, command GroupCommand) (GroupOutcome, error) {
	group, err := StartGroup(ctx, reaper, command)
	if err != nil {
		return GroupOutcome{ExitCode: -1}, err
	}
	return group.Wait()
}

// stopGroup terminates the group politely, then kills whatever ignored it,
// and reports whether the group is ending: it was gone, or took SIGKILL.
// The grace runs on group emptiness, not on the leader: a shell wrapper
// exits at once on SIGTERM while the server it started still needs its
// moment to shut down.
func stopGroup(cmd *exec.Cmd, grace time.Duration, kill func(*exec.Cmd) error) bool {
	if err := TerminateConfiguredGroup(cmd); errors.Is(err, os.ErrProcessDone) {
		return true
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			// On cancellation a refused kill also shows as a leader that
			// does not exit, which Wait reports as ErrGroupSurvived.
			err := kill(cmd)
			return err == nil || errors.Is(err, os.ErrProcessDone)
		case <-poll.C:
			if !ConfiguredGroupAlive(cmd) {
				return true
			}
		}
	}
}
