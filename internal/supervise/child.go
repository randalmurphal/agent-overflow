package supervise

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"agent-overflow/internal/procutil"
)

// child is one running backend and the channel to it.
type child struct {
	cmd      *exec.Cmd
	conn     *Conn
	version  string
	trial    bool
	updateID string

	// exited is closed once the process has been reaped, after exitErr is
	// stored. The exit is a FACT about the child, not a message: two readers
	// need it (the loop, which decides what the exit means, and stopChild,
	// which waits for the stop it asked for), and a one-shot value channel gave
	// whichever read second a wait that never ended.
	exited  chan struct{}
	exitErr error
	// messages carries decoded frames and is closed when the child's write
	// end goes away.
	messages chan Message

	// pipes the parent holds, closed with the child.
	toChild   *os.File
	fromChild *os.File
}

// spawn starts the selected version with the channel inherited.
//
// The activate frame is written BEFORE the child can possibly read it. That is
// deliberate: the child learns whether it is a trial from its first frame, and
// pre-loading the pipe means it never waits on a supervisor that might be
// busy. The frame is tens of bytes into a pipe buffer measured in tens of
// kilobytes, so the write cannot block.
func (s *Supervisor) spawn(selection Selection) (*child, error) {
	binary, err := s.layout.VersionBinary(selection.Version)
	if err != nil {
		return nil, err
	}
	childReads, parentWrites, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("supervise: create supervisor->backend pipe: %w", err)
	}
	parentReads, childWrites, err := os.Pipe()
	if err != nil {
		childReads.Close()
		parentWrites.Close()
		return nil, fmt.Errorf("supervise: create backend->supervisor pipe: %w", err)
	}

	// CommandContext with a context that never cancels, deliberately. The
	// supervisor's own ctx must NOT be the one here: exec would SIGKILL the
	// group the instant it cancelled, and shutdown is precisely when the
	// backend has to be allowed to close provider sessions and flush SQLite.
	// A context is still required, because procutil.ConfigureGroup installs a
	// Cancel func and exec refuses one on a command built without a context.
	// WHEN the group dies stays stopChild's decision alone.
	command := exec.CommandContext(context.Background(), binary, s.config.ChildArgs...)
	command.Env = append(s.childEnv(), EnvChannel+"="+ChannelEnvValue())
	command.Stdin = nil
	command.Stdout = s.config.Stdout
	command.Stderr = s.config.Stderr
	if command.Stdout == nil {
		command.Stdout = os.Stdout
	}
	if command.Stderr == nil {
		command.Stderr = os.Stderr
	}
	// ExtraFiles start at descriptor 3 in the child, which is what
	// ChildReadFD/ChildWriteFD name.
	command.ExtraFiles = []*os.File{childReads, childWrites}
	// The backend's own children (provider CLIs, terminals) belong to its
	// group, and a stop that has to become a kill must take the whole tree.
	procutil.ConfigureGroup(command)

	conn := NewConn(parentReads, parentWrites, parentReads)
	if err := conn.Send(Message{
		Type: MsgActivate, ProtocolVersion: ProtocolVersion,
		Trial: selection.Trial, UpdateID: selection.UpdateID,
		Outcome: string(selection.Outcome), Reason: selection.Reason,
		TargetVersion: selection.Target,
		OwnsDataRoot:  s.config.OwnsDataRoot,
	}); err != nil {
		closeAll(childReads, childWrites, parentReads, parentWrites)
		return nil, err
	}

	if err := command.Start(); err != nil {
		closeAll(childReads, childWrites, parentReads, parentWrites)
		return nil, fmt.Errorf("supervise: start %s: %w", binary, err)
	}
	// The child owns its ends now. Holding them here would keep the read side
	// open after the child dies, and the supervisor would never see EOF.
	closeAll(childReads, childWrites)

	c := &child{
		cmd: command, conn: conn,
		version: selection.Version, trial: selection.Trial, updateID: selection.UpdateID,
		exited:    make(chan struct{}),
		messages:  make(chan Message, messageBuffer),
		toChild:   parentWrites,
		fromChild: parentReads,
	}
	go func() {
		// Written before the close and read only after a receive on it, which
		// is the happens-before edge that makes exitErr safe without a lock.
		c.exitErr = command.Wait()
		close(c.exited)
	}()
	go c.readMessages(s.config.Log)
	s.config.Log("supervise: started version %s (pid %d, trial=%t)",
		selection.Version, command.Process.Pid, selection.Trial)
	return c, nil
}

// messageBuffer is how many frames may sit unread. The protocol is a handful
// of frames per update, so a small buffer means the reader goroutine never
// blocks on a loop that is briefly busy stopping a process.
const messageBuffer = 8

// readMessages decodes the child's frames onto one channel and closes it when
// the child's write end goes away — which is the child exiting, in every
// ordinary case.
func (c *child) readMessages(logf func(string, ...any)) {
	defer close(c.messages)
	for {
		msg, err := c.conn.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				logf("supervise: reading from the backend: %v", err)
			}
			return
		}
		c.messages <- msg
	}
}

// stopChild ends the child gracefully, then by force, then waits.
//
// SIGTERM to the process rather than the group: the backend's own shutdown
// closes provider sessions, flushes SQLite and drains the transport, and
// signalling its children directly would interrupt exactly that. The group
// kill is the fallback for a process that did not finish in time — and there
// it must be the group, or a provider CLI keeps the database open past the
// snapshot.
func (s *Supervisor) stopChild(c *child) {
	if c == nil {
		return
	}
	defer c.conn.Close()
	if c.cmd.Process == nil {
		return
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		s.config.Log("supervise: asking version %s to stop: %v", c.version, err)
	}
	timer := time.NewTimer(s.config.StopTimeout)
	defer timer.Stop()
	select {
	case <-c.exited:
		if c.exitErr != nil {
			s.config.Log("supervise: version %s stopped: %v", c.version, c.exitErr)
		}
		return
	case <-timer.C:
	}
	s.config.Log("supervise: version %s did not stop within %s; killing its process group",
		c.version, s.config.StopTimeout)
	if err := procutil.KillConfiguredGroup(c.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
		s.config.Log("supervise: killing version %s: %v", c.version, err)
	}
	<-c.exited
}

func (s *Supervisor) childEnv() []string {
	if len(s.config.Env) > 0 {
		return s.config.Env
	}
	return os.Environ()
}

func closeAll(files ...*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

// idleTimer is a timer that starts disarmed and can be armed at most once per
// use. It exists so the select in runChild can always name both timers'
// channels: a nil channel blocks forever, which is exactly "not armed".
type idleTimer struct{ timer *time.Timer }

func newIdleTimer() *idleTimer { return &idleTimer{} }

func (t *idleTimer) Reset(d time.Duration) {
	t.Stop()
	t.timer = time.NewTimer(d)
}

func (t *idleTimer) C() <-chan time.Time {
	if t.timer == nil {
		return nil
	}
	return t.timer.C
}

func (t *idleTimer) Stop() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// randomSuffix is the distinguishing half of an update id.
func randomSuffix() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// The id only has to be distinct within one install's history, and the
		// timestamp already carries the ordering. A degraded suffix is far
		// better than an update that cannot start.
		return "000000000000"
	}
	return hex.EncodeToString(buf[:])
}
