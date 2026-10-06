//go:build windows

package procutil

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// RunDrained runs cmd with its stdout and stderr copied to the given writers
// (nil discards). Windows pipes take no read deadline, so this is exec's own
// copy bounded by WaitDelay: a pipe still open linger after the process
// exits is closed, and the ErrWaitDelay that reports it is returned, since
// output may have been cut short. exec itself watches ctx, the context cmd
// was created with, until the pipes close.
func RunDrained(_ context.Context, cmd *exec.Cmd, stdout, stderr io.Writer, linger time.Duration) error {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return errors.New("procutil: RunDrained owns the command's stdout and stderr")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = linger
	}
	return cmd.Run()
}
