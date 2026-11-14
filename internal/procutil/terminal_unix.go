//go:build !windows

package procutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Terminal is a pty a command gets as its controlling terminal
// (GroupCommand.Terminal) and nothing else: the command's stdin stays the
// null device and its output stays a pipe. Only a program that opens
// /dev/tty reaches it, which is how sudo and ssh ask for a password.
//
// Read returns what the command wrote to its terminal, with the echo of
// what was typed when the program left echo on. Write types into it. Neither
// is part of the command's output.
type Terminal struct {
	master *os.File
	slave  *os.File

	closeOnce sync.Once
	closeErr  error
}

// OpenTerminal allocates a pty. The caller closes it once the command it was
// given to has ended.
func OpenTerminal() (*Terminal, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("procutil: open pty: %w", err)
	}
	// A size, so a program that asks is not told zero by zero.
	sizeErr := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80})
	pollable, err := PollableMaster(master)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("procutil: pty master: %w", err), slave.Close())
	}
	if sizeErr != nil {
		return nil, errors.Join(fmt.Errorf("procutil: size pty: %w", sizeErr), pollable.Close(), slave.Close())
	}
	return &Terminal{master: pollable, slave: slave}, nil
}

func (t *Terminal) Read(p []byte) (int, error)  { return t.master.Read(p) }
func (t *Terminal) Write(p []byte) (int, error) { return t.master.Write(p) }

// Close releases both ends. The terminal keeps its own handle on the far end
// until now, so a Read never fails because no process happens to hold the
// terminal open. On Linux a Read in flight returns; elsewhere it returns once
// no process holds the terminal (PollableMaster). Idempotent.
func (t *Terminal) Close() error {
	t.closeOnce.Do(func() { t.closeErr = errors.Join(t.slave.Close(), t.master.Close()) })
	return t.closeErr
}

// terminalShim runs as /bin/sh before the command, for the two things the
// start of a process cannot do. It ignores SIGHUP, which the command's tree
// inherits: the kernel hangs up a terminal's foreground group when its
// session leader exits, and that would end the processes a command left
// running before Wait could count them. It closes the descriptor the
// terminal was handed over on. Then it becomes the command.
const terminalShim = `trap '' HUP; exec 3<&-; exec "$@"`

// terminalDescriptor is where the terminal arrives in the child: the first
// descriptor after the standard three.
const terminalDescriptor = 3

// command is argv run under the shim. The executable is resolved first, so a
// command that does not exist is still a start failure and not the shim's
// exit status. A relative path names a file in dir, as os/exec resolves one.
func (t *Terminal) command(argv []string, dir string) ([]string, error) {
	name := argv[0]
	if dir != "" && !filepath.IsAbs(name) && strings.ContainsRune(name, filepath.Separator) {
		name = filepath.Join(dir, name)
	}
	target, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	return append([]string{"/bin/sh", "-c", terminalShim, "sh", target}, argv[1:]...), nil
}

// attach makes the terminal the controlling terminal of cmd's new session.
// ConfigureGroup has set the session.
func (t *Terminal) attach(cmd *exec.Cmd) {
	cmd.ExtraFiles = []*os.File{t.slave}
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = terminalDescriptor
}
