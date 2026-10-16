package procutil

import (
	"os/exec"
	"sync/atomic"
)

// recordInterrupt wraps cmd.Cancel so a run can tell that its context
// interrupted the process. exec's Wait prefers the process's own exit status
// to the context's error, so a process the context killed before it exited
// reports only "signal: killed" (an exit code on Windows). A cancel that
// found the process already gone (os.ErrProcessDone) interrupted nothing.
func recordInterrupt(cmd *exec.Cmd) *atomic.Bool {
	interrupted := new(atomic.Bool)
	if cancel := cmd.Cancel; cancel != nil {
		cmd.Cancel = func() error {
			err := cancel()
			if err == nil {
				interrupted.Store(true)
			}
			return err
		}
	}
	return interrupted
}
