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

// GroupCommand is one command StartGroup supervises.
type GroupCommand struct {
	Dir  string
	Env  []string
	Argv []string
	// Stdin is the command's standard input; nil is the null device.
	Stdin io.Reader
	// Output receives stdout and stderr interleaved, including what a child
	// the command left behind writes until the sweep stops it. It is written
	// from one goroutine, and never after Wait returns.
	Output io.Writer
	// Stderr, when set, receives stderr instead of Output, from a pipe of
	// its own that the Group owns, drains and settles as it does Output's.
	// It is written from one goroutine, and never after Wait returns.
	Stderr io.Writer
	// TerminateGrace is how long the group gets between SIGTERM and SIGKILL on
	// cancellation and on the leftover sweep. Zero means DefaultTerminateGrace.
	TerminateGrace time.Duration
	// KeepLeftovers leaves running the processes still in the group when the
	// command exits on its own, instead of sweeping them. Cancellation still
	// stops the whole group. The kept processes are released from the
	// reaper too, so they outlive this process. A worktree setup command or a
	// workflow tool command may start a service that is meant to outlive it.
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
	// OutputHeld reports that an output pipe was still open when the drain
	// grace ended: a kept leftover, or a process that left the group, holds
	// it. What that process writes later is lost.
	OutputHeld bool
	// PipeErr is a failure of the output path: Output or Stderr refused a
	// write, or a pipe did not close. The command still ran; what Output and
	// Stderr hold may be incomplete.
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
// returns, and Wait releases the group once no member is alive; a group the
// kill could not end stays watched. Leftovers KeepLeftovers keeps after the
// command exits on its own are handed off: released alive, they outlive this
// process.
type Reaper interface {
	Watch(pgid int)
	Release(pgid int)
}

// ErrNoReaper is StartGroup's error for a command given no Reaper: no group
// starts without one to end it with this process.
var ErrNoReaper = errors.New("procutil: run group: no reaper watches the group")

// ErrGroupSurvived is returned when a process of the group is still alive
// after SIGKILL: on cancellation the leader ignored it, or a process the
// command left outlived the leftover sweep. Such a process runs as another
// user (sudo) or is stuck in the kernel. It is abandoned still running, with
// the output pipe closed and the group still watched.
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

	// readers are the read ends of the output pipes: Output's, then
	// Stderr's when the command has one.
	readers []*os.File
	pipeErr error
	// drained closes when every copy into Output and Stderr has ended;
	// copyErr is set before that.
	drained chan struct{}
	copyMu  sync.Mutex
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
// The output pipes belong to the Group, not to os/exec: a child the command
// left behind keeps writing into Output and Stderr, which are drained from the
// moment the command starts.
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
	cmd.Stdin = command.Stdin
	ConfigureGroup(cmd)
	// os/exec refuses a Cancel hook without CommandContext, and Wait drives
	// cancellation itself so SIGTERM can precede the group kill.
	cmd.Cancel = nil
	if command.Terminal != nil {
		command.Terminal.attach(cmd)
	}
	sinks := []io.Writer{command.Output}
	if command.Stderr != nil {
		sinks = append(sinks, command.Stderr)
	}
	readers, writers, err := outputPipes(len(sinks))
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = writers[0], writers[len(writers)-1]
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, closeFiles(readers), closeFiles(writers))
	}
	group.cmd, group.readers = cmd, readers
	// The children hold the write ends now. These copies would keep the
	// drain open until the grace in Wait.
	group.pipeErr = closeFiles(writers)
	var copies sync.WaitGroup
	for i, reader := range readers {
		copies.Add(1)
		go func() {
			defer copies.Done()
			group.copyOutput(sinks[i], reader)
		}()
	}
	go func() {
		copies.Wait()
		close(group.drained)
	}()
	reaper.Watch(cmd.Process.Pid)
	return group, nil
}

// outputPipes opens n pipes and returns their read and write ends in order.
func outputPipes(n int) (readers, writers []*os.File, err error) {
	for range n {
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, nil, errors.Join(err, closeFiles(readers), closeFiles(writers))
		}
		readers, writers = append(readers, reader), append(writers, writer)
	}
	return readers, writers, nil
}

func closeFiles(files []*os.File) error {
	var err error
	for _, file := range files {
		err = errors.Join(err, file.Close())
	}
	return err
}

// copyOutput copies one pipe into its sink until the pipe ends.
func (g *Group) copyOutput(sink io.Writer, reader *os.File) {
	if _, err := io.Copy(sink, reader); err != nil && !errors.Is(err, os.ErrClosed) {
		g.copyMu.Lock()
		g.copyErr = errors.Join(g.copyErr, err)
		g.copyMu.Unlock()
		// Keep reading: a full pipe would stall the command on its next
		// write.
		_, _ = io.Copy(io.Discard, reader)
	}
}

