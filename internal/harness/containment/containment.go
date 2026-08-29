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
// called before cmd.Start. Adopt is called immediately after Start on
// platforms whose process creation API requires it. Close is safe to call
// after the process has exited and must be checked by the caller.
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
