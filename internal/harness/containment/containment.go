// Package containment installs an OS-owned memory boundary around a harness
// process before it starts. Descendants inherit the boundary.
package containment

import (
	"errors"
	"os/exec"
	"time"
)

var ErrUnsupported = errors.New("harness containment is unsupported on this platform")

// Group owns the kernel resource boundary for one launch. Configure must be
// called before cmd.Start. Adopt must be called immediately after Start,
// before the process is waited on. It returns once the process runs the
// configured command inside the boundary, so a process identity recorded
// after Adopt names that command. When Adopt fails, the caller tears the
// process down and waits for it; a process held suspended for adoption has
// already been terminated, so that wait returns. Close is safe to call after
// the process has exited and must be checked by the caller.
type Group interface {
	Configure(*exec.Cmd) error
	Adopt(*exec.Cmd) error
	Close() error
}

// Killer is implemented by containment backends that can terminate every
// process in their owned kernel boundary without addressing a PID. It is
// intentionally optional because RLIMIT fallback has no kernel kill handle.
type Killer interface {
	Kill() error
}

// Waiter reports when an owned kernel boundary has no remaining processes.
// Releasing a Windows Job Object before this is true loses the only reliable
// descendant identity and can strand a browser after its root exits.
type Waiter interface {
	Wait(timeout time.Duration) error
}