// Wait returns once the whole group is gone and its output has settled. The
// run does not report finished while a child the sweep stops still holds the
// pipe. Cancelling the ctx given to StartGroup stops the group with SIGTERM,
// then SIGKILL after the grace. The returned error is what the command's own
// Wait reported, joined with ErrGroupSurvived when a process outlived the
// kill. Every call returns the first call's result.
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
		// A failed stop shows next: a leader that does not exit is
		// ErrGroupSurvived, members that remain are the leftover sweep's.
		_ = stopGroup(g.cmd, g.grace, g.kill)
		select {
		case waitErr = <-waited:
		case <-time.After(g.grace):
			return GroupOutcome{ExitCode: -1, Stopped: true, PipeErr: g.settle()}, ErrGroupSurvived
		}
	}
	leftovers := ConfiguredGroupAlive(g.cmd)
	if leftovers && g.keepLeftovers && !stopped {
		g.handOffKeptLeftovers()
	} else {
		waitErr = errors.Join(waitErr, g.releaseWhenEmpty(leftovers))
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

// releaseWhenEmpty ends what the leader left in the group and releases the
// group's watch once no member is alive. The leader's exit is not the
// group's, so this is the release after Wait. A group the stop could not
// empty stays watched and is reported as ErrGroupSurvived.
func (g *Group) releaseWhenEmpty(leftovers bool) error {
	if leftovers {
		if err := stopGroup(g.cmd, g.grace, g.kill); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errors.Join(ErrGroupSurvived, err)
		}
	}
	g.reaper.Release(g.cmd.Process.Pid)
	return nil
}

// handOffKeptLeftovers releases the watch on a group whose command exited on
// its own leaving processes KeepLeftovers keeps. This is the intended
// hand-off, not the release after Wait: a dev server or container a setup
// command started must outlive the backend, through an update or restart, so
// it is released alive and never swept.
func (g *Group) handOffKeptLeftovers() {
	g.reaper.Release(g.cmd.Process.Pid)
}

// settle closes the read ends, which also ends a drain that a process outside
// the sweep still feeds, and reports the output path's failures.
func (g *Group) settle() error {
	err := errors.Join(g.pipeErr, closeFiles(g.readers))
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

// groupKillJoin bounds how long StopConfiguredGroup waits for the group to
// be gone once SIGKILL was delivered: the members are dying, and each must
// still be reaped by its parent or by init.
const groupKillJoin = time.Second

// errGroupOutlivedKill is StopConfiguredGroup's error for a group that still
// has members groupKillJoin after SIGKILL was delivered.
var errGroupOutlivedKill = errors.New("procutil: processes in the group outlived SIGKILL")

// StopConfiguredGroup stops the group ConfigureGroup arranged for command and
// returns once it is gone: SIGTERM to every member, then SIGKILL to the
// group once grace has passed with any member still present, then a short
// wait for the killed members to be reaped. The grace runs on group
// emptiness, not on the leader: a leader that exits at once on SIGTERM
// (git, a shell wrapper) can leave a hook or server that still needs its
// moment, or that ignores SIGTERM and must be killed.
//
// It returns os.ErrProcessDone when the group was already gone, nil once
// the group is gone after the signals, and otherwise the error that left
// members running. It suits exec.Cmd.Cancel: the leader is reaped by Wait
// while it blocks. On Windows the stop is the forced tree kill.
func StopConfiguredGroup(command *exec.Cmd, grace time.Duration) error {
	return stopGroup(command, grace, KillConfiguredGroup)
}

func stopGroup(cmd *exec.Cmd, grace time.Duration, kill func(*exec.Cmd) error) error {
	return stopSequence(grace,
		func() error { return terminateConfiguredGroup(cmd) },
		func() error { return kill(cmd) },
		func() bool { return ConfiguredGroupAlive(cmd) })
}

// stopSequence is the stop StopConfiguredGroup and StopGroup share: term,
// a wait of up to grace for the group to empty, kill, and a wait of up to
// groupKillJoin for the killed members to be reaped.
func stopSequence(grace time.Duration, term, kill func() error, alive func() bool) error {
	// A refused SIGTERM still gets the grace and the kill: some members may
	// have taken it, and the kill reports what is left.
	termErr := term()
	if errors.Is(termErr, os.ErrProcessDone) {
		return termErr
	}
	if waitGroupGone(alive, grace) {
		return nil
	}
	if err := kill(); errors.Is(err, os.ErrProcessDone) {
		return nil
	} else if err != nil {
		return errors.Join(termErr, err)
	}
	if waitGroupGone(alive, groupKillJoin) {
		return nil
	}
	return errGroupOutlivedKill
}

// waitGroupGone polls until alive reports no member of the group remains or
// bound ends, and reports whether the group is gone.
func waitGroupGone(alive func() bool, bound time.Duration) bool {
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		if !alive() {
			return true
		}
		select {
		case <-deadline.C:
			return !alive()
		case <-poll.C:
		}
	}
}
